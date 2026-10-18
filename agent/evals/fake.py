"""The test fake JMAP server, with s3mail's search semantics and write capture."""

from __future__ import annotations

import unicodedata
from typing import Any

from tests.conftest import ACCOUNT, FakeJmap

from .mailbox import MAILBOX_IDS

READ_METHODS = {"Email/query", "Email/get", "Thread/get", "Mailbox/get"}


def fold(text: str) -> str:
    """NFKC, case folding and diacritics removed, like the index tokenizer."""
    decomposed = unicodedata.normalize("NFKD", unicodedata.normalize("NFKC", text).casefold())
    return "".join(c for c in decomposed if not unicodedata.combining(c))


def matches(email: dict[str, Any], condition: dict[str, Any] | None) -> bool:
    # docs/specs/search.md: text is matched word by word; from, to and
    # subject are case-insensitive substrings of the header.
    if not condition:
        return True
    sender = " ".join(f"{a.get('name') or ''} {a['email']}" for a in email["from"])
    to = " ".join(f"{a.get('name') or ''} {a['email']}" for a in email["to"])
    if "text" in condition:
        hay = fold(f"{email['subject']} {email['body']} {sender} {to}")
        if not all(word in hay for word in fold(condition["text"]).split()):
            return False
    for key, value in (("from", sender), ("to", to), ("subject", email["subject"])):
        if key in condition and fold(condition[key]) not in fold(value):
            return False
    if "after" in condition and email["receivedAt"] < condition["after"]:
        return False
    return not ("before" in condition and email["receivedAt"] >= condition["before"])


class EvalJmap(FakeJmap):
    def __init__(self, mailbox: list[dict[str, Any]]) -> None:
        super().__init__(mailbox)
        self.writes: list[str] = []

    def read_ids(self) -> set[str]:
        """Emails whose bodies the agent fetched."""
        return {
            i for name, args in self.calls if name == "Email/get" for i in args.get("ids") or []
        }

    def method(self, name: str, args: dict[str, Any]) -> dict[str, Any]:
        if name not in READ_METHODS:
            self.writes.append(name)
            return {"type": "forbidden"}
        assert args["accountId"] == ACCOUNT
        if name == "Email/query":
            found = [e for e in self.mailbox if matches(e, args.get("filter"))]
            found.sort(key=lambda e: e["receivedAt"], reverse=True)
            ids = [e["id"] for e in found][: args.get("limit", 50)]
            return {"ids": ids, "total": len(found), "queryState": "q1", "position": 0}
        if name == "Mailbox/get":
            boxes = [
                {
                    "id": box,
                    "name": box.capitalize(),
                    "role": box,
                    "totalEmails": sum(box in e["mailboxIds"] for e in self.mailbox),
                    "unreadEmails": sum(
                        box in e["mailboxIds"] and "$seen" not in e["keywords"]
                        for e in self.mailbox
                    ),
                }
                for box in MAILBOX_IDS
            ]
            return {"list": boxes, "notFound": [], "state": "m1"}
        return super().method(name, args)
