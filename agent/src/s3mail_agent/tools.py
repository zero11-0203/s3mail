"""Read-only mail tools (A0). Each is a typed function over JmapClient.

The model sees only the argument schemas below; it never supplies raw JMAP.
Every result is wrapped as untrusted data, and the ids of emails it returned
feed the citation check.
"""

from __future__ import annotations

import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from datetime import UTC, date, datetime, time
from html.parser import HTMLParser
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from .jmap import MAX_BODY_VALUE_BYTES, SEMANTIC_QUERY, JmapClient, Session, ref

HEADER_PROPERTIES = ["id", "threadId", "mailboxIds", "from", "to", "subject", "receivedAt"]
THREAD_BODY_BYTES = 4 * 1024
THREAD_MAX_EMAILS = 30

DATA_OPEN = '<mail_data untrusted="true">'
DATA_CLOSE = "</mail_data>"


class _Args(BaseModel):
    model_config = ConfigDict(extra="forbid")


class search_mail(_Args):
    """Search the mailbox. Returns matching emails (newest first) with headers and a preview."""

    text: str | None = Field(None, description="Words to match anywhere in the email.")
    from_: str | None = Field(None, alias="from", description="Sender address or name.")
    to: str | None = Field(None, description="Recipient address or name.")
    subject: str | None = Field(None, description="Words in the subject.")
    after: date | None = Field(None, description="Received on or after this date (YYYY-MM-DD).")
    before: date | None = Field(None, description="Received before this date (YYYY-MM-DD).")
    limit: int = Field(10, ge=1, le=25, description="Maximum emails to return.")


class read_email(_Args):
    """Read up to five emails in full: headers and plain-text body."""

    ids: list[str] = Field(min_length=1, max_length=5, description="Email ids to read.")


class read_thread(_Args):
    """Read a thread, oldest first, with shortened bodies: the newest 30 emails,
    or earlier ones with skip_latest."""

    thread_id: str = Field(description="The threadId of any email in the thread.")
    skip_latest: int = Field(
        0, ge=0, le=10_000, description="Skip this many of the newest emails, to read earlier ones."
    )


class list_mailboxes(_Args):
    """List mailboxes with their roles and email counts."""


@dataclass(frozen=True)
class ToolResult:
    content: str
    email_ids: frozenset[str]


Tool = Callable[[JmapClient, Any], Awaitable[ToolResult]]


class _TextExtractor(HTMLParser):
    _BLOCKS = frozenset({"p", "div", "br", "li", "tr", "h1", "h2", "h3", "h4", "blockquote"})
    _SKIP = frozenset({"script", "style", "head", "title"})

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []
        self._skip = 0

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag in self._SKIP:
            self._skip += 1
        elif tag in self._BLOCKS:
            self.parts.append("\n")
        elif tag == "a":
            # Links stay visible text; the agent never fetches them.
            href = dict(attrs).get("href")
            if href:
                self.parts.append(f" <{href}> ")

    def handle_endtag(self, tag: str) -> None:
        if tag in self._SKIP and self._skip:
            self._skip -= 1
        elif tag in self._BLOCKS:
            self.parts.append("\n")

    def handle_data(self, data: str) -> None:
        if not self._skip:
            self.parts.append(data)


def html_to_text(html: str) -> str:
    parser = _TextExtractor()
    parser.feed(html)
    parser.close()
    lines = (" ".join(line.split()) for line in "".join(parser.parts).splitlines())
    return "\n".join(line for line in lines if line)


def envelope(payload: Any) -> str:
    """Mark tool output as data. Escaping '<' keeps mail from closing the block."""
    body = json.dumps(payload, ensure_ascii=False, default=str).replace("<", "\\u003c")
    return f"{DATA_OPEN}\n{body}\n{DATA_CLOSE}"


def _addresses(value: list[dict[str, Any]] | None) -> list[str]:
    out = []
    for a in value or []:
        name, email = a.get("name"), a.get("email", "")
        out.append(f"{name} <{email}>" if name else email)
    return out


def header_of(email: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": email["id"],
        "threadId": email.get("threadId"),
        "from": _addresses(email.get("from")),
        "to": _addresses(email.get("to")),
        "subject": email.get("subject"),
        "receivedAt": email.get("receivedAt"),
    }


def body_text(email: dict[str, Any], cap: int) -> tuple[str, bool]:
    values = email.get("bodyValues", {})
    texts, truncated = [], False
    for part in email.get("textBody", []):
        value = values.get(part.get("partId"))
        if not value:
            continue
        text = value.get("value", "")
        if part.get("type") == "text/html":
            text = html_to_text(text)
        texts.append(text)
        truncated = truncated or bool(value.get("isTruncated"))
    body = "\n\n".join(texts)
    if len(body.encode()) > cap:
        body = body.encode()[:cap].decode(errors="ignore")
        truncated = True
    return body, truncated


def _utc(day: date) -> str:
    return datetime.combine(day, time(), UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


async def run_search_mail(client: JmapClient, args: search_mail) -> ToolResult:
    condition: dict[str, Any] = {}
    for key, value in (
        ("text", args.text),
        ("from", args.from_),
        ("to", args.to),
        ("subject", args.subject),
    ):
        if value:
            condition[key] = value
    if args.after:
        condition["after"] = _utc(args.after)
    if args.before:
        condition["before"] = _utc(args.before)
    session = await client.session()
    results = await client.call(
        [
            (
                "Email/query",
                {
                    "filter": condition or None,
                    "sort": [{"property": "receivedAt", "isAscending": False}],
                    # The dependent Email/get must fit the server's limit.
                    "limit": min(args.limit, session.max_objects_in_get),
                    "calculateTotal": True,
                },
            ),
            (
                "Email/get",
                {
                    "#ids": ref("c0", "Email/query", "/ids"),
                    "properties": [*HEADER_PROPERTIES, "preview"],
                },
            ),
        ]
    )
    emails = [{**header_of(e), "preview": e.get("preview")} for e in results["c1"]["list"]]
    payload = {"total": results["c0"].get("total"), "emails": emails}
    return ToolResult(envelope(payload), frozenset(e["id"] for e in emails))


async def run_read_email(client: JmapClient, args: read_email) -> ToolResult:
    results = await client.call(
        [
            (
                "Email/get",
                {
                    "ids": args.ids,
                    "properties": [*HEADER_PROPERTIES, "textBody", "bodyValues", "attachments"],
                    "fetchTextBodyValues": True,
                    "maxBodyValueBytes": MAX_BODY_VALUE_BYTES,
                },
            )
        ]
    )
    emails = []
    for email in results["c0"]["list"]:
        body, truncated = body_text(email, MAX_BODY_VALUE_BYTES)
        item = {**header_of(email), "body": body, "truncated": truncated}
        attachments = email.get("attachments") or []
        if attachments:
            # Metadata only; read_attachment (A4) reads one by its index.
            item["attachments"] = [
                {"index": i, "name": a.get("name"), "type": a.get("type"), "size": a.get("size")}
                for i, a in enumerate(attachments)
            ]
        emails.append(item)
    payload = {"emails": emails, "notFound": results["c0"].get("notFound", [])}
    return ToolResult(envelope(payload), frozenset(e["id"] for e in emails))


async def run_read_thread(client: JmapClient, args: read_thread) -> ToolResult:
    # Membership first, then only the chosen window, in batches the server
    # accepts: a long thread never becomes one oversized Email/get.
    threads = await client.call([("Thread/get", {"ids": [args.thread_id]})])
    found = threads["c0"].get("list", [])
    ids: list[str] = found[0]["emailIds"] if found else []
    end = max(len(ids) - args.skip_latest, 0)
    window = ids[max(end - THREAD_MAX_EMAILS, 0) : end]
    session = await client.session()
    fetched: list[dict[str, Any]] = []
    not_found: list[str] = []
    for i in range(0, len(window), session.max_objects_in_get):
        results = await client.call(
            [
                (
                    "Email/get",
                    {
                        "ids": window[i : i + session.max_objects_in_get],
                        "properties": [*HEADER_PROPERTIES, "textBody", "bodyValues"],
                        "fetchTextBodyValues": True,
                        "maxBodyValueBytes": THREAD_BODY_BYTES,
                    },
                )
            ]
        )
        fetched.extend(results["c0"]["list"])
        not_found.extend(results["c0"].get("notFound", []))
    emails = []
    for email in sorted(fetched, key=lambda e: e.get("receivedAt") or ""):
        body, truncated = body_text(email, THREAD_BODY_BYTES)
        emails.append({**header_of(email), "body": body, "truncated": truncated})
    payload = {
        "threadId": args.thread_id,
        "threadSize": len(ids),
        "emails": emails,
        "omittedEarlier": max(end - THREAD_MAX_EMAILS, 0),
        "omittedLater": len(ids) - end,
        "notFound": [*threads["c0"].get("notFound", []), *not_found],
    }
    return ToolResult(envelope(payload), frozenset(e["id"] for e in emails))


async def run_list_mailboxes(client: JmapClient, args: list_mailboxes) -> ToolResult:
    results = await client.call(
        [
            (
                "Mailbox/get",
                {
                    "ids": None,
                    "properties": ["id", "name", "role", "totalEmails", "unreadEmails"],
                },
            )
        ]
    )
    return ToolResult(envelope({"mailboxes": results["c0"]["list"]}), frozenset())


@dataclass(frozen=True)
class MailboxIndex:
    roles: dict[str, str]  # role -> mailbox id
    top_level: dict[str, str]  # name -> id of mailboxes without a parent
    names: dict[str, str]  # id -> name


async def mailbox_index(client: JmapClient) -> MailboxIndex:
    out = await client.call(
        [("Mailbox/get", {"ids": None, "properties": ["id", "name", "role", "parentId"]})]
    )
    boxes = out["c0"]["list"]
    return MailboxIndex(
        roles={b["role"]: b["id"] for b in boxes if b.get("role")},
        top_level={b["name"]: b["id"] for b in boxes if not b.get("parentId")},
        names={b["id"]: b["name"] for b in boxes},
    )


@dataclass(frozen=True)
class ToolSpec:
    args: type[_Args]
    run: Tool


# The ask mode's fixed allowlist (docs/specs/agent.md#identity-and-authorization).
ASK_TOOLS: dict[str, ToolSpec] = {
    "search_mail": ToolSpec(search_mail, run_search_mail),
    "read_email": ToolSpec(read_email, run_read_email),
    "read_thread": ToolSpec(read_thread, run_read_thread),
    "list_mailboxes": ToolSpec(list_mailboxes, run_list_mailboxes),
}


class semantic_search(_Args):
    """Find emails by meaning rather than exact words, for example "trips I booked
    last spring". Returns emails (best match first) with headers and a preview."""

    query: str = Field(min_length=1, max_length=500, description="What to look for.")
    limit: int = Field(10, ge=1, le=25, description="Maximum emails to return.")


async def run_semantic_search(client: JmapClient, args: semantic_search) -> ToolResult:
    session = await client.session()
    results = await client.call(
        [
            (
                SEMANTIC_QUERY,
                {"text": args.query, "limit": min(args.limit, session.max_objects_in_get)},
            ),
            (
                "Email/get",
                {
                    "#ids": ref("c0", SEMANTIC_QUERY, "/ids"),
                    "properties": [*HEADER_PROPERTIES, "preview"],
                },
            ),
        ]
    )
    emails = [{**header_of(e), "preview": e.get("preview")} for e in results["c1"]["list"]]
    return ToolResult(envelope({"emails": emails}), frozenset(e["id"] for e in emails))


def read_tools(session: Session, features: frozenset[str]) -> dict[str, ToolSpec]:
    """The read tools of this session: A0's, plus the A4 tools the
    deployment enables (features) and, for semantic search, the server
    advertises."""
    from .attachments import read_attachment, run_read_attachment

    tools = dict(ASK_TOOLS)
    if "attachments" in features and session.download_url:
        tools["read_attachment"] = ToolSpec(read_attachment, run_read_attachment)
    if "semantic" in features and session.semantic:
        tools["semantic_search"] = ToolSpec(semantic_search, run_semantic_search)
    return tools
