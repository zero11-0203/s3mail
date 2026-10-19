from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

import pytest
from langchain_core.messages import AIMessage

from s3mail_agent import digest
from s3mail_agent.digest import run_digest
from s3mail_agent.jmap import CORE, EMAIL_SET, MAILBOX_SET, JmapClient, JmapMethodError
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


async def test_changes_are_paged_and_a_capped_window_continues(scripted: ScriptFactory) -> None:
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
    assert "More new mail follows in the next digest." in result["digest"]
    assert sum(n == "Email/changes" for n, _ in s.calls) == 2
    # The next digest lists the rest, so every email appears exactly once.
    again = await run_digest(
        client(s), scripted(AIMessage(content="Bulk [email:N299].")), table, now=NOW
    )
    assert again["emails"] == 4 and "follows in the next digest" not in again["digest"]
    listed = [
        line.rsplit("[email:", 1)[1].rstrip("]")
        for text in (result["digest"], again["digest"])
        for line in text.splitlines()
        if line.startswith("- Bulk")
    ]
    assert sorted(listed) == sorted(f"N{i}" for i in range(300))
    assert table.cursor is not None and table.cursor.state == f"s{s.seq}"


async def test_a_truncated_query_fallback_continues_by_time(scripted: ScriptFactory) -> None:
    s, table = setup()
    for i in range(301):
        at = f"2026-10-17T{13 + i // 60:02d}:{i % 60:02d}:00Z"
        s.deliver(message(f"N{i}", f"TN{i}", "bulk@example.org", f"Bulk {i}", at, "b"))
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z")
    result = await run_digest(client(s), scripted(), table, now=NOW)
    assert "follows in the next digest" in result["digest"]
    # The next digest queries on after the last listed email, from the state
    # read before this page.
    assert table.cursor is not None and table.cursor.after_id == "N295"
    assert table.cursor.state == f"s{s.seq}" and table.cursor.at > "2026-10-17T07:00:00Z"


@pytest.mark.parametrize("tied", [True, False])
async def test_a_truncated_query_fallback_lists_every_email_once(
    scripted: ScriptFactory, tied: bool
) -> None:
    s, table = setup()
    for i in range(650):
        at = "2026-10-17T13:00:00Z" if tied else f"2026-10-17T{13 + i // 60:02d}:{i % 60:02d}:00Z"
        s.deliver(message(f"N{i:03d}", f"TN{i}", "b@example.org", f"Bulk {i}", at, "b"))
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z")
    listed: list[str] = []
    for _ in range(4):
        text = (await run_digest(client(s), scripted(), table, now=NOW))["digest"]
        listed += [
            line.rsplit("[email:", 1)[1].rstrip("]")
            for line in text.splitlines()
            if line.startswith("- Bulk")
        ]
    assert sorted(listed) == [f"N{i:03d}" for i in range(650)]
    # Once the backlog is listed, the cursor holds a full state again.
    assert table.cursor is not None and table.cursor.state is not None
    assert table.cursor.after_id is None


def bulk(text: str) -> list[str]:
    return [
        line.rsplit("[email:", 1)[1].rstrip("]")
        for line in text.splitlines()
        if line.startswith("- Bulk")
    ]


@pytest.mark.parametrize(
    "late_at",
    ["2026-10-17T12:00:00Z", "2026-10-17T13:00:00Z"],
    ids=["earlier", "same-second"],
)
async def test_mail_landing_behind_a_continuing_query_is_listed(
    scripted: ScriptFactory, late_at: str
) -> None:
    s, table = setup()
    for i in range(650):
        s.deliver(
            message(
                f"N{i:03d}", f"TN{i}", "b@example.org", f"Bulk {i}", "2026-10-17T13:00:00Z", "b"
            )
        )
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z")
    listed = bulk((await run_digest(client(s), scripted(), table, now=NOW))["digest"])
    # Ingested late, it sorts before where the query resumes; L < N by id.
    s.deliver(message("L1", "TL1", "b@example.org", "Bulk late", late_at, "b"))
    for _ in range(3):
        listed += bulk((await run_digest(client(s), scripted(), table, now=NOW))["digest"])
    assert sorted(listed) == ["L1", *(f"N{i:03d}" for i in range(650))]
    assert table.cursor is not None and table.cursor.after_id is None


@pytest.mark.parametrize(
    ("start", "gone"),
    [("expired", 1), ("none", 1), ("expired", 300), ("none", 300)],
    ids=["expired-tail", "first-tail", "expired-page", "first-page"],
)
async def test_mail_destroyed_at_a_capped_page_end_keeps_the_query_going(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch, start: str, gone: int
) -> None:
    s, table = setup()
    for i in range(650):
        s.deliver(
            message(
                f"N{i:03d}", f"TN{i}", "b@example.org", f"Bulk {i}", "2026-10-17T13:00:00Z", "b"
            )
        )
    # An expired cursor, or none: the first digest covers the same day.
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z") if start == "expired" else None
    real = s.method
    destroyed: set[str] = set()

    def method(name: str, args: dict[str, Any]) -> Any:
        out = real(name, args)
        if name == "Email/query" and not destroyed:
            # The end of the first page is destroyed before the digest reads it.
            destroyed.update(out["ids"][-gone:])
            s.mailbox = [e for e in s.mailbox if e["id"] not in destroyed]
            s.touch()
        return out

    monkeypatch.setattr(s, "method", method)
    listed: list[str] = []
    for _ in range(4):
        listed += bulk((await run_digest(client(s), scripted(), table, now=NOW))["digest"])
    assert sorted(listed) == [f"N{i:03d}" for i in range(650) if f"N{i:03d}" not in destroyed]
    assert table.cursor is not None and table.cursor.after_id is None
    assert table.cursor.state is not None


def destroy_pages(
    s: MailServer, monkeypatch: pytest.MonkeyPatch, pages: list[bool]
) -> tuple[set[str], list[bool]]:
    """Destroy fallback queries' whole pages before they are read: pages
    holds one entry per coming query, True to destroy its page, and tests
    may refill it between runs."""
    real = s.method
    destroyed: set[str] = set()

    def method(name: str, args: dict[str, Any]) -> Any:
        out = real(name, args)
        fallback = "after" in (args.get("filter") or {})
        if name == "Email/query" and fallback and pages and pages.pop(0):
            destroyed.update(out["ids"])
            s.mailbox = [e for e in s.mailbox if e["id"] not in destroyed]
            s.touch()
        return out

    monkeypatch.setattr(s, "method", method)
    return destroyed, pages


def mailed(s: MailServer, result: dict[str, Any]) -> list[str]:
    email = s.by_id(result["email_id"])
    assert email is not None
    return bulk(email["body"])


async def test_destroyed_pages_never_stall_mailed_digests(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    for i in range(950):
        s.deliver(
            message(
                f"N{i:03d}", f"TN{i}", "b@example.org", f"Bulk {i}", "2026-10-17T13:00:00Z", "b"
            )
        )
    table.cursor = Cursor("expired-state", "2026-10-17T07:00:00Z")
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    destroyed, pages = destroy_pages(s, monkeypatch, [])
    listed: list[str] = []
    for run in range(4):
        # The first query of each of the first two digests loses its whole
        # page; mail arrives once the deletions stop.
        pages[:] = [True] if run < 2 else []
        if run == 2:
            s.deliver(
                message("NEW", "TNEW", "b@example.org", "Bulk new", "2026-10-18T06:00:00Z", "b")
            )
        table.leases.clear()  # each run comes after the last one's lease
        result = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
        assert result["status"] == "ok" and "reused" not in result
        listed += mailed(s, result)
    survivors = [f"N{i:03d}" for i in range(950) if f"N{i:03d}" not in destroyed]
    assert len(destroyed) == 600 and sorted(listed) == sorted(["NEW", *survivors])


async def test_a_window_that_cannot_pass_destroyed_pages_is_retried_later(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    for i in range(1000):
        s.deliver(
            message(
                f"N{i:03d}", f"TN{i}", "b@example.org", f"Bulk {i}", "2026-10-17T13:00:00Z", "b"
            )
        )
    start = Cursor("expired-state", "2026-10-17T07:00:00Z")
    table.cursor = start
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    destroyed, _ = destroy_pages(s, monkeypatch, [True] * digest.QUERY_ATTEMPTS)
    result = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    assert result == {"status": "error", "error": "digest_retry"}
    assert table.cursor == start and not any(n == "Email/set" for n, _ in s.writes)
    table.leases.clear()
    again = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    assert again["status"] == "ok" and "reused" not in again
    survivors = [f"N{i:03d}" for i in range(1000) if f"N{i:03d}" not in destroyed]
    assert len(destroyed) == 900 and sorted(mailed(s, again)) == survivors


async def test_a_mailed_window_ending_where_it_started_is_not_delivered(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    table.cursor = Cursor(None, "2026-10-17T07:00:00Z")

    async def stuck(*_: Any) -> digest.Window:
        return digest.Window([], None, None, "2026-10-17T07:00:00Z")

    monkeypatch.setattr(digest, "_changes", stuck)
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    result = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    assert result == {"status": "error", "error": "digest_retry"}
    assert not any(n == "Email/set" for n, _ in s.writes) and table.ends == {}


async def test_a_change_page_budget_bounds_the_work(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    monkeypatch.setattr(digest, "CHANGES_PAGE", 1)
    monkeypatch.setattr(digest, "MAX_CHANGE_PAGES", 2)
    model = scripted(AIMessage(content="x [email:M1]"))
    result = await run_digest(client(s), model, table, now=NOW)
    assert sum(n == "Email/changes" for n, _ in s.calls) == 2 and result["emails"] == 2
    assert table.cursor is not None and table.cursor.state == "s3"


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
    start = table.cursor
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    result = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
    )
    email = s.by_id(result["email_id"])
    assert email is not None and "digest" not in result
    agent = next(b for b in s.mailboxes if b["name"] == "Agent")
    assert email["mailboxIds"] == {agent["id"]: True}
    assert list(email["keywords"]) == [digest.DIGEST_KEYWORD.format(digest.window_key(start))]
    assert email["subject"] == "Mail digest 2026-10-18" and "Q3 numbers" in email["body"]
    assert [n for n, _ in s.writes] == ["Mailbox/set", "Email/set"]

    # The next digest reuses the mailbox and does not list its own digest.
    s.deliver(message("M5", "T5", "a@example.com", "New", "2026-10-18T08:00:00Z", "n"))
    again = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M5]")), table, deliver="mail", now=NOW
    )
    assert [n for n, _ in s.writes] == ["Mailbox/set", "Email/set", "Email/set"]
    assert "Mail digest" not in s.by_id(again["email_id"])["body"].split("\n", 1)[1]


async def test_reminders_stay_within_the_thread_get_limit(scripted: ScriptFactory) -> None:
    s, table = setup()
    s.capabilities[CORE]["maxObjectsInGet"] = 64  # s3mail's limit
    for i in range(65):
        id, thread = f"W{i}", f"TW{i}"
        s.deliver(
            message(id, thread, "friend@example.com", f"Ask {i}", "2026-10-10T09:00:00Z", "?")
        )
        table.triage[id] = row(id, thread, "2026-10-10T09:00:00Z", reply=True)
    table.cursor = Cursor(f"s{s.seq}", "2026-10-17T07:00:00Z")
    model = scripted(AIMessage(content="x [email:W64]"))
    result = await run_digest(client(s), model, table, now=NOW)
    assert result["waiting"] == digest.MAX_REMINDERS
    assert all(len(a["ids"]) <= 64 for n, a in s.calls if n == "Thread/get")


async def test_a_sent_reply_early_in_a_long_thread_ends_the_reminder(
    scripted: ScriptFactory,
) -> None:
    s, table = setup()
    s.deliver(message("W0", "TW", "friend@example.com", "Q", "2026-10-05T09:00:00Z", "?"))
    s.deliver(
        message(
            "W1", "TW", "owner@example.com", "Re: Q", "2026-10-05T10:00:00Z", "A", mailbox="sent"
        )
    )
    for i in range(21):
        at = f"2026-10-06T{i:02d}:00:00Z"
        s.deliver(message(f"X{i}", "TW", "friend@example.com", "Re: Q", at, "more"))
    table.triage["W0"] = row("W0", "TW", "2026-10-05T09:00:00Z", reply=True)
    table.cursor = Cursor(f"s{s.seq}", "2026-10-17T07:00:00Z")
    result = await run_digest(client(s), scripted(), table, now=NOW)
    assert result["waiting"] == 0


async def test_a_thread_too_long_to_inspect_is_not_reminded(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    monkeypatch.setattr(digest, "MAX_LATER_READS", 2)
    s.deliver(message("W0", "TW", "friend@example.com", "Q", "2026-10-05T09:00:00Z", "?"))
    for i in range(3):
        at = f"2026-10-06T{i:02d}:00:00Z"
        s.deliver(message(f"X{i}", "TW", "friend@example.com", "Re: Q", at, "more"))
    table.triage["W0"] = row("W0", "TW", "2026-10-05T09:00:00Z", reply=True)
    table.cursor = Cursor(f"s{s.seq}", "2026-10-17T07:00:00Z")
    result = await run_digest(client(s), scripted(), table, now=NOW)
    # Unread later emails could hold the owner's reply: no reminder.
    assert result["waiting"] == 0


async def test_failed_reminders_do_not_stop_the_digest(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()

    async def broken(*args: object) -> None:
        raise JmapMethodError("serverFail")

    monkeypatch.setattr(digest, "_waiting", broken)
    model = scripted(AIMessage(content="x [email:M1]"))
    result = await run_digest(client(s), model, table, now=NOW)
    assert result["status"] == "ok" and "Q3 numbers" in result["digest"]
    assert "Reminders were unavailable this time." in result["digest"]


@pytest.mark.parametrize("failure", [TimeoutError("model timed out"), ValueError("bad reply")])
async def test_a_model_failure_delivers_the_digest_without_an_overview(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch, failure: Exception
) -> None:
    s, table = setup()

    async def fail(*args: object, **kwargs: object) -> None:
        raise failure

    monkeypatch.setattr(digest, "_overview", fail)
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    result = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    email = s.by_id(result["email_id"])
    assert result["overview"] is False and email is not None and "Q3 numbers" in email["body"]
    assert table.cursor is not None and table.cursor.state == f"s{s.seq - 1}"


class FlakyCursor(MemoryTable):
    def __init__(self, base: MemoryTable) -> None:
        super().__init__()
        self.triage, self.cursor = base.triage, base.cursor
        self.failures = 1

    async def set_cursor(self, cursor: Cursor, expected: Cursor | None) -> bool:
        if self.failures:
            self.failures -= 1
            raise RuntimeError("table unavailable")
        return await super().set_cursor(cursor, expected)


async def test_a_retried_delivery_finds_its_earlier_email(scripted: ScriptFactory) -> None:
    s, base = setup()
    table = FlakyCursor(base)
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    with pytest.raises(RuntimeError):
        await run_digest(
            jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
        )
    table.leases.clear()  # the failed run's lease has expired
    result = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
    )
    assert [n for n, _ in s.writes] == ["Mailbox/set", "Email/set"]
    digests = [e["id"] for e in s.mailbox if e["subject"].startswith("Mail digest")]
    assert digests == [result["email_id"]]
    # The retry advances to the cursor the reused email covers.
    assert table.cursor is not None and table.cursor.state == f"s{s.seq - 1}"


async def test_mail_arriving_before_a_retry_waits_for_the_next_digest(
    scripted: ScriptFactory,
) -> None:
    s, base = setup()
    table = FlakyCursor(base)
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    with pytest.raises(RuntimeError):
        await run_digest(
            jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
        )
    s.deliver(message("NEW", "TNEW", "a@example.com", "Fresh", "2026-10-18T06:00:00Z", "n"))
    table.leases.clear()
    retry = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    assert retry["reused"] is True
    following = await run_digest(
        jmap, scripted(AIMessage(content="x [email:NEW]")), table, deliver="mail", now=NOW
    )
    email = s.by_id(following["email_id"])
    assert email is not None and "[email:NEW]" in email["body"]


async def test_a_preview_never_stands_in_for_the_digest(scripted: ScriptFactory) -> None:
    s, table = setup()
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    preview = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", peek=True, now=NOW
    )
    preview_email = s.by_id(preview["email_id"])
    assert preview_email is not None and preview_email["keywords"] == {}
    s.deliver(message("NEW", "TNEW", "a@example.com", "Fresh", "2026-10-18T06:00:00Z", "n"))
    real = await run_digest(
        jmap, scripted(AIMessage(content="x [email:M1]")), table, deliver="mail", now=NOW
    )
    email = s.by_id(real["email_id"])
    assert real["email_id"] != preview["email_id"]
    assert email is not None and "[email:NEW]" in email["body"]


async def test_a_concurrent_delivery_of_the_same_window_is_refused(scripted: ScriptFactory) -> None:
    s, table = setup()
    jmap = client(s, writes=frozenset({EMAIL_SET, MAILBOX_SET}))
    stamp = int(NOW.timestamp())
    assert await table.claim_digest(digest.window_key(table.cursor), stamp, stamp + 600)
    result = await run_digest(jmap, scripted(), table, deliver="mail", now=NOW)
    assert result == {"status": "error", "error": "digest_in_progress"} and s.writes == []


async def test_a_run_finishing_last_never_moves_the_cursor_back(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    s, table = setup()
    start = table.cursor
    await run_digest(client(s), scripted(AIMessage(content="x [email:M1]")), table, now=NOW)
    newer = table.cursor
    assert newer != start

    async def stale() -> Cursor | None:
        return start  # this run read the cursor before the other one finished

    monkeypatch.setattr(table, "get_cursor", stale)
    later = await run_digest(client(s), scripted(AIMessage(content="x [email:M1]")), table, now=NOW)
    assert later["cursor"] == "already_advanced" and table.cursor == newer
