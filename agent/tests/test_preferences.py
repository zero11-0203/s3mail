from __future__ import annotations

from datetime import UTC, datetime

import boto3
import pytest
from botocore.stub import ANY, Stubber

from s3mail_agent.preferences import (
    MAX_PREFERENCE_CHARS,
    AgentCorePreferences,
    MemoryPreferences,
    TooManyPreferences,
    clean,
)


def stubbed() -> tuple[AgentCorePreferences, Stubber]:
    client = boto3.client(
        "bedrock-agentcore",
        region_name="us-east-1",
        aws_access_key_id="test",
        aws_secret_access_key="test",  # noqa: S106 - a stub credential
    )
    return AgentCorePreferences("s3mail_agent_memory-AbCdEf1234", "us-east-1", client), Stubber(
        client
    )


def listing(*records: tuple[str, str]) -> dict[str, object]:
    return {
        "memoryRecordSummaries": [
            {
                "memoryRecordId": rid,
                "content": {"text": text},
                "memoryStrategyId": "none",
                "namespaces": ["/preferences/owner"],
                "createdAt": datetime(2026, 10, 1, tzinfo=UTC),
            }
            for rid, text in records
        ]
    }


RID = "mem-" + "a" * 40
LIST = {
    "memoryId": "s3mail_agent_memory-AbCdEf1234",
    "namespace": "/preferences/owner",
    "maxResults": 100,
}


async def test_remember_lists_and_forgets_through_memory_records() -> None:
    prefs, stub = stubbed()
    stub.add_response("list_memory_records", listing(), LIST)
    stub.add_response(
        "batch_create_memory_records",
        {
            "successfulRecords": [
                {"memoryRecordId": RID, "status": "SUCCEEDED", "requestIdentifier": "x"}
            ],
            "failedRecords": [],
        },
        {
            "memoryId": "s3mail_agent_memory-AbCdEf1234",
            "records": [
                {
                    "requestIdentifier": ANY,
                    "namespaces": ["/preferences/owner"],
                    "content": {"text": "Sign off with Cheers."},
                    "timestamp": ANY,
                }
            ],
            "clientToken": ANY,
        },
    )
    stub.add_response("list_memory_records", listing((RID, "Sign off with Cheers.")), LIST)
    stub.add_response("list_memory_records", listing((RID, "Sign off with Cheers.")), LIST)
    stub.add_response(
        "batch_delete_memory_records",
        {
            "successfulRecords": [{"memoryRecordId": RID, "status": "SUCCEEDED"}],
            "failedRecords": [],
        },
        {
            "memoryId": "s3mail_agent_memory-AbCdEf1234",
            "records": [{"memoryRecordId": RID, "namespace": "/preferences/owner"}],
        },
    )
    with stub:
        added = await prefs.add("owner", "Sign off with Cheers.")
        assert added.id == RID
        assert [p.text for p in await prefs.all("owner")] == ["Sign off with Cheers."]
        assert await prefs.forget("owner", None) == 1
    stub.assert_no_pending_responses()


async def test_memory_preferences_cap_and_forget_by_id() -> None:
    prefs = MemoryPreferences()
    for i in range(50):
        await prefs.add("owner", f"pref {i}")
    with pytest.raises(TooManyPreferences):
        await prefs.add("owner", "one more")
    assert await prefs.forget("owner", ["P1", "P2"]) == 2
    assert len(await prefs.all("owner")) == 48


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("  Be brief.\n", "Be brief."),
        ("Tone:\x1b[31m warm‮", "Tone: [31m warm"),
        ("", None),
        ("x" * (MAX_PREFERENCE_CHARS + 1), None),
    ],
)
def test_clean(text: str, expected: str | None) -> None:
    assert clean(text) == expected
