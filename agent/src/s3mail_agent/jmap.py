"""A thin JMAP client: guarded reads for every mode, and the few writes the
later phases need (docs/specs/agent.md#tools-and-the-jmap-contract).

The method allowlists are a guard in depth: s3mail's per-method scope checks
remain the boundary, and each mode's token carries only its phase's scopes.

- Reads (READ_METHODS) are retried on 429, 5xx, timeouts and dropped
  connections, which cannot repeat a change.
- Writes go through `write()`, only for the methods the client was built
  with, and are never retried: a failure after sending is reported as
  JmapUnknownResult, because the server may have applied it.
- No call may carry `destroy`, in any mode.
"""

from __future__ import annotations

import asyncio
import random
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any
from urllib.parse import quote

import httpx

CORE = "urn:ietf:params:jmap:core"
MAIL = "urn:ietf:params:jmap:mail"
SUBMISSION = "urn:ietf:params:jmap:submission"
# s3mail's semantic search capability (A4, docs/specs/search.md).
SEMANTIC = "urn:s3mail:params:jmap:semantic"
SEMANTIC_QUERY = "SemanticEmail/query"
USING = (CORE, MAIL)

# Every read method a tool may send.
READ_METHODS = frozenset(
    {
        "Email/query",
        "Email/get",
        "Email/changes",
        "Thread/get",
        "Mailbox/get",
        "Identity/get",
        "EmailSubmission/query",
        "EmailSubmission/get",
        SEMANTIC_QUERY,
    }
)

# Writes by name, each with the arguments it may carry. A mode's client is
# built with the subset its phase and scopes allow (see app.MODE_WRITES).
EMAIL_SET = "Email/set"
MAILBOX_SET = "Mailbox/set"
SUBMISSION_SET = "EmailSubmission/set"
WRITE_ARGUMENTS: Mapping[str, frozenset[str]] = {
    EMAIL_SET: frozenset({"create", "update"}),
    MAILBOX_SET: frozenset({"create"}),
    SUBMISSION_SET: frozenset({"create", "onSuccessUpdateEmail"}),
}
WRITE_METHODS = frozenset(WRITE_ARGUMENTS)
_MUTATIONS = ("create", "update", "destroy", "onSuccessUpdateEmail", "onSuccessDestroyEmail")

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


class JmapMethodError(JmapError):
    """A method-level error response, such as stateMismatch."""

    def __init__(self, type: str) -> None:
        super().__init__(f"method error {type}")
        self.type = type


class JmapUnknownResult(JmapError):
    """A write was sent, but whether the server applied it is unknown."""


class GuardError(JmapError):
    """A call outside the mode's contract was attempted."""


@dataclass(frozen=True)
class Session:
    api_url: str
    download_url: str
    account_id: str
    max_objects_in_get: int
    max_calls_in_request: int
    max_objects_in_set: int
    submission: bool
    semantic: bool = False


def _check_read(name: str, args: Mapping[str, Any]) -> None:
    if name not in READ_METHODS:
        raise GuardError(f"method {name} is not a read")
    for key in _MUTATIONS:
        if key in args:
            raise GuardError(f"argument {key} is not allowed in a read")


def _check_write(name: str, args: Mapping[str, Any], allowed: frozenset[str]) -> None:
    if name not in allowed:
        raise GuardError(f"method {name} is not allowed in this mode")
    permitted = WRITE_ARGUMENTS[name]
    for key in _MUTATIONS:
        if key in args and key not in permitted:
            raise GuardError(f"argument {key} is not allowed")


class JmapClient:
    """JMAP over one bearer token: retried reads, guarded single-shot writes."""

    def __init__(
        self,
        session_url: str,
        token: str,
        *,
        writes: frozenset[str] = frozenset(),
        transport: httpx.AsyncBaseTransport | None = None,
        retries: int = 3,
        backoff: float = 0.5,
    ) -> None:
        if not writes <= WRITE_METHODS:
            raise GuardError("unknown write method")
        self._session_url = session_url
        self._writes = writes
        self._http = httpx.AsyncClient(
            headers={"Authorization": f"Bearer {token}", "Accept": "application/json"},
            transport=transport,
            timeout=httpx.Timeout(20.0),
            follow_redirects=False,
        )
        self._retries = retries
        self._backoff = backoff
        self._session: Session | None = None

    @property
    def writes(self) -> frozenset[str]:
        return self._writes

    async def __aenter__(self) -> JmapClient:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.aclose()

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _send(self, method: str, url: str, *, retry: bool = True, **kwargs: Any) -> Any:
        # Reads are retried; a write gets one attempt, and anything after
        # the request left is an unknown result.
        attempts = self._retries + 1 if retry else 1
        for attempt in range(attempts):
            last = attempt == attempts - 1
            try:
                response = await self._http.request(method, url, **kwargs)
            except httpx.TransportError as exc:
                if not retry:
                    raise JmapUnknownResult(f"JMAP transport failed: {type(exc).__name__}") from exc
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
                if not retry and response.status_code >= 500:
                    raise JmapUnknownResult(f"JMAP returned {response.status_code}")
                raise JmapError(f"JMAP returned {response.status_code}")
            try:
                return response.json()
            except ValueError as exc:
                if not retry:
                    raise JmapUnknownResult("JMAP returned invalid JSON") from exc
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
            account_caps = data["accounts"][account_id].get("accountCapabilities", {})
            self._session = Session(
                api_url=data["apiUrl"],
                download_url=data.get("downloadUrl", ""),
                account_id=account_id,
                max_objects_in_get=int(core.get("maxObjectsInGet", 50)),
                max_calls_in_request=int(core.get("maxCallsInRequest", 16)),
                max_objects_in_set=int(core.get("maxObjectsInSet", 16)),
                submission=SUBMISSION in caps and SUBMISSION in account_caps,
                semantic=SEMANTIC in caps and SEMANTIC in account_caps,
            )
        return self._session

    async def _post(
        self, calls: list[tuple[str, dict[str, Any]]], *, retry: bool
    ) -> dict[str, dict[str, Any]]:
        session = await self.session()
        if len(calls) > session.max_calls_in_request:
            raise JmapError("too many calls in one request")
        needs_submission = any(n.split("/")[0] in ("Identity", "EmailSubmission") for n, _ in calls)
        using = [*USING, SUBMISSION] if needs_submission else list(USING)
        if any(n == SEMANTIC_QUERY for n, _ in calls):
            using.append(SEMANTIC)
        method_calls = [
            [name, {"accountId": session.account_id, **args}, f"c{i}"]
            for i, (name, args) in enumerate(calls)
        ]
        data = await self._send(
            "POST",
            session.api_url,
            retry=retry,
            json={"using": using, "methodCalls": method_calls},
        )
        results: dict[str, dict[str, Any]] = {}
        for name, args, call_id in data.get("methodResponses", []):
            if name == "error":
                raise JmapMethodError(str(args.get("type", "unknown")))
            # onSuccessUpdateEmail answers as an implicit Email/set under
            # the same call id; keep the submission's own response.
            if call_id in results:
                results[f"{call_id}:{name}"] = args
                continue
            results[call_id] = args
        return results

    async def call(self, calls: list[tuple[str, dict[str, Any]]]) -> dict[str, dict[str, Any]]:
        """Send one batched read; return each call's arguments by call id.

        Call ids are c0, c1, ... so later calls can use result references.
        A method-level error raises JmapMethodError.
        """
        for name, args in calls:
            _check_read(name, args)
        return await self._post(calls, retry=True)

    async def write(self, name: str, args: dict[str, Any]) -> dict[str, dict[str, Any]]:
        """Send one write, once. Raises JmapUnknownResult when the outcome is
        unknown; the caller reports it and never retries."""
        _check_write(name, args, self._writes)
        return await self._post([(name, args)], retry=False)

    async def download(self, blob_id: str, name: str, max_bytes: int) -> bytes:
        """Download a blob of this account, refusing more than max_bytes."""
        session = await self.session()
        if not session.download_url:
            raise JmapError("server has no download URL")
        url = (
            session.download_url.replace("{accountId}", quote(session.account_id, safe=""))
            .replace("{blobId}", quote(blob_id, safe=""))
            .replace("{name}", quote(name or "attachment", safe=""))
            .replace("{type}", "application/octet-stream")
        )
        for attempt in range(self._retries + 1):
            try:
                async with self._http.stream("GET", url) as response:
                    if response.status_code in (401, 403):
                        raise JmapAuthError(response.status_code)
                    if response.status_code in _RETRY_STATUSES and attempt < self._retries:
                        await self._pause(attempt)
                        continue
                    if response.status_code != 200:
                        raise JmapError(f"download returned {response.status_code}")
                    chunks: list[bytes] = []
                    size = 0
                    async for chunk in response.aiter_bytes():
                        size += len(chunk)
                        if size > max_bytes:
                            raise JmapError("download exceeds the size cap")
                        chunks.append(chunk)
                    return b"".join(chunks)
            except httpx.TransportError as exc:
                if attempt == self._retries:
                    raise JmapError(f"download failed: {type(exc).__name__}") from exc
                await self._pause(attempt)
        raise AssertionError("unreachable")


def ref(call_id: str, name: str, path: str) -> dict[str, str]:
    """A JMAP result reference to an earlier call in the same request."""
    return {"resultOf": call_id, "name": name, "path": path}
