"""AgentCore Runtime entrypoint: serves /invocations and /ping on port 8080.

Payloads, by mode (docs/specs/agent.md#architecture):

    login      {"state": "<random>", "force": bool}       no scope
    ask        {"question": "..."}                        mail.read (A0)
    triage     {"event": {"eventId", "emailId"}}          mail.read (A1)
    digest     {"deliver": "return"|"mail", "peek": bool} mail.read; mail by mail.write (A2)
    draft      {"email_id", "instructions", "reply_all"}  mail.write (A2)
    remember   {"text": "..."}; preferences {}; forget {"ids": [...]|null}  (A2)
    act        {"question": "..."}                        mail.write, +mail.submit to send (A3)
    approve    {"hash": "<sha256>", "approve": bool}      as act (A3)

The router is code. A mode whose scope is not configured is refused before
any JMAP or model call (scope_not_granted), on top of s3mail's own per-method
scope checks, and each mode's JMAP client carries only its own write methods.
The runtime session id is the LangGraph thread id for ask, act and approve,
so one CLI conversation resumes from its last checkpoint on any microVM;
triage, digest and draft runs keep no checkpoint.

Logs are content-free (docs/operations/monitoring.md): mode, status, error
code, counts and duration, never questions, answers, mail or exception text.
"""

from __future__ import annotations

import json
import logging
import re
import time
from collections.abc import Callable
from typing import Any

from bedrock_agentcore.runtime import BedrockAgentCoreApp, RequestContext
from langchain_core.language_models import BaseChatModel
from langgraph.checkpoint.base import BaseCheckpointSaver

from .config import OWNER_USER_ID, Settings
from .digest import run_digest
from .drafts import DraftError, run_draft
from .graph import Limits, build_graph, run_approve, run_ask
from .identity import AuthorizationRequired, TokenSource, vault_token_source
from .jmap import (
    EMAIL_SET,
    MAILBOX_SET,
    SUBMISSION_SET,
    JmapAuthError,
    JmapClient,
    JmapError,
    JmapUnknownResult,
)
from .metering import BudgetExceeded
from .models import chat_model, read_api_key
from .preferences import Preferences, TooManyPreferences, clean
from .store import AgentTable
from .tools import read_tools
from .triage import TriageFailed, run_triage

log = logging.getLogger("s3mail_agent")

# Runtime session ids are at least 33 characters.
MIN_SESSION_ID = 33
# agent login's CSRF state: 128 random bits or more, URL-safe.
LOGIN_STATE = re.compile(r"[A-Za-z0-9_-]{22,128}")
APPROVAL_HASH = re.compile(r"[0-9a-f]{64}")

READ, WRITE, SUBMIT = "mail.read", "mail.write", "mail.submit"
# The scopes each mode needs.
MODE_SCOPES: dict[str, frozenset[str]] = {
    "login": frozenset(),
    "ask": frozenset({READ}),
    "triage": frozenset({READ}),
    "digest": frozenset({READ}),
    "draft": frozenset({READ, WRITE}),
    "remember": frozenset(),
    "preferences": frozenset(),
    "forget": frozenset(),
    "act": frozenset({READ, WRITE}),
    "approve": frozenset({READ, WRITE}),
}
MODES = frozenset(MODE_SCOPES)

# Which model serves each mode: questions, drafts and actions get the main
# model; triage and the digest the smaller one.
ModelFactory = Callable[[str], BaseChatModel]


class Handler:
    def __init__(
        self,
        settings: Settings,
        *,
        token_source: TokenSource,
        model_factory: ModelFactory,
        checkpointer: BaseCheckpointSaver[Any] | None,
        jmap_factory: Callable[[str, frozenset[str]], JmapClient] | None = None,
        table: AgentTable | None = None,
        preferences: Preferences | None = None,
    ) -> None:
        self.settings = settings
        self.token_source = token_source
        self.model_factory = model_factory
        self.checkpointer = checkpointer
        self.jmap_factory = jmap_factory or (
            lambda token, writes: JmapClient(settings.jmap_session_url, token, writes=writes)
        )
        self.table = table
        self.preferences = preferences
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
                # Where the vault redirects: agent login listens there.
                result["return_url"] = self.settings.oauth_return_url
        except JmapAuthError as exc:
            result = {"status": "error", "error": "authorization_failed", "http_status": exc.status}
        except JmapUnknownResult:
            result = {"status": "error", "error": "unknown_result"}
        except JmapError:
            result = {"status": "error", "error": "jmap_unavailable"}
        except BudgetExceeded:
            result = {"status": "error", "error": "budget_exceeded"}
        except TriageFailed:
            result = {"status": "error", "error": "triage_failed"}
        except DraftError as exc:
            result = {"status": "error", "error": exc.code}
        except Exception as exc:
            log.error(json.dumps({"event": "invocation_failed", "exception": type(exc).__name__}))
            result = {"status": "error", "error": "internal"}
        record: dict[str, Any] = {
            "event": "invocation",
            "mode": mode if mode in MODES else "unknown",
            "status": result["status"],
            "duration_ms": int((time.monotonic() - started) * 1000),
        }
        for key in (
            "error",
            "steps",
            "tool_calls",
            "model_calls",
            "tokens",
            "evidence",
            "triage",
            "emails",
            "needs_reply",
            "waiting",
        ):
            if key in result:
                record[key] = result[key]
        if "citations" in result:
            record["citations"] = len(result["citations"])
        if isinstance(result.get("outcome"), dict):
            record["outcome"] = result["outcome"].get("outcome")
        if isinstance(result.get("approval"), dict):
            record["approval"] = result["approval"].get("kind")
        log.info(json.dumps(record))
        return result

    def _writes(self, mode: str, payload: dict[str, Any]) -> frozenset[str]:
        granted = self.settings.granted
        if mode == "digest" and payload.get("deliver") == "mail":
            return frozenset({EMAIL_SET, MAILBOX_SET})
        if mode == "draft":
            return frozenset({EMAIL_SET})
        if mode == "approve":
            return frozenset({EMAIL_SET} | ({SUBMISSION_SET} if SUBMIT in granted else set()))
        return frozenset()

    def _propose(self) -> frozenset[str]:
        granted = self.settings.granted
        tools = {"propose_update"} if WRITE in granted else set()
        if WRITE in granted and SUBMIT in granted:
            tools.add("propose_send")
        return frozenset(tools)

    async def _dispatch(
        self, mode: Any, payload: dict[str, Any], session_id: str | None
    ) -> dict[str, Any]:
        if mode not in MODES:
            return {"status": "error", "error": "unsupported_mode"}
        needed = set(MODE_SCOPES[mode])
        if mode == "digest" and payload.get("deliver") == "mail":
            needed.add(WRITE)
        if not needed <= self.settings.granted:
            return {"status": "error", "error": "scope_not_granted"}
        if mode == "login":
            state = payload.get("state")
            if not isinstance(state, str) or not LOGIN_STATE.fullmatch(state):
                return {"status": "error", "error": "bad_request"}
            if payload.get("force") is True:
                # Consent again, such as after a scope was granted.
                await self.token_source(state, force=True)
            else:
                await self.token_source(state)
            return {"status": "ok"}
        if mode in ("remember", "preferences", "forget"):
            return await self._preferences(mode, payload)
        if mode in ("triage", "digest") and (self.table is None or not self.settings.triage_model):
            return {"status": "error", "error": "unsupported_mode"}
        if mode in ("ask", "act", "approve") and (
            not session_id or len(session_id) < MIN_SESSION_ID
        ):
            return {"status": "error", "error": "bad_request"}

        token = await self.token_source(None)
        async with self.jmap_factory(token, self._writes(mode, payload)) as jmap:
            session = await jmap.session()
            if mode == "triage":
                assert self.table is not None
                event = payload.get("event")
                if not isinstance(event, dict):
                    return {"status": "error", "error": "bad_request"}
                return await run_triage(jmap, self.model_factory("triage"), self.table, event)
            if mode == "digest":
                assert self.table is not None
                deliver = payload.get("deliver", "return")
                if deliver not in ("return", "mail"):
                    return {"status": "error", "error": "bad_request"}
                return await run_digest(
                    jmap,
                    self.model_factory("triage"),
                    self.table,
                    deliver=deliver,
                    peek=payload.get("peek") is True,
                )
            if mode == "draft":
                email_id, instructions = payload.get("email_id"), payload.get("instructions", "")
                if not isinstance(email_id, str) or not isinstance(instructions, str):
                    return {"status": "error", "error": "bad_request"}
                prefs = await self.preferences.all(OWNER_USER_ID) if self.preferences else []
                return await run_draft(
                    jmap,
                    self.model_factory("ask"),
                    email_id=email_id,
                    instructions=instructions,
                    preferences=prefs,
                    reply_all=payload.get("reply_all") is True,
                )

            assert session_id is not None
            graph = build_graph(
                self.model_factory("ask"),
                jmap,
                limits=self.limits,
                checkpointer=self.checkpointer,
                read_tools=read_tools(session, self.settings.features),
                propose=self._propose(),
                table=self.table,
            )
            if mode == "approve":
                digest, approve = payload.get("hash"), payload.get("approve")
                if not (isinstance(digest, str) and APPROVAL_HASH.fullmatch(digest)):
                    return {"status": "error", "error": "bad_request"}
                if not isinstance(approve, bool) or self.table is None:
                    return {"status": "error", "error": "bad_request"}
                return await run_approve(
                    graph,
                    thread_id=session_id,
                    actor_id=OWNER_USER_ID,
                    approval_hash=digest,
                    approve=approve,
                    limits=self.limits,
                )
            question = payload.get("question")
            if not isinstance(question, str):
                return {"status": "error", "error": "bad_request"}
            if mode == "act" and self.table is None:
                return {"status": "error", "error": "unsupported_mode"}
            return await run_ask(
                graph,
                question,
                thread_id=session_id,
                actor_id=OWNER_USER_ID,
                limits=self.limits,
                mode=mode,
            )

    async def _preferences(self, mode: str, payload: dict[str, Any]) -> dict[str, Any]:
        prefs = self.preferences
        if prefs is None:
            return {"status": "error", "error": "unsupported_mode"}
        if mode == "remember":
            text = payload.get("text")
            value = clean(text) if isinstance(text, str) else None
            if value is None:
                return {"status": "error", "error": "bad_request"}
            try:
                pref = await prefs.add(OWNER_USER_ID, value)
            except TooManyPreferences:
                return {"status": "error", "error": "too_many_preferences"}
            return {"status": "ok", "preference": {"id": pref.id, "text": pref.text}}
        if mode == "forget":
            ids = payload.get("ids")
            if ids is not None and not (
                isinstance(ids, list) and all(isinstance(i, str) for i in ids)
            ):
                return {"status": "error", "error": "bad_request"}
            return {"status": "ok", "forgotten": await prefs.forget(OWNER_USER_ID, ids)}
        items = await prefs.all(OWNER_USER_ID)
        return {"status": "ok", "preferences": [{"id": p.id, "text": p.text} for p in items]}


def _runtime_handler() -> Handler:
    from langgraph_checkpoint_aws import AgentCoreMemorySaver

    from .preferences import AgentCorePreferences
    from .store import DynamoTable

    settings = Settings.from_env()
    keys: list[str] = []

    def model(kind: str) -> BaseChatModel:
        # Read the key on first use, so the runtime starts before the owner
        # has set it; one microVM serves one session, so caching is enough.
        if not keys:
            keys.append(read_api_key(settings.model_key_secret_arn, settings.region))
        name = settings.triage_model if kind == "triage" else settings.ask_model
        return chat_model(name, keys[0], settings.model_base_url)

    return Handler(
        settings,
        token_source=vault_token_source(settings),
        model_factory=model,
        checkpointer=AgentCoreMemorySaver(settings.memory_id, region_name=settings.region),
        table=DynamoTable(settings.agent_table, settings.region) if settings.agent_table else None,
        preferences=AgentCorePreferences(settings.memory_id, settings.region),
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
