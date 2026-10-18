from __future__ import annotations

from typing import Any

import pytest
from langchain_core.messages import AIMessage

from s3mail_agent.drafts import DraftError, guard_draft, run_draft
from s3mail_agent.jmap import EMAIL_SET, GuardError, JmapClient
from s3mail_agent.preferences import Preference

from .conftest import SESSION_URL, ScriptFactory
from .mailserver import OWNER, MailServer, message


def server(*, submission: bool = True) -> MailServer:
    s = MailServer(submission=submission)
    first = message(
        "M1", "T1", "friend@example.com", "Dinner?", "2026-10-16T09:00:00Z", "Dinner Friday?"
    )
    first["cc"] = [{"name": None, "email": "other@example.com"}, {"name": None, "email": OWNER}]
    s.deliver(first)
    s.deliver(
        message("M2", "T9", "stranger@example.net", "Unrelated", "2026-10-16T10:00:00Z", "Secret")
    )
    return s


def client(s: MailServer, writes: frozenset[str] = frozenset({EMAIL_SET})) -> JmapClient:
    return JmapClient(SESSION_URL, "t", transport=s.transport(), backoff=0, writes=writes)


async def draft(s: MailServer, model: Any, **kw: Any) -> dict[str, Any]:
    args: dict[str, Any] = {"email_id": "M1", "instructions": "", "preferences": []} | kw
    return await run_draft(client(s), model, **args)


async def test_draft_is_saved_only_in_drafts(scripted: ScriptFactory) -> None:
    s = server()
    model = scripted(AIMessage(content="Friday works. See you then."))
    result = await draft(s, model, instructions="say yes")
    saved = s.by_id(result["draft_id"])
    assert saved["mailboxIds"] == {"drafts": True}
    assert saved["keywords"] == {"$draft": True, "$seen": True}
    assert saved["to"] == [{"name": None, "email": "friend@example.com"}]
    assert saved["from"][0]["email"] == OWNER and saved["subject"] == "Re: Dinner?"
    assert saved["inReplyTo"] == ["M1@example.net"] and saved["references"] == ["M1@example.net"]
    assert saved["body"] == "Friday works. See you then."
    assert [n for n, _ in s.writes] == ["Email/set"]
    assert "create" in s.writes[0][1] and "update" not in s.writes[0][1]


async def test_context_is_this_thread_only_and_instructions_stay_outside_the_data(
    scripted: ScriptFactory,
) -> None:
    s = server()
    model = scripted(AIMessage(content="Yes."))
    prefs = [Preference("P1", "Sign off with 'Cheers, O.'", "2026-10-01")]
    await draft(s, model, instructions="decline politely", preferences=prefs)
    _, data, owner = model.seen[0]
    assert '<mail_data untrusted="true">' in data.content and "Secret" not in data.content
    assert '"reply_to": true' in data.content
    assert "decline politely" in owner.content and "Cheers, O." in owner.content
    assert "mail_data" not in owner.content


async def test_reply_all_copies_others_but_not_the_owner(scripted: ScriptFactory) -> None:
    s = server()
    result = await draft(s, scripted(AIMessage(content="Yes.")), reply_all=True)
    assert result["draft"]["cc"] == ["other@example.com"]


async def test_without_submission_the_addressed_owner_is_the_sender(
    scripted: ScriptFactory,
) -> None:
    s = server(submission=False)
    result = await draft(s, scripted(AIMessage(content="Yes.")))
    assert result["draft"]["from"] == OWNER
    assert not any(n == "Identity/get" for n, _ in s.calls)


async def test_empty_replies_are_retried_then_fail(scripted: ScriptFactory) -> None:
    s = server()
    with pytest.raises(DraftError, match="draft_failed"):
        await draft(s, scripted(AIMessage(content=" "), AIMessage(content="```\n```")))
    assert s.writes == []


@pytest.mark.parametrize("email_id", ["../x", "", "M1 M2"])
async def test_bad_ids_are_refused(scripted: ScriptFactory, email_id: str) -> None:
    with pytest.raises(DraftError, match="bad_request"):
        await draft(server(), scripted(), email_id=email_id)


async def test_unknown_email(scripted: ScriptFactory) -> None:
    with pytest.raises(DraftError, match="not_found"):
        await draft(server(), scripted(), email_id="M404")


@pytest.mark.parametrize(
    "email",
    [
        {"mailboxIds": {"inbox": True}, "keywords": {"$draft": True, "$seen": True}},
        {
            "mailboxIds": {"drafts": True, "inbox": True},
            "keywords": {"$draft": True, "$seen": True},
        },
        {"mailboxIds": {"drafts": True}, "keywords": {"$seen": True}},
        {
            "mailboxIds": {"drafts": True},
            "keywords": {"$draft": True, "$seen": True, "$flagged": True},
        },
    ],
)
def test_save_draft_guard(email: dict[str, Any]) -> None:
    with pytest.raises(GuardError):
        guard_draft(email, "drafts")


async def test_drafts_need_the_write_allowlist(scripted: ScriptFactory) -> None:
    s = server()
    with pytest.raises(GuardError):
        await run_draft(
            client(s, writes=frozenset()),
            scripted(AIMessage(content="Yes.")),
            email_id="M1",
            instructions="",
            preferences=[],
        )
    assert s.writes == []
