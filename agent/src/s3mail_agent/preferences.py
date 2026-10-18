"""Owner preferences in AgentCore Memory long-term records (A2,
docs/specs/agent.md#state-and-memory).

Preferences are written only by the owner's own `agent remember` request,
verbatim; nothing is ever extracted from mail or by a model, so an email
cannot plant a preference. `agent forget` deletes them. Drafts read them as
owner instructions, outside the untrusted-data block.
"""

from __future__ import annotations

import asyncio
import re
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Protocol

MAX_PREFERENCE_CHARS = 500
MAX_PREFERENCES = 50
CONTROL = re.compile(r"[\x00-\x08\x0b-\x1f\x7f-\x9f‪-‮⁦-⁩]")


def namespace(actor_id: str) -> str:
    return f"/preferences/{actor_id}"


@dataclass(frozen=True)
class Preference:
    id: str
    text: str
    created_at: str


class TooManyPreferences(Exception):
    pass


def clean(text: str) -> str | None:
    """One preference as plain text, or None when empty or too long."""
    value = " ".join(CONTROL.sub(" ", text).split())
    if not value or len(value) > MAX_PREFERENCE_CHARS:
        return None
    return value


class Preferences(Protocol):
    async def all(self, actor_id: str) -> list[Preference]: ...

    async def add(self, actor_id: str, text: str) -> Preference: ...

    async def forget(self, actor_id: str, ids: list[str] | None) -> int:
        """Delete the given preferences, or all of them; the number deleted."""


class MemoryPreferences:
    """An in-process Preferences for tests and evaluations."""

    def __init__(self) -> None:
        self.items: dict[str, list[Preference]] = {}

    async def all(self, actor_id: str) -> list[Preference]:
        return list(self.items.get(actor_id, []))

    async def add(self, actor_id: str, text: str) -> Preference:
        items = self.items.setdefault(actor_id, [])
        if len(items) >= MAX_PREFERENCES:
            raise TooManyPreferences
        pref = Preference(f"P{len(items) + 1}", text, datetime.now(UTC).isoformat())
        items.append(pref)
        return pref

    async def forget(self, actor_id: str, ids: list[str] | None) -> int:
        items = self.items.get(actor_id, [])
        keep = [p for p in items if ids is not None and p.id not in ids]
        self.items[actor_id] = keep
        return len(items) - len(keep)


class AgentCorePreferences:
    """Preferences as long-term records of the agent's Memory, without an
    extraction strategy."""

    def __init__(self, memory_id: str, region: str, client: Any = None) -> None:
        if client is None:
            import boto3

            client = boto3.client("bedrock-agentcore", region_name=region)
        self._client = client
        self._memory = memory_id

    async def all(self, actor_id: str) -> list[Preference]:
        out: list[Preference] = []
        kwargs: dict[str, Any] = {
            "memoryId": self._memory,
            "namespace": namespace(actor_id),
            "maxResults": 100,
        }
        while True:
            page = await asyncio.to_thread(self._client.list_memory_records, **kwargs)
            for r in page.get("memoryRecordSummaries", []):
                created = r.get("createdAt")
                out.append(
                    Preference(
                        r["memoryRecordId"],
                        r.get("content", {}).get("text", ""),
                        created.isoformat() if hasattr(created, "isoformat") else str(created),
                    )
                )
            if not page.get("nextToken"):
                return sorted(out, key=lambda p: p.created_at)
            kwargs["nextToken"] = page["nextToken"]

    async def add(self, actor_id: str, text: str) -> Preference:
        if len(await self.all(actor_id)) >= MAX_PREFERENCES:
            raise TooManyPreferences
        now = datetime.now(UTC)
        out = await asyncio.to_thread(
            self._client.batch_create_memory_records,
            memoryId=self._memory,
            records=[
                {
                    "requestIdentifier": uuid.uuid4().hex,
                    "namespaces": [namespace(actor_id)],
                    "content": {"text": text},
                    "timestamp": now,
                }
            ],
            clientToken=uuid.uuid4().hex,
        )
        done = out.get("successfulRecords") or []
        if not done:
            raise RuntimeError("preference not stored")
        return Preference(done[0]["memoryRecordId"], text, now.isoformat())

    async def forget(self, actor_id: str, ids: list[str] | None) -> int:
        current = await self.all(actor_id)
        targets = [p.id for p in current if ids is None or p.id in ids]
        deleted = 0
        for start in range(0, len(targets), 100):
            out = await asyncio.to_thread(
                self._client.batch_delete_memory_records,
                memoryId=self._memory,
                records=[
                    {"memoryRecordId": i, "namespace": namespace(actor_id)}
                    for i in targets[start : start + 100]
                ],
            )
            deleted += len(out.get("successfulRecords") or [])
        return deleted
