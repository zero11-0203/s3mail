"""The agent table (docs/specs/agent.md#state-and-memory): derived, expiring
data only. s3mail stays the source of truth.

Items (key pk, sk):

    TRIAGE#<emailId>  -     one triage row per email; the first event wins
    CURSOR            digest  the Email state the last digest covered
    DIGEST#<window>   -     the lease of a run delivering that digest window
    DIGEST#<window>   end   the cursor the window's digest email covers
    SEND#<hash>       -     the once-only marker of an approved send (A3)
    UPDATE#<hash>     -     the once-only marker of an approved update (A3)

Triage rows that need a reply carry the sparse `reply` index keys, so the
digest can list threads waiting on the owner without a scan. Every item but
the cursor expires after 30 days. The cursor is written only over the value
its run read, so overlapping digests cannot move it backwards.
"""

from __future__ import annotations

import asyncio
import time
from collections.abc import Iterable
from dataclasses import asdict, dataclass
from typing import Any, Protocol

TTL_SECONDS = 30 * 24 * 3600
# BatchGetItem attempts while keys come back unprocessed.
BATCH_ATTEMPTS = 5
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
    # None after a truncated query fallback: the next digest queries from at,
    # after the email after_id in (receivedAt, id) order.
    state: str | None
    at: str
    after_id: str | None = None


@dataclass(frozen=True)
class ActionMarker:
    kind: str  # "send" or "update"
    key: str  # the approval hash
    email_id: str
    attempted_at: str


class AgentTable(Protocol):
    async def put_triage(self, row: TriageRow) -> bool:
        """Store the row unless the email already has one; True if stored."""

    async def has_triage(self, email_id: str) -> bool: ...

    async def get_triage(self, email_ids: Iterable[str]) -> dict[str, TriageRow]: ...

    async def waiting_replies(self, since: str, until: str, limit: int) -> list[TriageRow]:
        """The newest limit rows that need a reply, received from since to
        until (ISO 8601), oldest first."""

    async def get_cursor(self) -> Cursor | None: ...

    async def set_cursor(self, cursor: Cursor, expected: Cursor | None) -> bool:
        """Replace the cursor only if it still equals expected (None: absent);
        True if replaced."""

    async def claim_digest(self, window: str, now: int, until: int) -> bool:
        """Lease a digest window unless another run holds an unexpired lease
        (epoch seconds); True if claimed."""

    async def set_digest_end(self, window: str, end: Cursor) -> None:
        """Record the cursor a window's digest covers, before it is created."""

    async def get_digest_end(self, window: str) -> Cursor | None: ...

    async def put_marker(self, marker: ActionMarker) -> bool:
        """Store the marker unless it exists; True if stored."""

    async def get_marker(self, kind: str, key: str) -> ActionMarker | None: ...


def _expires() -> int:
    return int(time.time()) + TTL_SECONDS


class MemoryTable:
    """An in-process AgentTable for tests and evaluations."""

    def __init__(self) -> None:
        self.triage: dict[str, TriageRow] = {}
        self.cursor: Cursor | None = None
        self.markers: dict[str, ActionMarker] = {}
        self.leases: dict[str, int] = {}
        self.ends: dict[str, Cursor] = {}
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

    async def waiting_replies(self, since: str, until: str, limit: int) -> list[TriageRow]:
        rows = [
            r for r in self.triage.values() if r.needs_reply and since <= r.received_at <= until
        ]
        return sorted(rows, key=lambda r: (r.received_at, r.email_id))[-limit:]

    async def get_cursor(self) -> Cursor | None:
        return self.cursor

    async def set_cursor(self, cursor: Cursor, expected: Cursor | None) -> bool:
        if self.cursor != expected:
            return False
        self.cursor = cursor
        return True

    async def claim_digest(self, window: str, now: int, until: int) -> bool:
        if self.leases.get(window, 0) >= now:
            return False
        self.leases[window] = until
        return True

    async def set_digest_end(self, window: str, end: Cursor) -> None:
        self.ends[window] = end

    async def get_digest_end(self, window: str) -> Cursor | None:
        return self.ends.get(window)

    async def put_marker(self, marker: ActionMarker) -> bool:
        key = f"{marker.kind}#{marker.key}"
        if key in self.markers:
            return False
        self.markers[key] = marker
        return True

    async def get_marker(self, kind: str, key: str) -> ActionMarker | None:
        return self.markers.get(f"{kind}#{key}")


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
            for attempt in range(BATCH_ATTEMPTS):
                if attempt:
                    # Unprocessed keys mean throttling: back off before retrying.
                    await asyncio.sleep(0.1 * 2**attempt)
                out = await asyncio.to_thread(self._db.batch_get_item, RequestItems=request)
                for item in out.get("Responses", {}).get(self._table, []):
                    row = _triage(item)
                    found[row.email_id] = row
                request = out.get("UnprocessedKeys") or {}
                if not request:
                    break
            else:
                raise RuntimeError("agent table reads stayed unprocessed")
        return found

    async def waiting_replies(self, since: str, until: str, limit: int) -> list[TriageRow]:
        rows: list[TriageRow] = []
        kwargs: dict[str, Any] = {
            "IndexName": REPLY_INDEX,
            # reply_sk is "<received>#<id>"; "~" sorts after "#", so rows
            # received at until are included.
            "KeyConditionExpression": "reply_pk = :p AND reply_sk BETWEEN :s AND :u",
            "ExpressionAttributeValues": {
                ":p": {"S": REPLY_PK},
                ":s": {"S": since},
                ":u": {"S": until + "~"},
            },
            "ScanIndexForward": False,
            "Limit": limit,
        }
        while len(rows) < limit:
            out = await self._run("query", **kwargs)
            rows.extend(_triage(i) for i in out.get("Items", []))
            if "LastEvaluatedKey" not in out:
                break
            kwargs["ExclusiveStartKey"] = out["LastEvaluatedKey"]
            kwargs["Limit"] = limit - len(rows)
        return rows[:limit][::-1]

    async def get_cursor(self) -> Cursor | None:
        out = await self._run(
            "get_item", Key={"pk": {"S": "CURSOR"}, "sk": {"S": "digest"}}, ConsistentRead=True
        )
        item = out.get("Item")
        return _cursor(item) if item else None

    async def set_cursor(self, cursor: Cursor, expected: Cursor | None) -> bool:
        item = {"pk": {"S": "CURSOR"}, "sk": {"S": "digest"}, **_cursor_attrs(cursor)}
        if expected is None:
            condition: dict[str, Any] = {"ConditionExpression": "attribute_not_exists(pk)"}
        else:
            # Every field: runs sharing a time still differ by state or by
            # the email they resumed after.
            names = {"#a": "at", "#s": "state", "#i": "after_id"}
            values = {":at": {"S": expected.at}}
            tests = ["#a = :at"]
            for name, value in (("#s", expected.state), ("#i", expected.after_id)):
                if value is None:
                    tests.append(f"attribute_not_exists({name})")
                else:
                    tests.append(f"{name} = :{name[1]}")
                    values[f":{name[1]}"] = {"S": value}
            condition = {
                "ConditionExpression": " AND ".join(tests),
                "ExpressionAttributeNames": names,
                "ExpressionAttributeValues": values,
            }
        try:
            await self._run("put_item", Item=item, **condition)
        except self._db.exceptions.ConditionalCheckFailedException:
            return False
        return True

    async def claim_digest(self, window: str, now: int, until: int) -> bool:
        try:
            await self._run(
                "put_item",
                Item={
                    "pk": {"S": f"DIGEST#{window}"},
                    "sk": {"S": "-"},
                    "expires": {"N": str(_expires())},
                    "until": {"N": str(until)},
                },
                ConditionExpression="attribute_not_exists(pk) OR #u < :now",
                ExpressionAttributeNames={"#u": "until"},
                ExpressionAttributeValues={":now": {"N": str(now)}},
            )
        except self._db.exceptions.ConditionalCheckFailedException:
            return False
        return True

    async def set_digest_end(self, window: str, end: Cursor) -> None:
        await self._run(
            "put_item",
            Item={
                "pk": {"S": f"DIGEST#{window}"},
                "sk": {"S": "end"},
                "expires": {"N": str(_expires())},
                **_cursor_attrs(end),
            },
        )

    async def get_digest_end(self, window: str) -> Cursor | None:
        out = await self._run(
            "get_item",
            Key={"pk": {"S": f"DIGEST#{window}"}, "sk": {"S": "end"}},
            ConsistentRead=True,
        )
        item = out.get("Item")
        return _cursor(item) if item else None

    async def put_marker(self, marker: ActionMarker) -> bool:
        return await self._put_new(
            {
                "pk": {"S": f"{marker.kind.upper()}#{marker.key}"},
                "sk": {"S": "-"},
                "expires": {"N": str(_expires())},
                "email_id": {"S": marker.email_id},
                "attempted_at": {"S": marker.attempted_at},
            }
        )

    async def get_marker(self, kind: str, key: str) -> ActionMarker | None:
        out = await self._run(
            "get_item",
            Key={"pk": {"S": f"{kind.upper()}#{key}"}, "sk": {"S": "-"}},
            ConsistentRead=True,
        )
        item = out.get("Item")
        if not item:
            return None
        return ActionMarker(kind, key, item["email_id"]["S"], item["attempted_at"]["S"])


def _cursor_attrs(cursor: Cursor) -> dict[str, Any]:
    attrs: dict[str, Any] = {"at": {"S": cursor.at}}
    if cursor.state is not None:
        attrs["state"] = {"S": cursor.state}
    if cursor.after_id is not None:
        attrs["after_id"] = {"S": cursor.after_id}
    return attrs


def _cursor(item: dict[str, Any]) -> Cursor:
    return Cursor(
        item["state"]["S"] if "state" in item else None,
        item["at"]["S"],
        item["after_id"]["S"] if "after_id" in item else None,
    )


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
