"""The owner's JMAP token from the AgentCore Identity token vault.

The vault holds the refresh token for the agent's Cognito client and returns
short-lived access tokens to this runtime's workload identity. When it has
no usable token it returns a consent URL; instead of polling for minutes
inside an invocation, the run stops with authorization_required and the
CLI's `agent login` completes consent (docs/specs/agent.md#identity-and-authorization).
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable

from bedrock_agentcore.runtime.context import BedrockAgentCoreContext
from bedrock_agentcore.services.identity import IdentityClient, TokenPoller

from .config import Settings


class AuthorizationRequired(Exception):
    def __init__(self, url: str | None) -> None:
        super().__init__("authorization_required")
        self.url = url


class _ConsentPending(Exception):
    pass


class _NoWait(TokenPoller):
    async def poll_for_token(self) -> str:
        raise _ConsentPending


TokenSource = Callable[[], Awaitable[str]]


def vault_token_source(settings: Settings, client: IdentityClient | None = None) -> TokenSource:
    identity = client or IdentityClient(settings.region)

    async def token() -> str:
        workload_token = BedrockAgentCoreContext.get_workload_access_token()
        if not workload_token:
            raise RuntimeError("no workload access token: not running on AgentCore Runtime")
        urls: list[str] = []
        try:
            return await identity.get_token(
                provider_name=settings.oauth_provider,
                scopes=list(settings.oauth_scopes),
                agent_identity_token=workload_token,
                auth_flow="USER_FEDERATION",
                callback_url=settings.oauth_return_url,
                on_auth_url=urls.append,
                token_poller=_NoWait(),
            )
        except _ConsentPending:
            raise AuthorizationRequired(urls[0] if urls else None) from None

    return token
