"""A stateful fake JMAP server for the write phases. All mail is synthetic.

It follows s3mail's contract where the agent depends on it: Email/changes
from an "s<seq>" state (anything else is cannotCalculateChanges), Email/set
create into given mailboxes and patch updates of keywords and mailboxIds,
Mailbox/set create, Identity/get, EmailSubmission/set with
onSuccessUpdateEmail answered as an implicit Email/set, and downloads.
Any destroy fails the test.
"""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import Any

import httpx

from s3mail_agent.jmap import CORE, MAIL, SUBMISSION

from .conftest import ACCOUNT, API_URL, SESSION_URL, FakeJmap, MethodError

DOWNLOAD_URL = "https://jmap.example.com/download/{accountId}/{blobId}/{name}"
OWNER = "owner@example.com"


def message(
    id: str,
    thread: str,
    sender: str,
    subject: str,
    received: str,
    body: str,
    *,
    mailbox: str = "inbox",
    keywords: dict[str, bool] | None = None,
    to: str = OWNER,
    message_id: str | None = None,
    attachments: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    return {
        "id": id,
        "threadId": thread,
        "mailboxIds": {mailbox: True},
        "keywords": dict(keywords or {}),
        "from": [{"name": None, "email": sender}],
        "to": [{"name": None, "email": to}],
        "cc": [],
        "replyTo": None,
        "subject": subject,
        "receivedAt": received,
        "sentAt": received,
        "messageId": [message_id or f"{id}@example.net"],
        "inReplyTo": None,
        "references": None,
        "preview": body[:60],
        "textBody": [{"partId": "1", "type": "text/plain"}],
        "body": body,
        "blobId": f"G{id}",
        "attachments": attachments or [],
    }


class MailServer(FakeJmap):
    def __init__(
        self, mailbox: list[dict[str, Any]] | None = None, *, submission: bool = True
    ) -> None:
        super().__init__(list(mailbox or []))
        self.mailboxes: list[dict[str, Any]] = [
            {"id": "inbox", "name": "Inbox", "role": "inbox", "parentId": None},
            {"id": "archive", "name": "Archive", "role": "archive", "parentId": None},
            {"id": "drafts", "name": "Drafts", "role": "drafts", "parentId": None},
            {"id": "sent", "name": "Sent", "role": "sent", "parentId": None},
            {"id": "junk", "name": "Junk", "role": "junk", "parentId": None},
            {"id": "trash", "name": "Trash", "role": "trash", "parentId": None},
        ]
        self.identities = [{"id": "I1", "name": "Owner", "email": OWNER}]
        self.submissions: list[dict[str, Any]] = []
        self.blobs: dict[str, bytes] = {}
        self.seq = 0
        self.log: list[tuple[int, str]] = []  # (seq, created email id)
        self.writes: list[tuple[str, dict[str, Any]]] = []
        self.fail_write: str | None = None  # "transport" or a method error type
        self.after_update: Callable[[str], None] | None = None  # after each email's commit
        self.submission = submission
        self.capabilities = {
            CORE: {"maxObjectsInGet": 50, "maxCallsInRequest": 16, "maxObjectsInSet": 16},
            MAIL: {},
            **({SUBMISSION: {}} if submission else {}),
        }

    # --- helpers for tests -------------------------------------------------

    def deliver(self, email: dict[str, Any]) -> None:
        self.mailbox.append(email)
        self.seq += 1
        self.log.append((self.seq, email["id"]))

    def by_id(self, id: str) -> dict[str, Any] | None:
        return next((e for e in self.mailbox if e["id"] == id), None)

    def touch(self) -> None:
        """Advance the Email state, as any change by another client does."""
        self.seq += 1

    # --- transport -----------------------------------------------------------

    def handle(self, request: httpx.Request) -> httpx.Response:
        if request.method == "GET" and str(request.url) == SESSION_URL:
            caps: dict[str, Any] = {MAIL: {}, **({SUBMISSION: {}} if self.submission else {})}
            return httpx.Response(
                200,
                json={
                    "capabilities": self.capabilities,
                    "accounts": {ACCOUNT: {"accountCapabilities": caps}},
                    "primaryAccounts": {MAIL: ACCOUNT},
                    "apiUrl": API_URL,
                    "downloadUrl": DOWNLOAD_URL,
                },
            )
        if request.method == "GET" and str(request.url).startswith(
            "https://jmap.example.com/download/"
        ):
            blob = request.url.path.split("/")[3]
            if blob not in self.blobs:
                return httpx.Response(404)
            return httpx.Response(200, content=self.blobs[blob])
        if request.method == "POST" and self.fail_write == "transport":
            body = json.loads(request.content)
            if any(
                not n.endswith(("/get", "/query", "/changes")) for n, _, _ in body["methodCalls"]
            ):
                for name, args, _ in body["methodCalls"]:
                    self.method(name, args)  # applied, then the connection drops
                raise httpx.ReadTimeout("dropped after send")
        return super().handle(request)

    # --- methods ---------------------------------------------------------------

    def method(self, name: str, args: dict[str, Any]) -> Any:
        assert args["accountId"] == ACCOUNT
        assert "destroy" not in args, "the agent sent destroy"
        assert "onSuccessDestroyEmail" not in args
        if name.endswith("/set"):
            self.writes.append((name, args))
            if self.fail_write and self.fail_write != "transport":
                raise MethodError(self.fail_write)
        if name == "Email/get" and args.get("ids") == []:
            return {"list": [], "notFound": [], "state": f"s{self.seq}"}
        if name == "Email/get":
            out = super().method(name, args)
            out["state"] = f"s{self.seq}"
            return out
        if name == "Email/changes":
            since = args["sinceState"]
            if not since.startswith("s") or not since[1:].isdigit():
                raise MethodError("cannotCalculateChanges")
            start = int(since[1:])
            created = [i for s, i in self.log if s > start]
            limit = args.get("maxChanges") or len(created) or 1
            page = created[:limit]
            more = len(created) > limit
            new = self.seq if not more else next(s for s, i in self.log if i == page[-1])
            return {
                "oldState": since,
                "newState": f"s{new}",
                "hasMoreChanges": more,
                "created": page,
                "updated": [],
                "destroyed": [],
            }
        if name == "Mailbox/get":
            return {"list": self.mailboxes, "notFound": [], "state": "m1"}
        if name == "Mailbox/set":
            made: dict[str, Any] = {}
            for key, box in args.get("create", {}).items():
                mailbox = {"id": f"MB{len(self.mailboxes)}", "role": None, **box}
                self.mailboxes.append(mailbox)
                made[key] = {"id": mailbox["id"]}
            return {"created": made, "notCreated": None}
        if name == "Email/set":
            if args.get("ifInState") not in (None, f"s{self.seq}"):
                raise MethodError("stateMismatch")
            return self._email_set(args)
        if name == "Identity/get":
            assert self.submission
            return {"list": self.identities, "notFound": [], "state": "i1"}
        if name == "EmailSubmission/set":
            return self._submission_set(args)
        if name == "EmailSubmission/query":
            ids = set(args.get("filter", {}).get("emailIds", []))
            found = [s["id"] for s in self.submissions if s["emailId"] in ids]
            return {"ids": found, "total": len(found), "queryState": "sq1", "position": 0}
        if name == "EmailSubmission/get":
            return {"list": [s for s in self.submissions if s["id"] in args["ids"]], "notFound": []}
        return super().method(name, args)

    def _email_set(self, args: dict[str, Any]) -> dict[str, Any]:
        created: dict[str, Any] = {}
        updated: dict[str, None] = {}
        known = {b["id"] for b in self.mailboxes}
        for key, spec in (args.get("create") or {}).items():
            assert set(spec["mailboxIds"]) <= known
            id = f"D{len(self.mailbox) + 1}"
            body = spec["bodyValues"][spec["textBody"][0]["partId"]]["value"]
            email = {
                "id": id,
                "threadId": f"T{id}",
                "mailboxIds": dict(spec["mailboxIds"]),
                "keywords": dict(spec.get("keywords", {})),
                "from": spec.get("from"),
                "to": spec.get("to"),
                "cc": spec.get("cc", []),
                "replyTo": None,
                "subject": spec.get("subject"),
                "receivedAt": "2026-10-18T12:00:00Z",
                "sentAt": "2026-10-18T12:00:00Z",
                "messageId": [f"{id}@example.com"],
                "inReplyTo": spec.get("inReplyTo"),
                "references": spec.get("references"),
                "preview": body[:60],
                "textBody": [{"partId": "1", "type": "text/plain"}],
                "body": body,
                "blobId": f"G{id}",
                "attachments": [],
            }
            self.deliver(email)
            created[key] = {"id": id, "threadId": email["threadId"], "blobId": email["blobId"]}
        # As in s3mail, each email commits on its own, and an ifInState
        # holds every commit: once another writer moves the state, the
        # emails not yet reached are refused one by one.
        guarded = args.get("ifInState") is not None
        accepted = self.seq
        refused: dict[str, Any] = {}
        for id, patch in (args.get("update") or {}).items():
            if guarded and self.seq > accepted:
                refused[id] = {"type": "stateMismatch"}
                continue
            target = self.by_id(id)
            assert target is not None, id
            for path, value in patch.items():
                field, _, key = path.partition("/")
                assert field in ("keywords", "mailboxIds"), path
                if value is None:
                    target[field].pop(key, None)
                else:
                    target[field][key] = value
            assert target["mailboxIds"], "an email keeps at least one mailbox"
            updated[id] = None
            self.touch()
            accepted = self.seq
            if self.after_update:
                self.after_update(id)
        return {
            "created": created or None,
            "updated": updated or None,
            "notUpdated": refused or None,
            "newState": f"s{accepted}",  # the call's own last commit
        }

    def _submission_set(self, args: dict[str, Any]) -> Any:
        created = {}
        for key, spec in args.get("create", {}).items():
            email = self.by_id(spec["emailId"])
            assert email is not None and "$draft" in email["keywords"]
            identity = next(i for i in self.identities if i["id"] == spec["identityId"])
            assert email["from"][0]["email"] == identity["email"], "forbiddenFrom"
            sub = {
                "id": f"S{len(self.submissions) + 1}",
                "emailId": email["id"],
                "undoStatus": "final",
            }
            self.submissions.append(sub)
            created[key] = {"id": sub["id"]}
        result: dict[str, Any] = {"created": created, "notCreated": None}
        hooks = args.get("onSuccessUpdateEmail") or {}
        if hooks:
            update = {}
            for ref, patch in hooks.items():
                sub_id = created[ref.lstrip("#")]["id"]
                email_id = next(s["emailId"] for s in self.submissions if s["id"] == sub_id)
                update[email_id] = patch
            return [
                ("EmailSubmission/set", result),
                ("Email/set", self._email_set({"update": update})),
            ]
        return result
