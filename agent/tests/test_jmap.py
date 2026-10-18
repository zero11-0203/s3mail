from __future__ import annotations

import ast
import pathlib

import pytest

from s3mail_agent import jmap as jmap_module
from s3mail_agent.jmap import (
    EMAIL_SET,
    MAILBOX_SET,
    READ_METHODS,
    SUBMISSION_SET,
    WRITE_ARGUMENTS,
    WRITE_METHODS,
    GuardError,
    JmapAuthError,
    JmapClient,
    JmapError,
    JmapUnknownResult,
)

from .conftest import SESSION_URL, FakeJmap
from .mailserver import MailServer, message

SRC = pathlib.Path(jmap_module.__file__).parent


async def test_session_discovers_account_and_limits(client: JmapClient) -> None:
    session = await client.session()
    assert session.account_id == "A1"
    assert session.max_calls_in_request == 16


async def test_refuses_server_without_mail_capability(fake: FakeJmap) -> None:
    fake.capabilities.pop("urn:ietf:params:jmap:mail")
    client = JmapClient(SESSION_URL, "t", transport=fake.transport())
    with pytest.raises(JmapError, match="capability"):
        await client.session()


@pytest.mark.parametrize(
    "method", ["Email/set", "Mailbox/set", "EmailSubmission/set", "Email/import"]
)
async def test_write_methods_are_refused_before_sending(
    client: JmapClient, fake: FakeJmap, method: str
) -> None:
    with pytest.raises(GuardError):
        await client.call([(method, {})])
    assert fake.calls == []


@pytest.mark.parametrize("key", ["create", "update", "destroy"])
async def test_mutation_arguments_are_refused(client: JmapClient, fake: FakeJmap, key: str) -> None:
    with pytest.raises(GuardError):
        await client.call([("Email/get", {key: ["M1"]})])
    assert fake.calls == []


def test_allowlist_has_only_reads() -> None:
    assert all(m.endswith(("/get", "/query", "/changes")) for m in READ_METHODS)
    assert not READ_METHODS & WRITE_METHODS


def test_no_source_can_send_destroy() -> None:
    """Fails the build if any agent module names destroy or a write method
    outside jmap.py, whose write table and guard are the only places."""
    for path in SRC.rglob("*.py"):
        if path.name == "jmap.py":
            continue
        tree = ast.parse(path.read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.Constant) and isinstance(node.value, str):
                value = node.value
                assert value != "destroy", f"{path.name} names destroy"
                assert "onSuccessDestroyEmail" not in value, path.name
                assert not value.endswith(("/set", "/import", "/copy")), f"{path.name}: {value}"
    # In jmap.py, destroy appears only in the guard's refusal list.
    tree = ast.parse((SRC / "jmap.py").read_text())
    names = [n for n in ast.walk(tree) if isinstance(n, ast.Constant) and n.value == "destroy"]
    assert len(names) == 1
    assert all("destroy" not in allowed for allowed in WRITE_ARGUMENTS.values())


@pytest.mark.parametrize("method", sorted(WRITE_METHODS))
async def test_writes_need_the_mode_allowlist(fake: FakeJmap, method: str) -> None:
    client = JmapClient(SESSION_URL, "t", transport=fake.transport())
    with pytest.raises(GuardError):
        await client.write(method, {"create": {}})
    assert fake.calls == []


async def test_destroy_is_refused_in_every_mode(fake: FakeJmap) -> None:
    client = JmapClient(SESSION_URL, "t", transport=fake.transport(), writes=WRITE_METHODS)
    for method in WRITE_METHODS:
        with pytest.raises(GuardError):
            await client.write(method, {"destroy": ["M1"]})
    with pytest.raises(GuardError):
        await client.write(MAILBOX_SET, {"update": {"inbox": {"name": "x"}}})
    with pytest.raises(GuardError):
        await client.write(SUBMISSION_SET, {"onSuccessDestroyEmail": ["#s"]})
    assert fake.calls == []


def test_unknown_write_methods_are_refused() -> None:
    with pytest.raises(GuardError):
        JmapClient(SESSION_URL, "t", writes=frozenset({"Email/import"}))


async def test_writes_are_sent_once() -> None:
    server = MailServer()
    client = JmapClient(
        SESSION_URL, "t", transport=server.transport(), backoff=0, writes=frozenset({EMAIL_SET})
    )
    server.statuses = [503, 200]
    with pytest.raises(JmapUnknownResult):
        await client.write(EMAIL_SET, {"update": {}})
    assert server.statuses == [200]  # not retried

    server.fail_write = "transport"
    server.deliver(message("M1", "T1", "a@example.com", "s", "2026-10-01T00:00:00Z", "b"))
    with pytest.raises(JmapUnknownResult):
        await client.write(EMAIL_SET, {"update": {"M1": {"keywords/$flagged": True}}})
    assert sum(n == "Email/set" for n, _ in server.writes) == 1
    assert server.by_id("M1")["keywords"] == {"$flagged": True}


async def test_retries_reads_on_429_and_5xx(client: JmapClient, fake: FakeJmap) -> None:
    fake.statuses = [429, 503]
    result = await client.call([("Mailbox/get", {"ids": None})])
    assert result["c0"]["list"][0]["role"] == "inbox"


async def test_gives_up_after_retries(client: JmapClient, fake: FakeJmap) -> None:
    fake.statuses = [500, 500, 500, 500]
    with pytest.raises(JmapError, match="500"):
        await client.call([("Mailbox/get", {"ids": None})])


@pytest.mark.parametrize("status", [401, 403])
async def test_auth_errors_are_not_retried(client: JmapClient, fake: FakeJmap, status: int) -> None:
    fake.statuses = [status, 200]
    with pytest.raises(JmapAuthError):
        await client.call([("Mailbox/get", {"ids": None})])
    assert fake.statuses == [200]


async def test_sends_bearer_token(client: JmapClient, fake: FakeJmap) -> None:
    await client.call([("Mailbox/get", {"ids": None})])
    assert set(fake.tokens) == {"Bearer test-token"}
