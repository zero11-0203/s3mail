"""The deployed table's requests, checked with botocore's Stubber."""

from __future__ import annotations

from typing import Any

import boto3
import pytest
from botocore.stub import Stubber

from s3mail_agent import store
from s3mail_agent.store import ActionMarker, Cursor, DynamoTable

TABLE = "s3mail-agent"


def table() -> tuple[DynamoTable, Stubber]:
    client = boto3.client(
        "dynamodb",
        region_name="us-east-1",
        aws_access_key_id="test",
        aws_secret_access_key="test",  # noqa: S106 - a stub, never sent
    )
    return DynamoTable(TABLE, "us-east-1", client=client), Stubber(client)


CURSOR_KEY = {"pk": {"S": "CURSOR"}, "sk": {"S": "digest"}}


async def test_the_first_cursor_is_written_only_if_absent() -> None:
    t, stub = table()
    item = {**CURSOR_KEY, "at": {"S": "t1"}, "state": {"S": "s1"}}
    stub.add_response(
        "put_item",
        {},
        {"TableName": TABLE, "Item": item, "ConditionExpression": "attribute_not_exists(pk)"},
    )
    with stub:
        assert await t.set_cursor(Cursor("s1", "t1"), None)


async def test_the_cursor_is_written_only_over_the_value_read() -> None:
    t, stub = table()
    item: dict[str, Any] = {**CURSOR_KEY, "at": {"S": "t2"}}
    stub.add_response(
        "put_item",
        {},
        {
            "TableName": TABLE,
            "Item": item,
            "ConditionExpression": "#a = :at AND #s = :s",
            "ExpressionAttributeNames": {"#a": "at", "#s": "state"},
            "ExpressionAttributeValues": {":at": {"S": "t1"}, ":s": {"S": "s1"}},
        },
    )
    stub.add_client_error("put_item", "ConditionalCheckFailedException")
    with stub:
        # A truncated query fallback stores no state.
        assert await t.set_cursor(Cursor(None, "t2"), Cursor("s1", "t1"))
        assert not await t.set_cursor(Cursor(None, "t3"), Cursor(None, "t2"))


async def test_a_cursor_without_state_reads_back() -> None:
    t, stub = table()
    stub.add_response("get_item", {"Item": {**CURSOR_KEY, "at": {"S": "t2"}}})
    with stub:
        assert await t.get_cursor() == Cursor(None, "t2")


async def test_a_digest_lease_is_taken_only_when_free_or_expired() -> None:
    t, stub = table()
    stub.add_response("put_item", {})
    stub.add_client_error("put_item", "ConditionalCheckFailedException")
    with stub:
        assert await t.claim_digest("w1", 100, 700)
        assert not await t.claim_digest("w1", 200, 800)


async def test_markers_are_keyed_by_kind_and_written_once() -> None:
    t, stub = table()
    stub.add_response("put_item", {})
    stub.add_client_error("put_item", "ConditionalCheckFailedException")
    stub.add_response(
        "get_item",
        {"Item": {"email_id": {"S": "M1"}, "attempted_at": {"S": "t"}}},
        {
            "TableName": TABLE,
            "Key": {"pk": {"S": "UPDATE#h1"}, "sk": {"S": "-"}},
            "ConsistentRead": True,
        },
    )
    with stub:
        marker = ActionMarker("update", "h1", "M1", "t")
        assert await t.put_marker(marker)
        assert not await t.put_marker(marker)
        assert await t.get_marker("update", "h1") == marker


async def test_unprocessed_triage_reads_are_retried_a_bounded_number_of_times(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def no_wait(seconds: float) -> None:
        pass

    monkeypatch.setattr(store.asyncio, "sleep", no_wait)
    t, stub = table()
    keys = {TABLE: {"Keys": [{"pk": {"S": "TRIAGE#M1"}, "sk": {"S": "-"}}]}}
    for _ in range(store.BATCH_ATTEMPTS):
        stub.add_response("batch_get_item", {"Responses": {TABLE: []}, "UnprocessedKeys": keys})
    with stub, pytest.raises(RuntimeError, match="unprocessed"):
        await t.get_triage(["M1"])
