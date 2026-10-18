from __future__ import annotations

import ast
import pathlib

import pytest

from s3mail_agent import jmap as jmap_module
from s3mail_agent.jmap import READ_METHODS, GuardError, JmapAuthError, JmapClient, JmapError

from .conftest import SESSION_URL, FakeJmap

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
    assert all(m.endswith(("/get", "/query")) for m in READ_METHODS)


def test_no_source_can_send_destroy() -> None:
    """Fails the build if any agent module names a destroy argument or a write method."""
    for path in SRC.rglob("*.py"):
        tree = ast.parse(path.read_text())
        for node in ast.walk(tree):
            if isinstance(node, ast.Constant) and isinstance(node.value, str):
                value = node.value
                if path.name == "jmap.py" and value in ("create", "update", "destroy"):
                    continue  # the guard's own refusal list
                assert value != "destroy", f"{path.name} names destroy"
                assert not value.endswith(("/set", "/import", "/copy")), f"{path.name}: {value}"


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
