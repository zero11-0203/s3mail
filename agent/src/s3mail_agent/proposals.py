"""Approved actions (A3, docs/specs/agent.md#langgraph-design).

The model may only propose. Code turns a proposal into the exact JMAP calls,
renders them for the owner and hashes them; the executor runs them only for
an approval carrying that hash, within 24 hours, at most once.

- propose_update: keywords ($flagged, $seen) and mailbox moves of emails
  found in this run. Never into Trash, Drafts or Sent, never a destroy.
- propose_send: one existing draft, solely in Drafts with $draft, from one
  of the owner's identities. The send-once marker, keyed by the approval
  hash, is written before EmailSubmission/set; a second attempt with the
  same approval reports the earlier state instead of sending, and an
  unknown result is reported, never retried.
"""

from __future__ import annotations

import hashlib
import json
import uuid
from datetime import UTC, datetime, timedelta
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from .jmap import EMAIL_SET, SUBMISSION_SET, JmapClient, JmapMethodError, JmapUnknownResult
from .store import AgentTable, SendMarker
from .tools import HEADER_PROPERTIES, MailboxIndex, body_text, header_of, mailbox_index

APPROVAL_TTL = timedelta(hours=24)
MAX_UPDATE_EMAILS = 50
SEND_PREVIEW_CHARS = 4000
FORBIDDEN_TARGETS = frozenset({"trash", "drafts", "sent"})


class _Args(BaseModel):
    model_config = ConfigDict(extra="forbid")


class propose_update(_Args):
    """Propose flagging, marking read or unread, archiving or moving emails found
    in this run. The owner approves or rejects the exact change."""

    email_ids: list[str] = Field(min_length=1, max_length=MAX_UPDATE_EMAILS)
    flagged: bool | None = Field(None, description="true to flag, false to unflag.")
    seen: bool | None = Field(None, description="true to mark read, false to mark unread.")
    move_to: str | None = Field(
        None, description="Target mailbox: a role such as archive, inbox or junk, or a name."
    )


class propose_send(_Args):
    """Propose sending one existing draft from the Drafts mailbox. The owner sees
    the exact message and approves or rejects it."""

    draft_id: str


class ProposalError(Exception):
    """A proposal the code refuses; the model sees the reason."""


def _now() -> datetime:
    return datetime.now(UTC)


def proposal_hash(kind: str, calls: list[Any], nonce: str) -> str:
    canonical = json.dumps(
        {"kind": kind, "calls": calls, "nonce": nonce}, sort_keys=True, separators=(",", ":")
    )
    return hashlib.sha256(canonical.encode()).hexdigest()


def _finish(kind: str, calls: list[Any], summary: str, extra: dict[str, Any]) -> dict[str, Any]:
    created = _now()
    nonce = uuid.uuid4().hex
    return {
        "kind": kind,
        "calls": calls,
        "summary": summary,
        "nonce": nonce,
        "created_at": created.isoformat(),
        "expires_at": (created + APPROVAL_TTL).isoformat(),
        "hash": proposal_hash(kind, calls, nonce),
        **extra,
    }


def _target(boxes: MailboxIndex, move_to: str) -> str:
    wanted = move_to.strip()
    box = boxes.roles.get(wanted.lower()) or boxes.top_level.get(wanted)
    if box is None:
        box = next((i for i, n in boxes.names.items() if n.lower() == wanted.lower()), None)
    if box is None:
        raise ProposalError(f"no mailbox {wanted!r}")
    role = next((r for r, i in boxes.roles.items() if i == box), None)
    if role in FORBIDDEN_TARGETS:
        raise ProposalError(f"moving to {role} is not allowed")
    return box


async def build_update(jmap: JmapClient, args: propose_update, fetched: set[str]) -> dict[str, Any]:
    ids = list(dict.fromkeys(args.email_ids))
    unknown = [i for i in ids if i not in fetched]
    if unknown:
        raise ProposalError("propose only emails a tool returned in this run")
    if args.flagged is None and args.seen is None and not args.move_to:
        raise ProposalError("the proposal changes nothing")
    boxes = await mailbox_index(jmap)
    target = _target(boxes, args.move_to) if args.move_to else None
    out = await jmap.call(
        [("Email/get", {"ids": ids, "properties": [*HEADER_PROPERTIES, "keywords"]})]
    )
    if out["c0"].get("notFound"):
        raise ProposalError("some emails no longer exist")
    patches: dict[str, dict[str, Any]] = {}
    lines: list[str] = []
    for email in out["c0"]["list"]:
        keywords = email.get("keywords") or {}
        current = set(email.get("mailboxIds") or {})
        patch: dict[str, Any] = {}
        changes: list[str] = []
        if args.flagged is not None and bool(keywords.get("$flagged")) != args.flagged:
            patch["keywords/$flagged"] = True if args.flagged else None
            changes.append("flag" if args.flagged else "unflag")
        if args.seen is not None and bool(keywords.get("$seen")) != args.seen:
            patch["keywords/$seen"] = True if args.seen else None
            changes.append("mark read" if args.seen else "mark unread")
        if target and current != {target}:
            for box in sorted(current - {target}):
                patch[f"mailboxIds/{box}"] = None
            patch[f"mailboxIds/{target}"] = True
            changes.append(f"move to {boxes.names.get(target, target)}")
        if patch:
            patches[email["id"]] = patch
            h = header_of(email)
            sender = h["from"][0] if h["from"] else "(no sender)"
            lines.append(
                f"- {', '.join(changes)}: {h['subject'] or '(no subject)'} — {sender} "
                f"[email:{email['id']}]"
            )
    if not patches:
        raise ProposalError("the emails already look like that")
    session = await jmap.session()
    items = sorted(patches.items())
    size = max(session.max_objects_in_set, 1)
    calls = [[EMAIL_SET, {"update": dict(items[i : i + size])}] for i in range(0, len(items), size)]
    summary = f"Change {len(patches)} email(s):\n" + "\n".join(lines)
    return _finish("update", calls, summary, {"emails": [i for i, _ in items]})


async def build_send(jmap: JmapClient, args: propose_send) -> dict[str, Any]:
    session = await jmap.session()
    if not session.submission:
        raise ProposalError("sending is not configured on the server")
    boxes = await mailbox_index(jmap)
    drafts, sent = boxes.roles.get("drafts"), boxes.roles.get("sent")
    if not drafts or not sent:
        raise ProposalError("the server has no Drafts or Sent mailbox")
    out = await jmap.call(
        [
            (
                "Email/get",
                {
                    "ids": [args.draft_id],
                    "properties": [
                        *HEADER_PROPERTIES,
                        "cc",
                        "bcc",
                        "keywords",
                        "blobId",
                        "textBody",
                        "bodyValues",
                    ],
                    "fetchTextBodyValues": True,
                    "maxBodyValueBytes": SEND_PREVIEW_CHARS * 4,
                },
            ),
            ("Identity/get", {"ids": None}),
        ]
    )
    if not out["c0"]["list"]:
        raise ProposalError("no such draft")
    draft = out["c0"]["list"][0]
    if "$draft" not in (draft.get("keywords") or {}) or draft.get("mailboxIds") != {drafts: True}:
        raise ProposalError("only a draft in the Drafts mailbox can be sent")
    sender = (draft.get("from") or [{}])[0].get("email", "").lower()
    identity = next((i for i in out["c1"]["list"] if i.get("email", "").lower() == sender), None)
    if identity is None:
        raise ProposalError("the draft's From is not one of the owner's identities")
    calls = [
        [
            SUBMISSION_SET,
            {
                "create": {"send": {"identityId": identity["id"], "emailId": draft["id"]}},
                "onSuccessUpdateEmail": {
                    "#send": {
                        "keywords/$draft": None,
                        f"mailboxIds/{drafts}": None,
                        f"mailboxIds/{sent}": True,
                    }
                },
            },
        ]
    ]
    h = header_of(draft)
    body, truncated = body_text(draft, SEND_PREVIEW_CHARS * 4)
    if len(body) > SEND_PREVIEW_CHARS:
        body, truncated = body[:SEND_PREVIEW_CHARS], True
    cc = [a.get("email", "") for a in draft.get("cc") or []]
    bcc = [a.get("email", "") for a in draft.get("bcc") or []]
    summary = "\n".join(
        [
            "Send this draft:",
            f"From: {identity['email']}",
            f"To: {', '.join(h['to']) or '(none)'}",
            *([f"Cc: {', '.join(cc)}"] if cc else []),
            *([f"Bcc: {', '.join(bcc)}"] if bcc else []),
            f"Subject: {h['subject'] or '(no subject)'}",
            "",
            body + ("\n[…truncated; read the whole draft in aerc]" if truncated else ""),
        ]
    )
    # The blob id pins the draft's exact content to the approval.
    return _finish(
        "send", calls, summary, {"emails": [draft["id"]], "blob_id": draft.get("blobId")}
    )


async def execute(
    jmap: JmapClient,
    table: AgentTable,
    proposal: dict[str, Any],
    decision: Any,
    now: datetime | None = None,
) -> dict[str, Any]:
    """Run an approved proposal once; anything else runs nothing."""
    now = now or _now()
    if not isinstance(decision, dict) or decision.get("approve") is not True:
        return {"outcome": "rejected"}
    if decision.get("hash") != proposal["hash"]:
        return {"outcome": "approval_mismatch"}
    if proposal_hash(proposal["kind"], proposal["calls"], proposal["nonce"]) != proposal["hash"]:
        return {"outcome": "approval_mismatch"}
    if now > datetime.fromisoformat(proposal["expires_at"]):
        return {"outcome": "expired"}
    if proposal["kind"] == "update":
        return await _execute_update(jmap, proposal)
    if proposal["kind"] == "send":
        return await _execute_send(jmap, table, proposal, now)
    return {"outcome": "approval_mismatch"}


async def _execute_update(jmap: JmapClient, proposal: dict[str, Any]) -> dict[str, Any]:
    updated: list[str] = []
    failed: list[str] = []
    for name, args in proposal["calls"]:
        try:
            out = await jmap.write(name, args)
        except JmapUnknownResult:
            return {"outcome": "unknown", "updated": updated, "failed": failed}
        except JmapMethodError as exc:
            return {"outcome": "failed", "error": exc.type, "updated": updated, "failed": failed}
        updated.extend((out["c0"].get("updated") or {}).keys())
        failed.extend((out["c0"].get("notUpdated") or {}).keys())
    return {"outcome": "done" if not failed else "partial", "updated": updated, "failed": failed}


async def _execute_send(
    jmap: JmapClient, table: AgentTable, proposal: dict[str, Any], now: datetime
) -> dict[str, Any]:
    draft_id = proposal["emails"][0]
    current = await jmap.call(
        [("Email/get", {"ids": [draft_id], "properties": ["id", "blobId", "keywords"]})]
    )
    found = current["c0"]["list"]
    marker = SendMarker(proposal["hash"], draft_id, now.isoformat())
    if not await table.put_send_marker(marker):
        # A re-run of an approval that already reached the send step.
        return await _report_existing(jmap, draft_id)
    if not found or found[0].get("blobId") != proposal["blob_id"]:
        return {"outcome": "draft_changed"}
    name, args = proposal["calls"][0]
    try:
        out = await jmap.write(name, args)
    except JmapUnknownResult:
        # Never retried (docs/specs/mail-flow.md#outbound-email).
        try:
            report = await _report_existing(jmap, draft_id)
        except Exception:
            report = {}
        return {**report, "outcome": "unknown"}
    except JmapMethodError as exc:
        return {"outcome": "failed", "error": exc.type}
    created = (out["c0"].get("created") or {}).get("send")
    if not created:
        errors = out["c0"].get("notCreated") or {}
        error = (errors.get("send") or {}).get("type", "notCreated")
        return {"outcome": "failed", "error": error}
    return {"outcome": "sent", "submission_id": created["id"]}


async def _report_existing(jmap: JmapClient, draft_id: str) -> dict[str, Any]:
    found = await jmap.call(
        [("EmailSubmission/query", {"filter": {"emailIds": [draft_id]}, "limit": 10})]
    )
    ids = found["c0"].get("ids") or []
    return {"outcome": "already_attempted", "submissions": ids}
