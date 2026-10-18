"""The agent table (docs/specs/agent.md#state-and-memory): derived, expiring
data only. s3mail stays the source of truth.

Items (key pk, sk):

    TRIAGE#<emailId>  -     one triage row per email; the first event wins
    CURSOR            digest  the Email state the last digest covered
    SEND#<hash>       -     the send-once marker of an approved send (A3)

Triage rows that need a reply carry the sparse `reply` index keys, so the
digest can list threads waiting on the owner without a scan. Every item but
the cursor expires after 30 days.
"""

from __future__ import annotations

import asyncio
import time
from collections.abc import Iterable
from dataclasses import asdict, dataclass
from typing import Any, Protocol

TTL_SECONDS = 30 * 24 * 3600
REPLY_INDEX = "reply"
REPLY_PK = "WAITING"


@dataclass(frozen=True)
class TriageRow:
    email_id: str
    event_id: str
    thread_id: str
    received_at: str
    category: str
    priority: str
    needs_reply: bool
    summary: str


@dataclass(frozen=True)
class Cursor:
    state: str
    at: str


@dataclass(frozen=True)
class SendMarker:
    key: str
    email_id: str
    attempted_at: str


class AgentTable(Protocol):
    async def put_triage(self, row: TriageRow) -> bool:
        """Store the row unless the email already has one; True if stored."""

    async def has_triage(self, email_id: str) -> bool: ...

    async def get_triage(self, email_ids: Iterable[str]) -> dict[str, TriageRow]: ...

    async def waiting_replies(self, since: str) -> list[TriageRow]:
        """Rows that need a reply, received at or after since (ISO 8601)."""

    async def get_cursor(self) -> Cursor | None: ...

    async def set_cursor(self, cursor: Cursor) -> None: ...

    async def put_send_marker(self, marker: SendMarker) -> bool:
        """Store the marker unless it exists; True if stored."""

    async def get_send_marker(self, key: str) -> SendMarker | None: ...


def _expires() -> int:
    return int(time.time()) + TTL_SECONDS


class MemoryTable:
    """An in-process AgentTable for tests and evaluations."""

    def __init__(self) -> None:
        self.triage: dict[str, TriageRow] = {}
        self.cursor: Cursor | None = None
        self.markers: dict[str, SendMarker] = {}
        self.puts = 0

    async def put_triage(self, row: TriageRow) -> bool:
        self.puts += 1
        if row.email_id in self.triage:
            return False
        self.triage[row.email_id] = row
        return True

    async def has_triage(self, email_id: str) -> bool:
        return email_id in self.triage

    async def get_triage(self, email_ids: Iterable[str]) -> dict[str, TriageRow]:
        return {i: self.triage[i] for i in email_ids if i in self.triage}

    async def waiting_replies(self, since: str) -> list[TriageRow]:
        rows = [r for r in self.triage.values() if r.needs_reply and r.received_at >= since]
        return sorted(rows, key=lambda r: (r.received_at, r.email_id))

    async def get_cursor(self) -> Cursor | None:
        return self.cursor

    async def set_cursor(self, cursor: Cursor) -> None:
        self.cursor = cursor

    async def put_send_marker(self, marker: SendMarker) -> bool:
        if marker.key in self.markers:
            return False
        self.markers[marker.key] = marker
        return True

    async def get_send_marker(self, key: str) -> SendMarker | None:
        return self.markers.get(key)


class DynamoTable:
    """The deployed AgentTable over boto3, run off the event loop."""

    def __init__(self, table_name: str, region: str, client: Any = None) -> None:
        if client is None:
            import boto3

            client = boto3.client("dynamodb", region_name=region)
        self._db = client
        self._table = table_name

    async def _run(self, op: str, **kwargs: Any) -> Any:
        return await asyncio.to_thread(getattr(self._db, op), TableName=self._table, **kwargs)

    async def _put_new(self, item: dict[str, Any]) -> bool:
        try:
            await self._run("put_item", Item=item, ConditionExpression="attribute_not_exists(pk)")
        except self._db.exceptions.ConditionalCheckFailedException:
            return False
        return True

    async def put_triage(self, row: TriageRow) -> bool:
        item: dict[str, Any] = {
            "pk": {"S": f"TRIAGE#{row.email_id}"},
            "sk": {"S": "-"},
            "expires": {"N": str(_expires())},
            **{k: _attr(v) for k, v in asdict(row).items()},
        }
        if row.needs_reply:
            item["reply_pk"] = {"S": REPLY_PK}
            item["reply_sk"] = {"S": f"{row.received_at}#{row.email_id}"}
        return await self._put_new(item)

    async def has_triage(self, email_id: str) -> bool:
        out = await self._run(
            "get_item",
            Key={"pk": {"S": f"TRIAGE#{email_id}"}, "sk": {"S": "-"}},
            ProjectionExpression="pk",
            ConsistentRead=True,
        )
        return "Item" in out

    async def get_triage(self, email_ids: Iterable[str]) -> dict[str, TriageRow]:
        ids = list(dict.fromkeys(email_ids))
        found: dict[str, TriageRow] = {}
        for start in range(0, len(ids), 100):
            keys = [
                {"pk": {"S": f"TRIAGE#{i}"}, "sk": {"S": "-"}} for i in ids[start : start + 100]
            ]
            request: dict[str, Any] = {self._table: {"Keys": keys}}
            while request:
                out = await asyncio.to_thread(self._db.batch_get_item, RequestItems=request)
                for item in out.get("Responses", {}).get(self._table, []):
                    row = _triage(item)
                    found[row.email_id] = row
                request = out.get("UnprocessedKeys") or {}
        return found

    async def waiting_replies(self, since: str) -> list[TriageRow]:
        rows: list[TriageRow] = []
        kwargs: dict[str, Any] = {
            "IndexName": REPLY_INDEX,
            "KeyConditionExpression": "reply_pk = :p AND reply_sk >= :s",
            "ExpressionAttributeValues": {":p": {"S": REPLY_PK}, ":s": {"S": since}},
        }
        while True:
            out = await self._run("query", **kwargs)
            rows.extend(_triage(i) for i in out.get("Items", []))
            if "LastEvaluatedKey" not in out:
                return rows
            kwargs["ExclusiveStartKey"] = out["LastEvaluatedKey"]

    async def get_cursor(self) -> Cursor | None:
        out = await self._run(
            "get_item", Key={"pk": {"S": "CURSOR"}, "sk": {"S": "digest"}}, ConsistentRead=True
        )
        item = out.get("Item")
        return Cursor(item["state"]["S"], item["at"]["S"]) if item else None

    async def set_cursor(self, cursor: Cursor) -> None:
        await self._run(
            "put_item",
            Item={
                "pk": {"S": "CURSOR"},
                "sk": {"S": "digest"},
                "state": {"S": cursor.state},
                "at": {"S": cursor.at},
            },
        )

    async def put_send_marker(self, marker: SendMarker) -> bool:
        return await self._put_new(
            {
                "pk": {"S": f"SEND#{marker.key}"},
                "sk": {"S": "-"},
                "expires": {"N": str(_expires())},
                "email_id": {"S": marker.email_id},
                "attempted_at": {"S": marker.attempted_at},
            }
        )

    async def get_send_marker(self, key: str) -> SendMarker | None:
        out = await self._run(
            "get_item", Key={"pk": {"S": f"SEND#{key}"}, "sk": {"S": "-"}}, ConsistentRead=True
        )
        item = out.get("Item")
        if not item:
            return None
        return SendMarker(key, item["email_id"]["S"], item["attempted_at"]["S"])


def _attr(value: Any) -> dict[str, Any]:
    if isinstance(value, bool):
        return {"BOOL": value}
    return {"S": str(value)}


def _triage(item: dict[str, Any]) -> TriageRow:
    return TriageRow(
        email_id=item["email_id"]["S"],
        event_id=item["event_id"]["S"],
        thread_id=item["thread_id"]["S"],
        received_at=item["received_at"]["S"],
        category=item["category"]["S"],
        priority=item["priority"]["S"],
        needs_reply=item["needs_reply"]["BOOL"],
        summary=item["summary"]["S"],
    )
