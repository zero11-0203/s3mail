"""Reply drafts (A2, docs/specs/agent.md#features).

read the email and its thread (code) -> write the body (model)
  -> save the draft in Drafts (code, guarded)

The model writes only the body text. Code picks the recipients, subject,
threading headers and sending identity from the email being answered, and
`save_draft` refuses anything that is not solely in the Drafts mailbox with
$draft set. The owner reviews, edits and sends the draft from aerc, or
approves sending it (A3). Revising creates a new draft: no tool destroys
an Email, so the earlier draft stays for the owner to discard.
"""

from __future__ import annotations

import re
from typing import Any

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import HumanMessage, SystemMessage

from .jmap import EMAIL_SET, GuardError, JmapClient, JmapMethodError
from .metering import Meter
from .preferences import Preference
from .prompts import DRAFT_SYSTEM
from .tools import HEADER_PROPERTIES, body_text, envelope, header_of, mailbox_index

MAX_INSTRUCTIONS_CHARS = 2000
MAX_DRAFT_CHARS = 8000
DRAFT_CONTEXT_EMAILS = 10
DRAFT_BODY_BYTES = 6 * 1024
DRAFT_MODEL_CALLS = 2
DRAFT_MAX_TOKENS = 60_000
EMAIL_ID = re.compile(r"[A-Za-z0-9_-]{1,255}")
REPLY_PROPERTIES = [
    *HEADER_PROPERTIES,
    "cc",
    "replyTo",
    "messageId",
    "references",
    "textBody",
    "bodyValues",
]


class DraftError(Exception):
    def __init__(self, code: str) -> None:
        super().__init__(code)
        self.code = code


def guard_draft(email: dict[str, Any], drafts_id: str) -> None:
    """save_draft's guard: solely in Drafts, $draft and $seen, no other keyword."""
    if email.get("mailboxIds") != {drafts_id: True}:
        raise GuardError("a draft goes only into the Drafts mailbox")
    if email.get("keywords") != {"$draft": True, "$seen": True}:
        raise GuardError("a draft carries exactly $draft and $seen")


async def save_draft(jmap: JmapClient, email: dict[str, Any], drafts_id: str) -> str:
    guard_draft(email, drafts_id)
    out = await jmap.write(EMAIL_SET, {"create": {"draft": email}})
    created = (out["c0"].get("created") or {}).get("draft")
    if not created:
        raise JmapMethodError("notCreated")
    return str(created["id"])


def _subject(subject: str | None) -> str:
    subject = (subject or "").strip()
    return subject if re.match(r"(?i)re:", subject) else f"Re: {subject}".strip()


def _owner_addresses(identities: list[dict[str, Any]]) -> set[str]:
    return {i["email"].lower() for i in identities if i.get("email")}


def _identity(email: dict[str, Any], identities: list[dict[str, Any]]) -> dict[str, Any] | None:
    """The identity the email was sent to, else the first one."""
    addressed = {
        a.get("email", "").lower() for a in (email.get("to") or []) + (email.get("cc") or [])
    }
    for identity in identities:
        if identity.get("email", "").lower() in addressed:
            return identity
    return identities[0] if identities else None


def _recipients(
    email: dict[str, Any], own: set[str], reply_all: bool
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    to = [
        {"name": a.get("name"), "email": a["email"]}
        for a in (email.get("replyTo") or email.get("from") or [])
        if a.get("email")
    ]
    cc: list[dict[str, Any]] = []
    if reply_all:
        seen = {a["email"].lower() for a in to} | own
        for a in (email.get("to") or []) + (email.get("cc") or []):
            address = a.get("email", "")
            if address and address.lower() not in seen:
                cc.append({"name": a.get("name"), "email": address})
                seen.add(address.lower())
    return to, cc


def _clean_body(text: str) -> str:
    body = text.strip()
    fence = re.fullmatch(r"```(?:\w+)?\s*(.*?)\s*```", body, re.DOTALL)
    if fence:
        body = fence.group(1)
    return re.sub(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]", "", body)


async def run_draft(
    jmap: JmapClient,
    model: BaseChatModel,
    *,
    email_id: str,
    instructions: str,
    preferences: list[Preference],
    reply_all: bool = False,
) -> dict[str, Any]:
    if not EMAIL_ID.fullmatch(email_id) or len(instructions) > MAX_INSTRUCTIONS_CHARS:
        raise DraftError("bad_request")
    session = await jmap.session()
    out = await jmap.call(
        [
            (
                "Email/get",
                {
                    "ids": [email_id],
                    "properties": REPLY_PROPERTIES,
                    "fetchTextBodyValues": True,
                    "maxBodyValueBytes": DRAFT_BODY_BYTES,
                },
            )
        ]
    )
    if not out["c0"]["list"]:
        raise DraftError("not_found")
    target = out["c0"]["list"][0]
    identities: list[dict[str, Any]] = []
    if session.submission:
        identities = (await jmap.call([("Identity/get", {"ids": None})]))["c0"]["list"]
    boxes = await mailbox_index(jmap)
    if "drafts" not in boxes.roles:
        raise DraftError("no_drafts_mailbox")

    # Context: the newest emails of this thread only, never other threads.
    thread = await jmap.call([("Thread/get", {"ids": [target["threadId"]]})])
    members = thread["c0"]["list"][0]["emailIds"] if thread["c0"]["list"] else [email_id]
    window = members[-DRAFT_CONTEXT_EMAILS:]
    if email_id not in window:
        window = [*window[1:], email_id]
    got = await jmap.call(
        [
            (
                "Email/get",
                {
                    "ids": window,
                    "properties": [*HEADER_PROPERTIES, "textBody", "bodyValues"],
                    "fetchTextBodyValues": True,
                    "maxBodyValueBytes": DRAFT_BODY_BYTES,
                },
            )
        ]
    )
    context = []
    for e in sorted(got["c0"]["list"], key=lambda e: e.get("receivedAt") or ""):
        body, truncated = body_text(e, DRAFT_BODY_BYTES)
        item = {**header_of(e), "body": body, "truncated": truncated}
        if e["id"] == email_id:
            item["reply_to"] = True
        context.append(item)

    owner_lines = [f"- {p.text}" for p in preferences]
    owner = "Owner preferences:\n" + ("\n".join(owner_lines) if owner_lines else "- (none)")
    owner += f"\n\nOwner instructions for this reply:\n{instructions.strip() or '(none)'}"
    meter = Meter(max_calls=DRAFT_MODEL_CALLS, max_tokens=DRAFT_MAX_TOKENS)
    messages: list[Any] = [
        SystemMessage(content=DRAFT_SYSTEM),
        HumanMessage(content=envelope({"thread": context})),
        HumanMessage(content=owner),
    ]
    body = ""
    while not body:
        ai = await meter.invoke(model, messages)
        body = _clean_body(ai.text)
        if len(body) > MAX_DRAFT_CHARS:
            body = ""
        if not body:
            if meter.calls >= meter.max_calls:
                raise DraftError("draft_failed")
            messages = [*messages, ai, HumanMessage(content="Write the reply body as plain text.")]

    identity = _identity(target, identities)
    sender = identity or (target.get("to") or [{}])[0]
    if not sender.get("email"):
        raise DraftError("no_identity")
    own = _owner_addresses(identities) | {sender["email"].lower()}
    to, cc = _recipients(target, own, reply_all)
    if not to:
        raise DraftError("no_recipient")
    message_ids = target.get("messageId") or []
    references = [*(target.get("references") or []), *message_ids]
    draft: dict[str, Any] = {
        "mailboxIds": {boxes.roles["drafts"]: True},
        "keywords": {"$draft": True, "$seen": True},
        "from": [{"name": sender.get("name"), "email": sender["email"]}],
        "to": to,
        "subject": _subject(target.get("subject")),
        "bodyValues": {"1": {"value": body}},
        "textBody": [{"partId": "1", "type": "text/plain"}],
    }
    if cc:
        draft["cc"] = cc
    if message_ids:
        draft["inReplyTo"] = message_ids
        draft["references"] = references
    draft_id = await save_draft(jmap, draft, boxes.roles["drafts"])
    return {
        "status": "ok",
        "draft_id": draft_id,
        "draft": {
            "from": sender["email"],
            "to": [a["email"] for a in to],
            "cc": [a["email"] for a in cc],
            "subject": draft["subject"],
            "body": body,
        },
        "model_calls": meter.calls,
        "tokens": meter.tokens,
    }
