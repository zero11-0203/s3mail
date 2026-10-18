from __future__ import annotations

import io
import json
import logging
from typing import Any

import pytest
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver

from s3mail_agent import cli
from s3mail_agent.app import Handler
from s3mail_agent.config import Settings
from s3mail_agent.identity import AuthorizationRequired
from s3mail_agent.jmap import JmapClient

from .conftest import MAILBOX, SESSION_URL, FakeJmap, ScriptFactory, call, tool_turn

SETTINGS = Settings(
    region="us-east-1",
    jmap_session_url=SESSION_URL,
    oauth_provider="s3mail-agent-cognito",
    oauth_scopes=("s3mail/mail.read",),
    oauth_return_url="http://localhost:8765/callback",
    memory_id="mem-1",
    ask_model_id="model",
)
SESSION = "cli-00000000-0000-4000-8000-000000000000"


def handler(fake: FakeJmap, model: Any, token: Any = None) -> Handler:
    async def tokens() -> str:
        if token:
            raise token
        return "vault-token"

    return Handler(
        SETTINGS,
        token_source=tokens,
        model_factory=lambda: model,
        checkpointer=InMemorySaver(),
        jmap_factory=lambda t: JmapClient(SESSION_URL, t, transport=fake.transport(), backoff=0),
    )


async def test_ask_uses_the_vault_token(scripted: ScriptFactory, fake: FakeJmap) -> None:
    model = scripted(
        tool_turn(call("search_mail", {"text": "dinner"})), AIMessage(content="7pm [email:M1].")
    )
    result = await handler(fake, model)({"mode": "ask", "question": "When?"}, SESSION)
    assert result["status"] == "ok" and result["citations"] == ["M1"]
    assert set(fake.tokens) == {"Bearer vault-token"}


async def test_login_returns_the_consent_url(scripted: ScriptFactory, fake: FakeJmap) -> None:
    h = handler(fake, scripted(), AuthorizationRequired("https://auth.example.com/consent"))
    assert await h({"mode": "login"}, SESSION) == {
        "status": "authorization_required",
        "authorization_url": "https://auth.example.com/consent",
    }
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


class FakeRuntimeClient:
    def __init__(self, replies: list[dict[str, Any]]) -> None:
        self.replies = replies
        self.invocations: list[dict[str, Any]] = []
        self.completed: list[dict[str, Any]] = []

    def invoke_agent_runtime(self, **kwargs: Any) -> dict[str, Any]:
        self.invocations.append(kwargs)
        return {"response": io.BytesIO(json.dumps(self.replies.pop(0)).encode())}

    def complete_resource_token_auth(self, **kwargs: Any) -> None:
        self.completed.append(kwargs)


ARN = "arn:aws:bedrock-agentcore:us-east-1:111111111111:runtime/s3mail_agent-abc"


def test_cli_ask_sends_owner_and_session(capsys: pytest.CaptureFixture[str]) -> None:
    client = FakeRuntimeClient([{"status": "ok", "answer": "7pm [email:M1]", "citations": ["M1"]}])
    assert cli.ask(cli.Runtime(ARN, client=client), "When?", None) == 0
    sent = client.invocations[0]
    assert sent["runtimeUserId"] == "owner" and sent["qualifier"] == "live"
    assert len(sent["runtimeSessionId"]) >= 33
    assert json.loads(sent["payload"]) == {"mode": "ask", "question": "When?"}
    assert "7pm" in capsys.readouterr().out


def test_cli_ask_reports_missing_authorization() -> None:
    client = FakeRuntimeClient([{"status": "authorization_required"}])
    assert cli.ask(cli.Runtime(ARN, client=client), "When?", "cli-thread-" + "0" * 30) == 2


def test_cli_login_completes_session_binding(monkeypatch: pytest.MonkeyPatch) -> None:
    client = FakeRuntimeClient(
        [
            {"status": "authorization_required", "authorization_url": "https://auth.example.com/c"},
            {"status": "ok"},
        ]
    )
    monkeypatch.setattr(cli, "wait_for_callback", lambda url, timeout: "urn:session:1")
    assert (
        cli.login(cli.Runtime(ARN, client=client), cli.DEFAULT_RETURN_URL, open_browser=False) == 0
    )
    assert client.completed == [
        {"sessionUri": "urn:session:1", "userIdentifier": {"userId": "owner"}}
    ]


def test_callback_parsing() -> None:
    assert cli.session_from_callback("/callback?session_id=abc", "/callback") == "abc"
    assert cli.session_from_callback("/other?session_id=abc", "/callback") is None
    assert cli.session_from_callback("/callback", "/callback") is None
