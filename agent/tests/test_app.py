from __future__ import annotations

import json
import logging
from dataclasses import replace
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver

from s3mail_agent.app import Handler, memory_saver
from s3mail_agent.config import Settings
from s3mail_agent.identity import AuthorizationRequired
from s3mail_agent.jmap import JmapClient
from s3mail_agent.preferences import MemoryPreferences
from s3mail_agent.store import MemoryTable

from .conftest import MAILBOX, SESSION_URL, FakeJmap, ScriptFactory, call, tool_turn
from .mailserver import MailServer, message

SETTINGS = Settings(
    region="us-east-1",
    jmap_session_url=SESSION_URL,
    oauth_provider="s3mail-agent-cognito",
    oauth_scopes=("s3mail/mail.read",),
    oauth_return_url="http://localhost:8765/callback",
    memory_id="mem-1",
    ask_model="openai/gpt-6.1-sol",
    model_base_url="https://openrouter.ai/api/v1",
    model_key_secret_arn="arn:aws:secretsmanager:us-east-1:111111111111:secret:model-key",  # noqa: S106 - an ARN, not a secret
)
SESSION = "cli-00000000-0000-4000-8000-000000000000"


STATE = "s" * 43


def handler(
    fake: FakeJmap,
    model: Any,
    token: Any = None,
    states: list | None = None,
    forced: list | None = None,
) -> Handler:
    async def tokens(state: str | None = None, *, force: bool = False) -> str:
        if states is not None:
            states.append(state)
        if forced is not None:
            forced.append(force)
        if token:
            raise token
        return "vault-token"

    return Handler(
        SETTINGS,
        token_source=tokens,
        model_factory=lambda kind: model,
        checkpointer=InMemorySaver(),
        jmap_factory=lambda t, writes: JmapClient(
            SESSION_URL, t, transport=fake.transport(), backoff=0, writes=writes
        ),
    )


async def test_ask_uses_the_vault_token(scripted: ScriptFactory, fake: FakeJmap) -> None:
    model = scripted(
        tool_turn(call("search_mail", {"text": "dinner"})), AIMessage(content="7pm [email:M1].")
    )
    result = await handler(fake, model)({"mode": "ask", "question": "When?"}, SESSION)
    assert result["status"] == "ok" and result["citations"] == ["M1"]
    assert set(fake.tokens) == {"Bearer vault-token"}


async def test_login_returns_the_consent_url(scripted: ScriptFactory, fake: FakeJmap) -> None:
    states: list = []
    h = handler(fake, scripted(), AuthorizationRequired("https://auth.example.com/consent"), states)
    assert await h({"mode": "login", "state": STATE}, SESSION) == {
        "status": "authorization_required",
        "authorization_url": "https://auth.example.com/consent",
        "return_url": "http://localhost:8765/callback",
    }
    assert states == [STATE]
    # An ask never hands out a consent URL.
    assert await h({"mode": "ask", "question": "x"}, SESSION) == {
        "status": "authorization_required"
    }


async def test_short_session_ids_are_rejected(scripted: ScriptFactory, fake: FakeJmap) -> None:
    result = await handler(fake, scripted())({"mode": "ask", "question": "x"}, "short")
    assert result == {"status": "error", "error": "bad_request"}


async def test_logs_are_content_free(
    scripted: ScriptFactory, fake: FakeJmap, caplog: pytest.LogCaptureFixture
) -> None:
    question = "What is the secret dinner plan?"
    model = scripted(
        tool_turn(call("read_email", {"ids": [e["id"] for e in MAILBOX][:5]})),
        AIMessage(content="Shall we meet at 7pm [email:M1]."),
    )
    with caplog.at_level(logging.DEBUG):
        await handler(fake, model)({"mode": "ask", "question": question}, SESSION)
        await handler(fake, scripted(), RuntimeError(question))(
            {"mode": "ask", "question": question}, SESSION
        )
    text = caplog.text
    assert question not in text
    for email in MAILBOX:
        assert email["subject"] not in text and email["body"][:20] not in text
    records = [json.loads(r.getMessage()) for r in caplog.records if r.name == "s3mail_agent"]
    assert {r["event"] for r in records} == {"invocation", "invocation_failed"}


@pytest.mark.parametrize("state", [None, "", "short", "x" * 129, "bad state with spaces!!", 7])
async def test_login_needs_a_random_state(
    scripted: ScriptFactory, fake: FakeJmap, state: Any
) -> None:
    states: list = []
    h = handler(fake, scripted(), states=states)
    result = await h({"mode": "login", "state": state}, SESSION)
    assert result == {"status": "error", "error": "bad_request"} and states == []


async def test_forced_login_asks_for_consent_again(scripted: ScriptFactory, fake: FakeJmap) -> None:
    forced: list = []
    h = handler(fake, scripted(), forced=forced)
    await h({"mode": "login", "state": STATE}, SESSION)
    await h({"mode": "login", "state": STATE, "force": True}, SESSION)
    await h({"mode": "login", "state": STATE, "force": "yes"}, SESSION)
    # Only a real boolean forces consent; asks never do.
    await h({"mode": "ask", "question": "x", "force": True}, SESSION)
    assert forced[:3] == [False, True, False] and not any(forced[3:])


async def test_asks_send_no_login_state(scripted: ScriptFactory, fake: FakeJmap) -> None:
    states: list = []
    model = scripted(AIMessage(content="Hello. [no-mail]"))
    await handler(fake, model, states=states)({"mode": "ask", "question": "Hi?"}, SESSION)
    assert states == [None]


# --- Later phases: routing and scope gating ----------------------------------

READ_ONLY = ("s3mail/mail.read",)
ALL_SCOPES = ("s3mail/mail.read", "s3mail/mail.write", "s3mail/mail.submit")


def full_handler(
    server: MailServer,
    model: Any,
    *,
    scopes: tuple[str, ...] = ALL_SCOPES,
    table: MemoryTable | None = None,
    prefs: MemoryPreferences | None = None,
    clients: list[JmapClient] | None = None,
) -> Handler:
    settings = replace(
        SETTINGS, oauth_scopes=scopes, triage_model="openai/gpt-6-luna", agent_table="t"
    )

    async def tokens(state: str | None = None, *, force: bool = False) -> str:
        return "vault-token"

    def jmap(token: str, writes: frozenset[str]) -> JmapClient:
        c = JmapClient(SESSION_URL, token, transport=server.transport(), backoff=0, writes=writes)
        if clients is not None:
            clients.append(c)
        return c

    return Handler(
        settings,
        token_source=tokens,
        model_factory=lambda kind: model,
        checkpointer=InMemorySaver(),
        jmap_factory=jmap,
        table=table if table is not None else MemoryTable(),
        preferences=prefs if prefs is not None else MemoryPreferences(),
    )


def inbox() -> MailServer:
    s = MailServer()
    # Within the first digest's one-day window.
    recent = (datetime.now(UTC) - timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    s.deliver(message("M1", "T1", "a@example.com", "Hello", recent, "Hi there"))
    return s


@pytest.mark.parametrize(
    "payload",
    [
        {"mode": "draft", "email_id": "M1"},
        {"mode": "act", "question": "Archive it"},
        {"mode": "approve", "hash": "0" * 64, "approve": True},
        {"mode": "digest", "deliver": "mail"},
    ],
)
async def test_write_modes_need_the_write_scope(scripted: ScriptFactory, payload: Any) -> None:
    s = inbox()
    model = scripted()
    result = await full_handler(s, model, scopes=READ_ONLY)(payload, SESSION)
    assert result == {"status": "error", "error": "scope_not_granted"}
    assert s.calls == [] and model.seen == []


async def test_triage_mode_stores_a_row_with_no_writes(scripted: ScriptFactory) -> None:
    s, table, clients = inbox(), MemoryTable(), []
    verdict = '{"category":"personal","priority":"normal","needs_reply":true,"summary":"Says hi."}'
    h = full_handler(
        s, scripted(AIMessage(content=verdict)), scopes=READ_ONLY, table=table, clients=clients
    )
    event = {"mode": "triage", "event": {"eventId": "M1:received", "emailId": "M1"}}
    assert (await h(event, SESSION))["triage"] == "stored"
    assert (await h(event, SESSION))["triage"] == "duplicate"
    assert all(c.writes == frozenset() for c in clients) and s.writes == []


async def test_digest_mode_returns_text_with_read_scope(scripted: ScriptFactory) -> None:
    s = inbox()
    h = full_handler(s, scripted(AIMessage(content="Hello [email:M1].")), scopes=READ_ONLY)
    result = await h({"mode": "digest"}, SESSION)
    assert result["status"] == "ok" and "Hello" in result["digest"] and s.writes == []


async def test_draft_mode_writes_only_email_set(scripted: ScriptFactory) -> None:
    s, clients = inbox(), []
    prefs = MemoryPreferences()
    h = full_handler(s, scripted(AIMessage(content="Hi back.")), prefs=prefs, clients=clients)
    assert (await h({"mode": "remember", "text": "Sign as O."}, SESSION))["status"] == "ok"
    result = await h({"mode": "draft", "email_id": "M1", "instructions": "be warm"}, SESSION)
    assert result["status"] == "ok" and s.by_id(result["draft_id"])["mailboxIds"] == {
        "drafts": True
    }
    assert clients[-1].writes == frozenset({"Email/set"})
    listed = await h({"mode": "preferences"}, SESSION)
    assert listed["preferences"] == [{"id": "P1", "text": "Sign as O."}]
    assert (await h({"mode": "forget", "ids": None}, SESSION)) == {"status": "ok", "forgotten": 1}


async def test_act_then_approve_through_the_handler(scripted: ScriptFactory) -> None:
    s = inbox()
    model = scripted(
        tool_turn(call("search_mail", {}, id="s1")),
        tool_turn(call("propose_update", {"email_ids": ["M1"], "move_to": "archive"}, id="p1")),
    )
    h = full_handler(s, model)
    pending = await h({"mode": "act", "question": "Archive hello"}, SESSION)
    assert pending["status"] == "approval_required"
    digest = pending["approval"]["hash"]
    done = await h({"mode": "approve", "hash": digest, "approve": True}, SESSION)
    assert done["outcome"]["outcome"] == "done" and s.by_id("M1")["mailboxIds"] == {"archive": True}


async def test_send_needs_the_submit_scope(scripted: ScriptFactory) -> None:
    s = inbox()
    model = scripted(tool_turn(call("propose_send", {"draft_id": "M1"}, id="p1")))
    h = full_handler(s, model, scopes=("s3mail/mail.read", "s3mail/mail.write"))
    result = await h({"mode": "act", "question": "Send it"}, SESSION)
    assert result["error"] == "tool_not_allowed"


@pytest.mark.parametrize(
    "payload",
    [
        {"mode": "approve", "hash": "not-a-hash", "approve": True},
        {"mode": "approve", "hash": "0" * 64, "approve": "yes"},
        {"mode": "triage", "event": "M1"},
        {"mode": "digest", "deliver": "fax"},
        {"mode": "remember", "text": ""},
        {"mode": "forget", "ids": "all"},
        {"mode": "draft", "email_id": 7},
    ],
)
async def test_bad_payloads(scripted: ScriptFactory, payload: Any) -> None:
    result = await full_handler(inbox(), scripted())(payload, SESSION)
    assert result == {"status": "error", "error": "bad_request"}


async def test_triage_and_digest_need_the_table(scripted: ScriptFactory) -> None:
    result = await handler(FakeJmap(), scripted())({"mode": "digest"}, SESSION)
    assert result == {"status": "error", "error": "unsupported_mode"}


async def test_later_mode_logs_are_content_free(
    scripted: ScriptFactory, caplog: pytest.LogCaptureFixture
) -> None:
    s = inbox()
    verdict = '{"category":"personal","priority":"normal","needs_reply":true,"summary":"Says hi."}'
    model = scripted(
        AIMessage(content=verdict),
        AIMessage(content="Hello there [email:M1]."),
        AIMessage(content="Hi back."),
    )
    h = full_handler(s, model)
    with caplog.at_level(logging.DEBUG):
        await h({"mode": "triage", "event": {"eventId": "M1:received", "emailId": "M1"}}, SESSION)
        await h({"mode": "digest"}, SESSION)
        await h({"mode": "remember", "text": "Secret preference text"}, SESSION)
        await h({"mode": "draft", "email_id": "M1", "instructions": "secret instruction"}, SESSION)
    text = caplog.text
    for secret in (
        "Hi there",
        "Hello",
        "Says hi",
        "Secret preference",
        "secret instruction",
        "Hi back",
    ):
        assert secret not in text
    modes = [json.loads(r.getMessage())["mode"] for r in caplog.records if r.name == "s3mail_agent"]
    assert modes == ["triage", "digest", "remember", "draft"]


def test_the_deployed_checkpointer_restores_from_snapshots() -> None:
    saver: Any = memory_saver(SETTINGS)
    # Bounded restores read the latest snapshot, never a capped history.
    assert saver.checkpoint_format == "snapshot" and saver.limit is None
