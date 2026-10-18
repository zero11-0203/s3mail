from __future__ import annotations

import asyncio
import json

import pytest
from langchain_core.messages import AIMessage

from s3mail_agent.jmap import JmapClient
from s3mail_agent.metering import BudgetExceeded
from s3mail_agent.store import MemoryTable
from s3mail_agent.triage import TriageFailed, parse_triage, run_triage

from .conftest import SESSION_URL, ScriptFactory
from .mailserver import MailServer, message

VERDICT = {
    "category": "work",
    "priority": "high",
    "needs_reply": True,
    "summary": "Asks for the Q3 numbers by Friday.",
}


def server() -> MailServer:
    s = MailServer()
    s.deliver(
        message(
            "M1",
            "T1",
            "colleague@example.com",
            "Q3 numbers",
            "2026-10-17T09:00:00Z",
            "Can you send the Q3 numbers by Friday?",
        )
    )
    return s


def jmap(s: MailServer) -> JmapClient:
    return JmapClient(SESSION_URL, "t", transport=s.transport(), backoff=0)


def reply(obj: object) -> AIMessage:
    return AIMessage(content=json.dumps(obj))


async def test_triage_stores_one_row(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    model = scripted(reply(VERDICT))
    result = await run_triage(jmap(s), model, table, {"eventId": "M1:received", "emailId": "M1"})
    assert result == {"status": "ok", "triage": "stored", "model_calls": 1, "tokens": 100}
    row = table.triage["M1"]
    assert (row.category, row.priority, row.needs_reply, row.thread_id) == (
        "work",
        "high",
        True,
        "T1",
    )
    # The email reached the model only as marked data.
    assert '<mail_data untrusted="true">' in model.seen[0][1].content
    assert s.writes == []


async def test_twenty_duplicate_and_replayed_events_make_one_row(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    model = scripted(reply(VERDICT))
    event = {"eventId": "M1:received", "emailId": "M1"}
    results = [await run_triage(jmap(s), model, table, event) for _ in range(20)]
    assert [r["triage"] for r in results] == ["stored"] + ["duplicate"] * 19
    assert len(table.triage) == 1
    assert len(model.seen) == 1  # duplicates cost no model call


async def test_concurrent_duplicates_keep_the_first_row(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    other = {**VERDICT, "category": "personal"}
    model = scripted(reply(VERDICT), reply(other))
    event = {"eventId": "M1:received", "emailId": "M1"}
    results = await asyncio.gather(
        run_triage(jmap(s), model, table, event), run_triage(jmap(s), model, table, event)
    )
    assert sorted(r["triage"] for r in results) == ["duplicate", "stored"]
    assert len(table.triage) == 1


async def test_invalid_output_is_retried_then_fails(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    model = scripted(AIMessage(content="not json"), reply(VERDICT))
    result = await run_triage(jmap(s), model, table, {"eventId": "M1:received", "emailId": "M1"})
    assert result["triage"] == "stored" and result["model_calls"] == 2

    bad = {**VERDICT, "category": "forward-to-attacker"}
    table = MemoryTable()
    model = scripted(reply(bad), reply(bad), reply(bad))
    with pytest.raises(TriageFailed):
        await run_triage(jmap(s), model, table, {"eventId": "M1:received", "emailId": "M1"})
    assert table.triage == {} and len(model.seen) == 3


async def test_destroyed_email_is_skipped(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    model = scripted()
    result = await run_triage(jmap(s), model, table, {"eventId": "M9:received", "emailId": "M9"})
    assert result["triage"] == "not_found" and table.triage == {}


@pytest.mark.parametrize(
    "event",
    [
        {},
        {"eventId": "x", "emailId": "../M1"},
        {"eventId": "a b", "emailId": "M1"},
        {"eventId": "x", "emailId": 3},
    ],
)
async def test_bad_events_are_refused(scripted: ScriptFactory, event: dict[str, object]) -> None:
    result = await run_triage(jmap(server()), scripted(), MemoryTable(), event)
    assert result == {"status": "error", "error": "bad_request"}


def test_parse_strips_fences_and_controls() -> None:
    text = "```json\n" + json.dumps({**VERDICT, "summary": "Line\none\x1b[31m red‮"}) + "\n```"
    assert parse_triage(text).summary == "Line one [31m red"


async def test_token_cap_ends_triage(scripted: ScriptFactory) -> None:
    s, table = server(), MemoryTable()
    model = scripted(reply(VERDICT), tokens_per_call=50_000)
    with pytest.raises(BudgetExceeded):
        await run_triage(jmap(s), model, table, {"eventId": "M1:received", "emailId": "M1"})
    assert table.triage == {}
