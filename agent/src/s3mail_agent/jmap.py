"""A thin JMAP client limited to the read methods A0 needs.

See docs/specs/agent.md#tools-and-the-jmap-contract.

The method allowlist is a guard in depth: s3mail's per-method scope checks
remain the boundary, and the A0 token carries only s3mail/mail.read.
"""

from __future__ import annotations

import asyncio
import random
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

import httpx

CORE = "urn:ietf:params:jmap:core"
MAIL = "urn:ietf:params:jmap:mail"
USING = (CORE, MAIL)

# Every method a tool may send. No /set, /import, /copy or submission method
# appears here, so no tool can create, change or destroy anything.
READ_METHODS = frozenset({"Email/query", "Email/get", "Thread/get", "Mailbox/get"})

# Caps applied to every body fetch.
MAX_BODY_VALUE_BYTES = 32 * 1024

_RETRY_STATUSES = frozenset({429, 500, 502, 503, 504})


class JmapError(Exception):
    """A JMAP request or method failed."""


class JmapAuthError(JmapError):
    """401 or 403: the run ends without retry."""

    def __init__(self, status: int) -> None:
        super().__init__(f"JMAP returned {status}")
        self.status = status


class GuardError(JmapError):
    """A call outside the read-only contract was attempted."""


@dataclass(frozen=True)
class Session:
    api_url: str
    account_id: str
    max_objects_in_get: int
    max_calls_in_request: int


def _check_call(name: str, args: Mapping[str, Any]) -> None:
    if name not in READ_METHODS:
        raise GuardError(f"method {name} is not allowed")
    for key in ("create", "update", "destroy"):
        if key in args:
            raise GuardError(f"argument {key} is not allowed")


class JmapClient:
    """Read-only JMAP over one bearer token, with jittered retries for reads."""

    def __init__(
        self,
        session_url: str,
        token: str,
        *,
        transport: httpx.AsyncBaseTransport | None = None,
        retries: int = 3,
        backoff: float = 0.5,
    ) -> None:
        self._session_url = session_url
        self._http = httpx.AsyncClient(
            headers={"Authorization": f"Bearer {token}", "Accept": "application/json"},
            transport=transport,
            timeout=httpx.Timeout(20.0),
            follow_redirects=False,
        )
        self._retries = retries
        self._backoff = backoff
        self._session: Session | None = None

    async def __aenter__(self) -> JmapClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _send(self, method: str, url: str, **kwargs: Any) -> Any:
        # Every request is a guarded read, so retrying after a timeout or a
        # dropped connection cannot repeat a change.
        for attempt in range(self._retries + 1):
            last = attempt == self._retries
            try:
                response = await self._http.request(method, url, **kwargs)
            except httpx.TransportError as exc:
                if last:
                    raise JmapError(f"JMAP transport failed: {type(exc).__name__}") from exc
                await self._pause(attempt)
                continue
            if response.status_code in (401, 403):
                raise JmapAuthError(response.status_code)
            if response.status_code in _RETRY_STATUSES and not last:
                await self._pause(attempt)
                continue
            if response.status_code != 200:
                raise JmapError(f"JMAP returned {response.status_code}")
            try:
                return response.json()
            except ValueError as exc:
                raise JmapError("JMAP returned invalid JSON") from exc
        raise AssertionError("unreachable")

    async def _pause(self, attempt: int) -> None:
        await asyncio.sleep(self._backoff * (2**attempt) * random.uniform(0.5, 1.5))  # noqa: S311

    async def session(self) -> Session:
        """Discover the session; refuse to start without the mail capability."""
        if self._session is None:
            data = await self._send("GET", self._session_url)
            caps = data.get("capabilities", {})
            if CORE not in caps or MAIL not in caps:
                raise JmapError("server lacks the JMAP core or mail capability")
            account_id = data.get("primaryAccounts", {}).get(MAIL)
            if not account_id or MAIL not in data["accounts"][account_id].get(
                "accountCapabilities", {}
            ):
                raise JmapError("no primary mail account")
            core = caps[CORE]
            self._session = Session(
                api_url=data["apiUrl"],
                account_id=account_id,
                max_objects_in_get=int(core.get("maxObjectsInGet", 50)),
                max_calls_in_request=int(core.get("maxCallsInRequest", 16)),
            )
        return self._session

    async def call(self, calls: list[tuple[str, dict[str, Any]]]) -> dict[str, dict[str, Any]]:
        """Send one batched request; return each call's arguments by call id.

        Call ids are c0, c1, ... so later calls can use result references.
        A method-level error raises JmapError.
        """
        for name, args in calls:
            _check_call(name, args)
        session = await self.session()
        if len(calls) > session.max_calls_in_request:
            raise JmapError("too many calls in one request")
        method_calls = [
            [name, {"accountId": session.account_id, **args}, f"c{i}"]
            for i, (name, args) in enumerate(calls)
        ]
        data = await self._send(
            "POST", session.api_url, json={"using": list(USING), "methodCalls": method_calls}
        )
        results: dict[str, dict[str, Any]] = {}
        for name, args, call_id in data.get("methodResponses", []):
            if name == "error":
                raise JmapError(f"method error {args.get('type', 'unknown')}")
            results[call_id] = args
        return results


def ref(call_id: str, name: str, path: str) -> dict[str, str]:
    """A JMAP result reference to an earlier call in the same request."""
    return {"resultOf": call_id, "name": name, "path": path}
