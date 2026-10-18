"""The daily digest (A1; delivered as an email from A2; reminders from A3).

changes since cursor (code) -> headers and triage rows (code)
  -> waiting threads (code) -> overview (model, cited) -> render (code)
  -> deliver: return to the CLI, or create the digest email (A2)

Code decides what the digest lists; the model writes only a short overview
whose citations must be emails in this digest. Ids are re-read with
Email/get, so destroyed mail never appears. The cursor advances only after
the digest was delivered.
"""

from __future__ import annotations

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
DIGEST_MODEL_CALLS = 2
DIGEST_MAX_TOKENS = 60_000
# Without a cursor (first digest or a lost table), cover the last day.
FIRST_WINDOW = timedelta(days=1)
# Threads waiting on the owner: needs-reply rows this old, up to this age.
REMIND_AFTER = timedelta(days=2)
REMIND_WITHIN = timedelta(days=21)
MAX_REMINDERS = 20
AGENT_MAILBOX = "Agent"
DIGEST_SENDER = {"name": "s3mail agent", "email": "agent@s3mail.invalid"}
# Mailboxes whose new emails are not news to the owner.
SKIP_ROLES = frozenset({"drafts", "sent", "trash"})


def _iso(t: datetime) -> str:
    return t.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


@dataclass(frozen=True)
class Window:
    ids: list[str]
    more: int
    state: str


async def _changes(jmap: JmapClient, cursor: Cursor | None, now: datetime) -> Window:
    """Created email ids since the cursor, oldest first, and the new state."""
    if cursor is not None:
        created: list[str] = []
        since = cursor.state
        try:
            while True:
                out = await jmap.call(
                    [("Email/changes", {"sinceState": since, "maxChanges": CHANGES_PAGE})]
                )
                page = out["c0"]
                created.extend(page.get("created", []))
                since = page["newState"]
                if not page.get("hasMoreChanges"):
                    break
            return Window(created[:MAX_EMAILS], max(len(created) - MAX_EMAILS, 0), since)
        except JmapMethodError as exc:
            if exc.type not in ("cannotCalculateChanges", "invalidArguments"):
                raise
        after = cursor.at
    else:
        after = _iso(now - FIRST_WINDOW)
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
    return Window(ids, max(total - len(ids), 0), out["c0"]["state"])


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
    ]
    if not rows:
        return []
    threads = await jmap.call(
        [("Thread/get", {"ids": list(dict.fromkeys(r.thread_id for r in rows))})]
    )
    members = {t["id"]: t["emailIds"] for t in threads["c0"]["list"]}
    ids = list(dict.fromkeys(i for r in rows for i in members.get(r.thread_id, [])))
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
    more: int,
    overview: str | None,
) -> str:
    lines = [f"Mail digest, {now.astimezone(UTC):%Y-%m-%d %H:%M} UTC"]
    lines.append(
        f"New since {since}: {len(emails) + more}" if since else f"New: {len(emails) + more}"
    )
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
    if more:
        lines += ["", f"…and {more} more not listed."]
    if waiting:
        lines += ["", "Still waiting on you:"]
        lines += [_line(e, r) for r, e in waiting]
    if not emails and not more:
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


async def deliver_digest(jmap: JmapClient, text: str, now: datetime) -> str:
    """Create the digest as an unread email in the Agent mailbox (A2)."""
    mailbox = await _agent_mailbox(jmap)
    email = {
        "mailboxIds": {mailbox: True},
        "keywords": {},
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
    waiting = await _waiting(jmap, table, now, {e["id"] for e in emails}, roles.get("sent"))
    meter = Meter(max_calls=DIGEST_MODEL_CALLS, max_tokens=DIGEST_MAX_TOKENS)
    try:
        overview = await _overview(model, meter, emails, rows, waiting)
    except BudgetExceeded:
        overview = None
    text = render(now, cursor.at if cursor else None, emails, rows, waiting, window.more, overview)
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
        result["email_id"] = await deliver_digest(jmap, text, now)
    else:
        result["digest"] = text
    if not peek:
        await table.set_cursor(Cursor(window.state, _iso(now)))
    return result
