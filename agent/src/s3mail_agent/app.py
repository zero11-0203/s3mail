"""AgentCore Runtime entrypoint: serves /invocations and /ping on port 8080.

Payloads: {"mode": "login"} or {"mode": "ask", "question": "..."}. The
runtime session id is the LangGraph thread id, so one CLI conversation
resumes from its last checkpoint on any microVM.

Logs are content-free (docs/operations/monitoring.md): mode, status, error
code, counts and duration, never questions, answers, mail or exception text.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Callable
from typing import Any

from bedrock_agentcore.runtime import BedrockAgentCoreApp, RequestContext
from langchain_core.language_models import BaseChatModel
from langgraph.checkpoint.base import BaseCheckpointSaver

from .config import OWNER_USER_ID, Settings
from .graph import Limits, build_graph, run_ask
from .identity import AuthorizationRequired, TokenSource, vault_token_source
from .jmap import JmapAuthError, JmapClient, JmapError
from .models import chat_model, read_api_key

log = logging.getLogger("s3mail_agent")

# Runtime session ids are at least 33 characters.
MIN_SESSION_ID = 33


class Handler:
    def __init__(
        self,
        settings: Settings,
        *,
        token_source: TokenSource,
        model_factory: Callable[[], BaseChatModel],
        checkpointer: BaseCheckpointSaver[Any] | None,
        jmap_factory: Callable[[str], JmapClient] | None = None,
    ) -> None:
        self.settings = settings
        self.token_source = token_source
        self.model_factory = model_factory
        self.checkpointer = checkpointer
        self.jmap_factory = jmap_factory or (
            lambda token: JmapClient(settings.jmap_session_url, token)
        )
        self.limits = Limits(settings.max_steps, settings.max_tool_calls, settings.max_tokens)

    async def __call__(self, payload: dict[str, Any], session_id: str | None) -> dict[str, Any]:
        started = time.monotonic()
        mode = payload.get("mode") if isinstance(payload, dict) else None
        try:
            result = await self._dispatch(mode, payload, session_id)
        except AuthorizationRequired as exc:
            result = {"status": "authorization_required"}
            if mode == "login" and exc.url:
                result["authorization_url"] = exc.url
        except JmapAuthError as exc:
            result = {"status": "error", "error": "authorization_failed", "http_status": exc.status}
        except JmapError:
            result = {"status": "error", "error": "jmap_unavailable"}
        except Exception as exc:
            log.error(json.dumps({"event": "invocation_failed", "exception": type(exc).__name__}))
            result = {"status": "error", "error": "internal"}
        record = {
            "event": "invocation",
            "mode": mode if mode in ("login", "ask") else "unknown",
            "status": result["status"],
            "duration_ms": int((time.monotonic() - started) * 1000),
        }
        for key in ("error", "steps", "tool_calls", "tokens"):
            if key in result:
                record[key] = result[key]
        if "citations" in result:
            record["citations"] = len(result["citations"])
        log.info(json.dumps(record))
        return result

    async def _dispatch(
        self, mode: Any, payload: dict[str, Any], session_id: str | None
    ) -> dict[str, Any]:
        if mode == "login":
            await self.token_source()
            return {"status": "ok"}
        if mode != "ask":
            return {"status": "error", "error": "unsupported_mode"}
        if not session_id or len(session_id) < MIN_SESSION_ID:
            return {"status": "error", "error": "bad_request"}
        question = payload.get("question")
        if not isinstance(question, str):
            return {"status": "error", "error": "bad_request"}
        token = await self.token_source()
        async with self.jmap_factory(token) as jmap:
            await jmap.session()
            graph = build_graph(
                self.model_factory(), jmap, limits=self.limits, checkpointer=self.checkpointer
            )
            return await run_ask(
                graph, question, thread_id=session_id, actor_id=OWNER_USER_ID, limits=self.limits
            )


def _runtime_handler() -> Handler:
    from langgraph_checkpoint_aws import AgentCoreMemorySaver

    settings = Settings.from_env()
    keys: list[str] = []

    def model() -> BaseChatModel:
        # Read the key on first use, so the runtime starts before the owner
        # has set it; one microVM serves one session, so caching is enough.
        if not keys:
            keys.append(read_api_key(settings.model_key_secret_arn, settings.region))
        return chat_model(settings.ask_model, keys[0], settings.model_base_url)

    return Handler(
        settings,
        token_source=vault_token_source(settings),
        model_factory=model,
        checkpointer=AgentCoreMemorySaver(settings.memory_id, region_name=settings.region),
    )


app = BedrockAgentCoreApp()
_handler: Handler | None = None


@app.entrypoint
async def invoke(payload: dict[str, Any], context: RequestContext) -> dict[str, Any]:
    if _handler is None:
        return {"status": "error", "error": "internal"}
    return await _handler(payload, context.session_id)


def main() -> None:
    global _handler
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    # Fail at boot, not on the first invocation, when configuration is missing.
    _handler = _runtime_handler()
    app.run()


if __name__ == "__main__":
    main()
