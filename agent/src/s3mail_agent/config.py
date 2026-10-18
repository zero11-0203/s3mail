"""Runtime settings, read once from the environment set by infra/agent."""

from __future__ import annotations

import os
from dataclasses import dataclass

# The single owner's identity in the token vault and Memory. The Go agent CLI
# (go/internal/agentcli) sends the same value, so tokens and checkpoints stay
# bound to one actor.
OWNER_USER_ID = "owner"


@dataclass(frozen=True)
class Settings:
    region: str
    jmap_session_url: str
    oauth_provider: str
    oauth_scopes: tuple[str, ...]
    oauth_return_url: str
    memory_id: str
    ask_model: str
    model_base_url: str
    model_key_secret_arn: str
    max_steps: int = 12
    max_tool_calls: int = 24
    max_tokens: int = 200_000

    @classmethod
    def from_env(cls) -> Settings:
        env = os.environ
        return cls(
            # Set by the runtime, not by infra/agent.
            region=env.get("AWS_REGION") or env["AWS_DEFAULT_REGION"],
            jmap_session_url=env["JMAP_SESSION_URL"],
            oauth_provider=env["OAUTH_PROVIDER_NAME"],
            oauth_scopes=tuple(env["OAUTH_SCOPES"].split()),
            oauth_return_url=env["OAUTH_RETURN_URL"],
            memory_id=env["MEMORY_ID"],
            ask_model=env["ASK_MODEL"],
            model_base_url=env["MODEL_BASE_URL"],
            model_key_secret_arn=env["MODEL_KEY_SECRET_ARN"],
            max_steps=int(env.get("ASK_MAX_STEPS", "12")),
            max_tool_calls=int(env.get("ASK_MAX_TOOL_CALLS", "24")),
            max_tokens=int(env.get("ASK_MAX_TOKENS", "200000")),
        )
