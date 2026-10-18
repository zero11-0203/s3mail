"""The daily digest (A1; delivered as an email from A2; reminders from A3).

changes since cursor (code) -> headers and triage rows (code)
  -> waiting threads (code) -> overview (model, cited) -> render (code)
  -> deliver: return to the CLI, or create the digest email (A2)

Code decides what the digest lists; the model writes only a short overview
whose citations must be emails in this digest, and a failed overview is
left out. Ids are re-read with Email/get, so destroyed mail never appears.

The cursor advances only after the digest was delivered, and only past what
it listed: a capped window ends at the last change page it consumed, and the
next digest continues there. It is written only over the value this run
read. A digest delivered as mail carries a keyword naming its window and is
created under a lease on that window, so a retry finds the earlier email
instead of creating another.
"""

from __future__ import annotations

import hashlib
import json
import logging
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from typing import Any

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import HumanMessage, SystemMessage

from .citations import check_evidence
from .jmap import EMAIL_SET, MAILBOX_SET, JmapClient, JmapMethodError
from .metering import BudgetExceeded, Meter
from .prompts import DIGEST_SYSTEM, EVIDENCE_RETRY
from .store import AgentTable, Cursor, TriageRow
from .tools import HEADER_PROPERTIES, envelope, header_of, mailbox_index

MAX_EMAILS = 300
CHANGES_PAGE = 256
# Email/changes pages per digest, also counting updates and destroys.
MAX_CHANGE_PAGES = 20
DIGEST_MODEL_CALLS = 2
DIGEST_MAX_TOKENS = 60_000
# Without a cursor (first digest or a lost table), cover the last day.
FIRST_WINDOW = timedelta(days=1)
# Threads waiting on the owner: needs-reply rows this old, up to this age.
REMIND_AFTER = timedelta(days=2)
REMIND_WITHIN = timedelta(days=21)
MAX_REMINDERS = 20
# Per digest, newest first: needs-reply rows checked and their threads'
# emails read, so a reply backlog bounds the work.
MAX_REMINDER_CANDIDATES = 200
MAX_REMINDER_READS = 600
THREAD_TAIL = 20
AGENT_MAILBOX = "Agent"
DIGEST_SENDER = {"name": "s3mail agent", "email": "agent@s3mail.invalid"}
# Marks the digest email of one window, so a retry can find it.
DIGEST_KEYWORD = "$s3mail-digest-{}"
# A delivering run's hold on its window; longer than a digest run takes.
DIGEST_LEASE = timedelta(minutes=10)

log = logging.getLogger("s3mail_agent")
# Mailboxes whose new emails are not news to the owner.
SKIP_ROLES = frozenset({"drafts", "sent", "trash"})


def _iso(t: datetime) -> str:
    return t.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


@dataclass(frozen=True)
class Window:
    ids: list[str]
    # How many more emails were left out, or None when the next digest
    # continues from where this one stopped.
    more: int | None
    state: str | None
    # The cursor time to store; None means the digest's own time.
    at: str | None = None


async def _changes(jmap: JmapClient, cursor: Cursor | None, now: datetime) -> Window:
    """Created email ids since the cursor, oldest first, and the state that
    covers exactly them."""
    if cursor is not None and cursor.state is not None:
        created: list[str] = []
        since = cursor.state
        try:
            for _ in range(MAX_CHANGE_PAGES):
                # Never ask for more than still fits, so the returned state
                # covers only listed changes.
                room = min(CHANGES_PAGE, MAX_EMAILS - len(created))
                out = await jmap.call(
                    [("Email/changes", {"sinceState": since, "maxChanges": room})]
                )
                page = out["c0"]
                created.extend(page.get("created", []))
                since = page["newState"]
                if not page.get("hasMoreChanges"):
                    return Window(created, 0, since)
                if len(created) >= MAX_EMAILS:
                    break
            return Window(created, None, since, cursor.at)
        except JmapMethodError as exc:
            if exc.type not in ("cannotCalculateChanges", "invalidArguments"):
                raise
    after = cursor.at if cursor is not None else _iso(now - FIRST_WINDOW)
    # A fresh query: received since the cursor time. The state comes first,
    # so a later digest sees anything that lands meanwhile.
    out = await jmap.call(
        [
            ("Email/get", {"ids": [], "properties": ["id"]}),
            (
                "Email/query",
                {
                    "filter": {"after": after},
                    "sort": [{"property": "receivedAt", "isAscending": True}],
                    "limit": MAX_EMAILS,
                    "calculateTotal": True,
                },
            ),
        ]
    )
    ids = out["c1"]["ids"]
    total = int(out["c1"].get("total") or len(ids))
    if total <= len(ids) or not ids:
        return Window(ids, 0, out["c0"]["state"])
    # Truncated: the state covers unlisted mail, so the next digest queries
    # from the last listed time instead, listing that second's emails again.
    last = await _headers(jmap, ids[-1:], ["id", "receivedAt"])
    at = last[0].get("receivedAt") if last else None
    if not at or at == after:
        # Time cannot move on; leave out the rest, as reported.
        return Window(ids, total - len(ids), out["c0"]["state"])
    return Window(ids, None, None, at)


async def _headers(jmap: JmapClient, ids: list[str], props: list[str]) -> list[dict[str, Any]]:
    session = await jmap.session()
    found: list[dict[str, Any]] = []
    for i in range(0, len(ids), session.max_objects_in_get):
        out = await jmap.call(
            [("Email/get", {"ids": ids[i : i + session.max_objects_in_get], "properties": props})]
        )
        found.extend(out["c0"]["list"])
    return found


async def _waiting(
    jmap: JmapClient, table: AgentTable, now: datetime, exclude: set[str], sent: str | None
) -> list[tuple[TriageRow, dict[str, Any]]]:
    """Needs-reply rows older than REMIND_AFTER whose thread has no later
    email from the owner (one in the Sent mailbox)."""
    rows = [
        r
        for r in await table.waiting_replies(_iso(now - REMIND_WITHIN))
        if r.received_at <= _iso(now - REMIND_AFTER) and r.email_id not in exclude
    ][-MAX_REMINDER_CANDIDATES:]
    if not rows:
        return []
    session = await jmap.session()
    thread_ids = list(dict.fromkeys(r.thread_id for r in rows))
    members: dict[str, list[str]] = {}
    for i in range(0, len(thread_ids), session.max_objects_in_get):
        threads = await jmap.call(
            [("Thread/get", {"ids": thread_ids[i : i + session.max_objects_in_get]})]
        )
        members.update((t["id"], t["emailIds"]) for t in threads["c0"]["list"])
    # Each row's email and its thread's newest emails, newest rows first,
    # within the read budget; rows left without their email are skipped.
    ids: list[str] = []
    for r in reversed(rows):
        wanted = [r.email_id, *members.get(r.thread_id, [])[-THREAD_TAIL:]]
        new = [i for i in dict.fromkeys(wanted) if i not in ids]
        if len(ids) + len(new) > MAX_REMINDER_READS:
            break
        ids.extend(new)
    emails = {e["id"]: e for e in await _headers(jmap, ids, [*HEADER_PROPERTIES, "keywords"])}
    waiting = []
    for r in rows:
        email = emails.get(r.email_id)
        if email is None:
            continue  # destroyed
        later = [
            emails[i]
            for i in members.get(r.thread_id, [])
            if i in emails and (emails[i].get("receivedAt") or "") > r.received_at
        ]
        replied = "$answered" in (email.get("keywords") or {}) or any(
            sent and sent in (e.get("mailboxIds") or {}) for e in later
        )
        if not replied:
            waiting.append((r, email))
    return waiting[-MAX_REMINDERS:]


def _line(email: dict[str, Any], row: TriageRow | None) -> str:
    h = header_of(email)
    sender = h["from"][0] if h["from"] else "(no sender)"
    subject = h["subject"] or "(no subject)"
    summary = f" — {row.summary}" if row else ""
    return f"- {subject} — {sender}{summary} [email:{h['id']}]"


def render(
    now: datetime,
    since: str | None,
    emails: list[dict[str, Any]],
    rows: dict[str, TriageRow],
    waiting: list[tuple[TriageRow, dict[str, Any]]],
    more: int | None,
    overview: str | None,
    reminders: bool = True,
) -> str:
    lines = [f"Mail digest, {now.astimezone(UTC):%Y-%m-%d %H:%M} UTC"]
    count = f"{len(emails) + more}" if more is not None else f"{len(emails)}+"
    lines.append(f"New since {since}: {count}" if since else f"New: {count}")
    if overview:
        lines += ["", overview]
    reply = [e for e in emails if (r := rows.get(e["id"])) and r.needs_reply]
    if reply:
        lines += ["", "Needs a reply:", *(_line(e, rows.get(e["id"])) for e in reply)]
    by_category: dict[str, list[dict[str, Any]]] = {}
    for e in emails:
        r = rows.get(e["id"])
        by_category.setdefault(r.category if r else "not triaged", []).append(e)
    for category in sorted(by_category, key=lambda c: (c == "not triaged", c)):
        group = by_category[category]
        lines += ["", f"{category.capitalize()} ({len(group)}):"]
        lines += [_line(e, rows.get(e["id"])) for e in group]
    if more is None:
        lines += ["", "More new mail follows in the next digest."]
    elif more:
        lines += ["", f"…and {more} more not listed."]
    if waiting:
        lines += ["", "Still waiting on you:"]
        lines += [_line(e, r) for r, e in waiting]
    if not reminders:
        lines += ["", "Reminders were unavailable this time."]
    if not emails and more == 0:
        lines += ["", "No new mail."]
    return "\n".join(lines)


async def _overview(
    model: BaseChatModel,
    meter: Meter,
    emails: list[dict[str, Any]],
    rows: dict[str, TriageRow],
    waiting: list[tuple[TriageRow, dict[str, Any]]],
) -> str | None:
    """A cited overview, or None when the model's citations stay invalid."""
    if not emails and not waiting:
        return None

    def item(e: dict[str, Any], r: TriageRow | None) -> dict[str, Any]:
        out = {**header_of(e)}
        if r:
            out.update(
                category=r.category,
                priority=r.priority,
                needs_reply=r.needs_reply,
                summary=r.summary,
            )
        return out

    data = {
        "new": [item(e, rows.get(e["id"])) for e in emails],
        "waiting_on_owner": [item(e, r) for r, e in waiting],
    }
    allowed = [e["id"] for e in emails] + [e["id"] for _, e in waiting]
    messages: list[Any] = [
        SystemMessage(content=DIGEST_SYSTEM),
        HumanMessage(content=envelope(data)),
    ]
    for _ in range(DIGEST_MODEL_CALLS):
        ai = await meter.invoke(model, messages)
        text = ai.text.strip()
        evidence = check_evidence(text, allowed, [])
        if not evidence.problems and evidence.emails:
            return text
        messages = [
            *messages,
            ai,
            HumanMessage(
                content=EVIDENCE_RETRY.format(
                    problems="; ".join(evidence.problems or ["cite the emails you mention"])
                )
            ),
        ]
    return None


async def _agent_mailbox(jmap: JmapClient) -> str:
    boxes = await mailbox_index(jmap)
    if AGENT_MAILBOX in boxes.top_level:
        return boxes.top_level[AGENT_MAILBOX]
    created = await jmap.write(
        MAILBOX_SET, {"create": {"agent": {"name": AGENT_MAILBOX, "parentId": None}}}
    )
    box = (created["c0"].get("created") or {}).get("agent")
    if not box:
        raise JmapMethodError("notCreated")
    return str(box["id"])


def window_key(cursor: Cursor | None) -> str:
    """Names the digest window that starts at this cursor."""
    start = json.dumps([cursor.state, cursor.at] if cursor else None)
    return hashlib.sha256(start.encode()).hexdigest()[:16]


async def deliver_digest(jmap: JmapClient, text: str, now: datetime, window: str) -> str:
    """Create the digest as an unread email in the Agent mailbox (A2), or
    return the one an earlier attempt for this window created."""
    mailbox = await _agent_mailbox(jmap)
    keyword = DIGEST_KEYWORD.format(window)
    found = await jmap.call(
        [("Email/query", {"filter": {"inMailbox": mailbox, "hasKeyword": keyword}, "limit": 1})]
    )
    if found["c0"]["ids"]:
        return str(found["c0"]["ids"][0])
    email = {
        "mailboxIds": {mailbox: True},
        "keywords": {keyword: True},
        "from": [DIGEST_SENDER],
        "to": [DIGEST_SENDER],
        "subject": f"Mail digest {now.astimezone(UTC):%Y-%m-%d}",
        "bodyValues": {"1": {"value": text}},
        "textBody": [{"partId": "1", "type": "text/plain"}],
    }
    out = await jmap.write(EMAIL_SET, {"create": {"digest": email}})
    created = (out["c0"].get("created") or {}).get("digest")
    if not created:
        raise JmapMethodError("notCreated")
    return str(created["id"])


async def run_digest(
    jmap: JmapClient,
    model: BaseChatModel,
    table: AgentTable,
    *,
    deliver: str = "return",
    peek: bool = False,
    now: datetime | None = None,
) -> dict[str, Any]:
    now = now or datetime.now(UTC)
    cursor = await table.get_cursor()
    key = window_key(cursor)
    if deliver == "mail" and not peek:
        stamp = int(now.timestamp())
        if not await table.claim_digest(key, stamp, stamp + int(DIGEST_LEASE.total_seconds())):
            return {"status": "error", "error": "digest_in_progress"}
    window = await _changes(jmap, cursor, now)
    boxes = await mailbox_index(jmap)
    roles = boxes.roles
    # Drafts, sent and trash are not news, nor are the agent's own digests.
    skip = {roles[r] for r in SKIP_ROLES if r in roles}
    if AGENT_MAILBOX in boxes.top_level:
        skip.add(boxes.top_level[AGENT_MAILBOX])
    found = await _headers(jmap, window.ids, HEADER_PROPERTIES)
    by_id = {e["id"]: e for e in found}
    # The server's order is creation order; destroyed ids drop out here.
    emails = [
        by_id[i]
        for i in window.ids
        if i in by_id and not set(by_id[i].get("mailboxIds") or {}) & skip
    ]
    rows = await table.get_triage(e["id"] for e in emails)
    reminders = True
    try:
        waiting = await _waiting(jmap, table, now, {e["id"] for e in emails}, roles.get("sent"))
    except JmapMethodError as exc:
        # New mail still goes out; the reminders come back next time.
        log.warning(json.dumps({"event": "digest_reminders_failed", "error": exc.type}))
        waiting, reminders = [], False
    meter = Meter(max_calls=DIGEST_MODEL_CALLS, max_tokens=DIGEST_MAX_TOKENS)
    try:
        overview = await _overview(model, meter, emails, rows, waiting)
    except BudgetExceeded:
        overview = None
    except Exception as exc:
        # The overview is optional: a model failure leaves it out, nothing more.
        log.warning(json.dumps({"event": "digest_overview_failed", "error": type(exc).__name__}))
        overview = None
    since = cursor.at if cursor else None
    text = render(now, since, emails, rows, waiting, window.more, overview, reminders)
    result: dict[str, Any] = {
        "status": "ok",
        "emails": len(emails),
        "needs_reply": sum(1 for e in emails if (r := rows.get(e["id"])) and r.needs_reply),
        "waiting": len(waiting),
        "triaged": len(rows),
        "overview": overview is not None,
        "model_calls": meter.calls,
        "tokens": meter.tokens,
    }
    if deliver == "mail":
        result["email_id"] = await deliver_digest(jmap, text, now, key)
    else:
        result["digest"] = text
    if not peek:
        # Only over the cursor this run read: a run finishing after a newer
        # one leaves the newer cursor in place.
        moved = await table.set_cursor(Cursor(window.state, window.at or _iso(now)), cursor)
        if not moved:
            result["cursor"] = "already_advanced"
    return result
