from __future__ import annotations

from datetime import UTC, datetime

from langchain_core.messages import AIMessage

from s3mail_agent.digest import run_digest
from s3mail_agent.jmap import EMAIL_SET, MAILBOX_SET, JmapClient
from s3mail_agent.store import Cursor, MemoryTable, TriageRow

from .conftest import SESSION_URL, ScriptFactory
from .mailserver import MailServer, message

NOW = datetime(2026, 10, 18, 7, 0, tzinfo=UTC)


def row(
    id: str, thread: str, received: str, *, category: str = "work", reply: bool = False
) -> TriageRow:
    return TriageRow(
        id, f"{id}:received", thread, received, category, "normal", reply, f"Summary of {id}."
    )


def setup() -> tuple[MailServer, MemoryTable]:
    s, table = MailServer(), MemoryTable()
    s.deliver(
        message("M0", "T0", "old@example.com", "Before the cursor", "2026-10-16T09:00:00Z", "old")
    )
    table.cursor = Cursor(f"s{s.seq}", "2026-10-17T07:00:00Z")
    s.deliver(
        message(
            "M1",
            "T1",
            "colleague@example.com",
            "Q3 numbers",
            "2026-10-17T09:00:00Z",
            "Send the numbers?",
        )
    )
    s.deliver(
        message("M2", "T2", "news@example.org", "Weekly news", "2026-10-17T10:00:00Z", "News.")
    )
    s.deliver(
        message("M3", "T3", "shop@example.net", "Order shipped", "2026-10-17T11:00:00Z", "Shipped.")
    )
    s.deliver(
        message(
            "D9",
            "T1",
            "owner@example.com",
            "Re: Q3 numbers",
            "2026-10-17T12:00:00Z",
            "draft",
            mailbox="drafts",
        )
    )
    table.triage["M1"] = row("M1", "T1", "2026-10-17T09:00:00Z", reply=True)
    table.triage["M2"] = row("M2", "T2", "2026-10-17T10:00:00Z", category="newsletter")
    return s, table


def client(s: MailServer, *, writes: frozenset[str] = frozenset()) -> JmapClient:
    return JmapClient(SESSION_URL, "t", transport=s.transport(), backoff=0, writes=writes)


async def test_digest_lists_new_mail_and_advances_the_cursor(scripted: ScriptFactory) -> None:
    s, table = setup()
    model = scripted(AIMessage(content="The Q3 numbers need a reply [email:M1]."))
    result = await run_digest(client(s), model, table, now=NOW)
    text = result["digest"]
    assert result["emails"] == 3 and result["needs_reply"] == 1 and result["triaged"] == 2
    assert (
        "Needs a reply:\n- Q3 numbers — colleague@example.com — Summary of M1. [email:M1]" in text
    )
    assert "Not triaged (1):" in text and "Order shipped" in text
    assert "Before the cursor" not in text and "draft" not in text.lower().split("digest")[1]
    assert text.index("The Q3 numbers need a reply") < text.index("Needs a reply")
    assert table.cursor == Cursor(f"s{s.seq}", "2026-10-18T07:00:00Z")
    assert s.writes == []

    # Nothing new on the next run.
    again = await run_digest(client(s), scripted(), table, now=NOW)
    assert "No new mail." in again["digest"] and again["model_calls"] == 0


async def test_peek_keeps_the_cursor(scripted: ScriptFactory) -> None:
    s, table = setup()
    before = table.cursor
    await run_digest(
        client(s), scripted(AIMessage(content="x [email:M1]")), table, peek=True, now=NOW
    )
    assert table.cursor == before


async def test_destroyed_mail_disappears(scripted: ScriptFactory) -> None:
    s, table = setup()
    s.mailbox = [e for e in s.mailbox if e["id"] != "M1"]  # destroyed after triage
    result = await run_digest(
        client(s), scripted(AIMessage(content="News [email:M2].")), table, now=NOW
    )
    assert "Q3 numbers" not in result["digest"] and result["emails"] == 2


async def test_invalid_overview_citations_drop_the_overview(scripted: ScriptFactory) -> None:
    s, table = setup()
    model = scripted(AIMessage(content="See [email:M0]."), AIMessage(content="No citations here."))
    result = await run_digest(client(s), model, table, now=NOW)
    assert result["overview"] is False and "See [email:M0]" not in result["digest"]
    assert "Q3 numbers" in result["digest"]


async def test_lost_cursor_state_falls_back_to_a_time_query(scripted: ScriptFactory) -> None:
    s, table = setup()
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z")
    result = await run_digest(
        client(s), scripted(AIMessage(content="x [email:M1]")), table, now=NOW
    )
    assert result["emails"] == 3
    assert any(n == "Email/query" for n, _ in s.calls)


async def test_changes_are_paged(scripted: ScriptFactory) -> None:
    s, table = setup()
    for i in range(300):
        s.deliver(
            message(f"N{i}", f"TN{i}", "bulk@example.org", f"Bulk {i}", "2026-10-17T13:00:00Z", "b")
        )
    result = await run_digest(
        client(s), scripted(AIMessage(content="Bulk [email:N0].")), table, now=NOW
    )
    # MAX_EMAILS (300) of the 304 created ids; the draft among them is skipped.
    assert result["emails"] == 299
    assert "more not listed" in result["digest"]
    assert sum(n == "Email/changes" for n, _ in s.calls) == 2


async def test_threads_waiting_on_the_owner_are_reminded(scripted: ScriptFactory) -> None:
    s, table = setup()
    s.deliver(
        message("W1", "TW1", "friend@example.com", "Weekend?", "2026-10-10T09:00:00Z", "Free?")
    )
    s.deliver(
        message("W2", "TW2", "client@example.com", "Proposal", "2026-10-11T09:00:00Z", "Thoughts?")
    )
    s.deliver(
        message(
            "W3",
            "TW2",
            "owner@example.com",
            "Re: Proposal",
            "2026-10-12T09:00:00Z",
            "Yes.",
            mailbox="sent",
        )
    )
    s.deliver(
        message(
            "W4",
            "TW4",
            "boss@example.com",
            "Answered",
            "2026-10-10T09:00:00Z",
            "?",
            keywords={"$answered": True},
        )
    )
    for id, thread, at in (
        ("W1", "TW1", "2026-10-10T09:00:00Z"),
        ("W2", "TW2", "2026-10-11T09:00:00Z"),
        ("W4", "TW4", "2026-10-10T09:00:00Z"),
    ):
        table.triage[id] = row(id, thread, at, reply=True)
    table.cursor = Cursor(f"s{s.seq}", "2026-10-17T07:00:00Z")
    result = await run_digest(
        client(s), scripted(AIMessage(content="Weekend plans wait [email:W1].")), table, now=NOW
    )
    text = result["digest"]
    assert result["waiting"] == 1
    assert (
        "Still waiting on you:\n- Weekend? — friend@example.com — Summary of W1. [email:W1]" in text
    )
    assert "Proposal" not in text and "Answered" not in text


async def test_digest_delivered_as_an_email_into_the_agent_mailbox(scripted: ScriptFactory) -> None:
    s, table = setup()
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    result = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
    )
    email = s.by_id(result["email_id"])
    assert email is not None and "digest" not in result
    agent = next(b for b in s.mailboxes if b["name"] == "Agent")
    assert email["mailboxIds"] == {agent["id"]: True} and email["keywords"] == {}
    assert email["subject"] == "Mail digest 2026-10-18" and "Q3 numbers" in email["body"]
    assert [n for n, _ in s.writes] == ["Mailbox/set", "Email/set"]

    # The next digest reuses the mailbox and does not list its own digest.
    s.deliver(message("M5", "T5", "a@example.com", "New", "2026-10-18T08:00:00Z", "n"))
    again = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M5]")), table, deliver="mail", now=NOW
    )
    assert [n for n, _ in s.writes] == ["Mailbox/set", "Email/set", "Email/set"]
    assert "Mail digest" not in s.by_id(again["email_id"])["body"].split("\n", 1)[1]
