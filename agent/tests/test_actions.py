"""A3 approval scenarios (docs/specs/agent.md#a3-gate-approved-actions):
0 actions without a matching approval, 0 re-submissions, and expired or
rejected approvals run nothing. Each scenario runs the real graph with a
checkpointer, a scripted model and the stateful fake server."""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any

import pytest
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from s3mail_agent import graph as graph_module
from s3mail_agent import proposals
from s3mail_agent.graph import build_graph, run_approve, run_ask
from s3mail_agent.jmap import EMAIL_SET, SUBMISSION_SET, JmapClient
from s3mail_agent.store import MemoryTable

from .conftest import SESSION_URL, ScriptFactory, call, tool_turn
from .mailserver import OWNER, MailServer, message

THREAD = "cli-00000000-0000-4000-8000-00000000000a"
BOTH = frozenset({"propose_update", "propose_send"})


def server() -> MailServer:
    s = MailServer()
    s.deliver(
        message("M1", "T1", "news@example.org", "Weekly news", "2026-10-17T09:00:00Z", "News")
    )
    s.deliver(message("M2", "T2", "news@example.org", "More news", "2026-10-17T10:00:00Z", "News"))
    s.deliver(
        message(
            "D1",
            "T3",
            OWNER,
            "Re: Plan",
            "2026-10-17T11:00:00Z",
            "Sounds good.",
            mailbox="drafts",
            keywords={"$draft": True, "$seen": True},
            to="friend@example.com",
        )
    )
    return s


class Rig:
    def __init__(self, s: MailServer, scripted: ScriptFactory, *script: AIMessage) -> None:
        self.server = s
        self.saver = InMemorySaver()
        self.table = MemoryTable()
        self.model = scripted(*script)

    def graph(self, *, writes: frozenset[str] = frozenset(), propose: frozenset[str] = BOTH) -> Any:
        jmap = JmapClient(
            SESSION_URL, "t", transport=self.server.transport(), backoff=0, writes=writes
        )
        return build_graph(
            self.model, jmap, checkpointer=self.saver, propose=propose, table=self.table
        )

    async def act(self, text: str = "Archive the newsletters", **kw: Any) -> dict[str, Any]:
        return await run_ask(self.graph(**kw), text, thread_id=THREAD, actor_id="owner", mode="act")

    async def approve(self, digest: str, approve: bool = True, **kw: Any) -> dict[str, Any]:
        writes = kw.pop("writes", frozenset({EMAIL_SET, SUBMISSION_SET}))
        return await run_approve(
            self.graph(writes=writes, **kw),
            thread_id=THREAD,
            actor_id="owner",
            approval_hash=digest,
            approve=approve,
        )


def search_then(*calls: dict[str, Any]) -> list[AIMessage]:
    return [tool_turn(call("search_mail", {"from": "news"}, id="s1")), tool_turn(*calls)]


ARCHIVE = call("propose_update", {"email_ids": ["M1", "M2"], "move_to": "archive"}, id="p1")
FLAG = call("propose_update", {"email_ids": ["M1"], "flagged": True}, id="p1")
SEND = tool_turn(call("propose_send", {"draft_id": "D1"}, id="p1"))


def updates(s: MailServer) -> int:
    return sum(n == "Email/set" for n, _ in s.writes)


def submissions(s: MailServer) -> int:
    return sum(n == "EmailSubmission/set" for n, _ in s.writes)


# 1
async def test_approved_archive_runs_once(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    pending = await rig.act()
    assert pending["status"] == "approval_required" and updates(rig.server) == 0
    approval = pending["approval"]
    assert approval["kind"] == "update" and "move to Archive" in approval["summary"]
    done = await rig.approve(approval["hash"])
    assert done["outcome"]["outcome"] == "done"
    assert rig.server.by_id("M1")["mailboxIds"] == {"archive": True}
    assert updates(rig.server) == 1
    # 2: approving again finds nothing pending and runs nothing.
    assert (await rig.approve(approval["hash"]))["error"] == "no_pending_approval"
    assert updates(rig.server) == 1


# 3
async def test_approved_flag(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(FLAG))
    approval = (await rig.act("Flag the news"))["approval"]
    await rig.approve(approval["hash"])
    assert rig.server.by_id("M1")["keywords"] == {"$flagged": True}


# 4
async def test_rejected_update_runs_nothing(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    approval = (await rig.act())["approval"]
    result = await rig.approve(approval["hash"], approve=False)
    assert result["outcome"] == {"outcome": "rejected"} and rig.server.writes == []


# 5
async def test_wrong_hash_runs_nothing(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    await rig.act()
    result = await rig.approve("0" * 64)
    assert result["outcome"] == {"outcome": "approval_mismatch"} and rig.server.writes == []


# 6
async def test_expired_approval_runs_nothing(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    approval = (await rig.act())["approval"]
    later = datetime.now(UTC) + timedelta(hours=25)
    real = proposals.execute

    async def late(*args: Any, **kw: Any) -> dict[str, Any]:
        return await real(*args, now=later)

    monkeypatch.setattr("s3mail_agent.graph.execute", late)
    result = await rig.approve(approval["hash"])
    assert result["outcome"] == {"outcome": "expired"} and rig.server.writes == []


# 7
async def test_approved_send_submits_once(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send my reply about the plan"))["approval"]
    assert approval["kind"] == "send"
    assert "To: friend@example.com" in approval["summary"] and "Sounds good." in approval["summary"]
    result = await rig.approve(approval["hash"])
    assert result["outcome"]["outcome"] == "sent" and submissions(rig.server) == 1
    draft = rig.server.by_id("D1")
    assert draft["mailboxIds"] == {"sent": True} and "$draft" not in draft["keywords"]
    assert rig.table.markers[f"send#{approval['hash']}"].email_id == "D1"


# 8
async def test_rejected_send_runs_nothing(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    await rig.approve(approval["hash"], approve=False)
    assert submissions(rig.server) == 0 and rig.table.markers == {}


# 9
async def test_crash_after_submit_does_not_resubmit(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    real_write = JmapClient.write

    async def crash_after(self: JmapClient, name: str, args: dict[str, Any]) -> Any:
        await real_write(self, name, args)
        raise RuntimeError("process killed after the submission")

    monkeypatch.setattr(JmapClient, "write", crash_after)
    with pytest.raises(RuntimeError):
        await rig.approve(approval["hash"])
    monkeypatch.setattr(JmapClient, "write", real_write)
    assert submissions(rig.server) == 1
    # The resumed executor finds the marker and reports instead of sending.
    result = await rig.approve(approval["hash"])
    assert result["outcome"] == {"outcome": "already_attempted", "submissions": ["S1"]}
    assert submissions(rig.server) == 1


# 10
async def test_unknown_send_result_is_reported_not_retried(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    rig.server.fail_write = "transport"
    result = await rig.approve(approval["hash"])
    assert result["outcome"]["outcome"] == "unknown"
    assert result["outcome"]["submissions"] == ["S1"]
    assert submissions(rig.server) == 1


# 11
async def test_changed_draft_is_not_sent(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    rig.server.by_id("D1")["blobId"] = "Gchanged"
    result = await rig.approve(approval["hash"])
    assert result["outcome"] == {"outcome": "draft_changed"} and submissions(rig.server) == 0


# 12
async def test_only_emails_found_in_the_run_can_be_proposed(scripted: ScriptFactory) -> None:
    rig = Rig(
        server(),
        scripted,
        tool_turn(ARCHIVE),
        AIMessage(content="I need to search first. [no-action]"),
    )
    result = await rig.act()
    assert result["status"] == "ok" and "approval" not in result
    assert "Proposal refused" in rig.model.seen[1][-1].content and rig.server.writes == []


# 13
@pytest.mark.parametrize("target", ["trash", "Trash", "drafts", "sent"])
async def test_moves_to_trash_drafts_or_sent_are_refused(
    scripted: ScriptFactory, target: str
) -> None:
    bad = call("propose_update", {"email_ids": ["M1"], "move_to": target}, id="p1")
    rig = Rig(server(), scripted, *search_then(bad), AIMessage(content="Cannot. [no-action]"))
    assert "approval" not in await rig.act("Delete the news")
    assert rig.server.writes == []


# 14
async def test_only_drafts_can_be_sent(scripted: ScriptFactory) -> None:
    bad = tool_turn(call("propose_send", {"draft_id": "M1"}, id="p1"))
    rig = Rig(server(), scripted, bad, AIMessage(content="Not a draft. [no-action]"))
    assert "approval" not in await rig.act("Send the news")


# 15
async def test_drafts_from_foreign_addresses_are_refused(scripted: ScriptFactory) -> None:
    s = server()
    s.by_id("D1")["from"] = [{"name": None, "email": "someone@example.net"}]
    rig = Rig(s, scripted, SEND, AIMessage(content="Refused. [no-action]"))
    assert "approval" not in await rig.act("Send it")


# 16
async def test_one_proposal_per_request(scripted: ScriptFactory) -> None:
    second = call("propose_update", {"email_ids": ["M2"], "seen": True}, id="p2")
    rig = Rig(
        server(), scripted, tool_turn(call("search_mail", {}, id="s1")), tool_turn(FLAG, second)
    )
    result = await rig.act("Flag one and read the other")
    summary = result["approval"]["summary"]
    assert "flag: Weekly news" in summary and "More news" not in summary
    refused = [m for m in rig.model.seen[-1] if getattr(m, "tool_call_id", None) == "p2"]
    assert refused == [] or "one proposal" in refused[0].content
    await rig.approve(result["approval"]["hash"])
    assert rig.server.by_id("M2")["keywords"] == {}  # the second proposal never ran


# 17
async def test_a_new_request_abandons_the_pending_approval(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE), AIMessage(content="Hi. [no-action]"))
    approval = (await rig.act())["approval"]
    await rig.act("Never mind")
    assert (await rig.approve(approval["hash"]))["error"] == "no_pending_approval"
    assert rig.server.writes == []


# 18
async def test_tampered_proposals_run_nothing() -> None:
    s = server()
    jmap = JmapClient(SESSION_URL, "t", transport=s.transport(), writes=frozenset({EMAIL_SET}))
    proposal = await proposals.build_update(
        jmap, proposals.propose_update(email_ids=["M1"], flagged=True), {"M1"}
    )
    tampered = {**proposal, "calls": [[EMAIL_SET, {"update": {"M2": {"mailboxIds/trash": True}}}]]}
    result = await proposals.execute(
        jmap, MemoryTable(), tampered, {"hash": proposal["hash"], "approve": True}
    )
    assert result == {"outcome": "approval_mismatch"} and s.writes == []


# 19
async def test_partial_updates_are_reported_not_retried(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    approval = (await rig.act())["approval"]
    rig.server.fail_write = "stateMismatch"
    result = await rig.approve(approval["hash"])
    assert result["outcome"]["outcome"] == "failed" and updates(rig.server) == 1


# 20
async def test_send_needs_the_submit_scope(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    result = await rig.act("Send it", propose=frozenset({"propose_update"}))
    assert result == {**result, "status": "error", "error": "tool_not_allowed"}
    assert rig.server.writes == []


# 21
async def test_act_needs_a_propose_scope(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted)
    result = await rig.act(propose=frozenset())
    assert result["error"] == "unsupported_mode" and rig.model.seen == []


# 22
async def test_executor_without_the_submit_write_cannot_send(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    with pytest.raises(Exception, match="not allowed"):
        await rig.approve(approval["hash"], writes=frozenset({EMAIL_SET}))
    assert submissions(rig.server) == 0


async def crash(*args: Any, **kwargs: Any) -> Any:
    raise RuntimeError("process killed")


# 23
@pytest.mark.parametrize("kind", ["update", "send"])
async def test_rejecting_an_interrupted_approval_runs_nothing(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch, kind: str
) -> None:
    rig = Rig(server(), scripted, *(search_then(FLAG) if kind == "update" else [SEND]))
    approval = (await rig.act())["approval"]
    real = graph_module.execute
    monkeypatch.setattr(graph_module, "execute", crash)  # killed before the executor ran
    with pytest.raises(RuntimeError):
        await rig.approve(approval["hash"])
    monkeypatch.setattr(graph_module, "execute", real)
    result = await rig.approve(approval["hash"], approve=False)
    assert result["outcome"] == {"outcome": "rejected"} and rig.server.writes == []
    # The rejection is final: approving afterwards finds nothing pending.
    assert (await rig.approve(approval["hash"]))["error"] == "no_pending_approval"
    assert rig.server.writes == []


# 24
async def test_rejecting_after_the_send_step_began_reports_it(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    rig = Rig(server(), scripted, SEND)
    approval = (await rig.act("Send it"))["approval"]
    monkeypatch.setattr(JmapClient, "write", crash)  # killed after the marker, before sending
    with pytest.raises(RuntimeError):
        await rig.approve(approval["hash"])
    monkeypatch.undo()
    result = await rig.approve(approval["hash"], approve=False)
    assert result["outcome"] == {"outcome": "already_attempted", "submissions": []}
    assert submissions(rig.server) == 0


# 25
async def test_crash_after_an_update_does_not_repeat_it(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    rig = Rig(server(), scripted, *search_then(FLAG))
    approval = (await rig.act("Flag the news"))["approval"]
    real_write = JmapClient.write

    async def crash_after(self: JmapClient, name: str, args: dict[str, Any]) -> Any:
        await real_write(self, name, args)
        raise RuntimeError("process killed after the update")

    monkeypatch.setattr(JmapClient, "write", crash_after)
    with pytest.raises(RuntimeError):
        await rig.approve(approval["hash"])
    monkeypatch.undo()
    rig.server.by_id("M1")["keywords"].pop("$flagged")  # the owner unflags it in aerc
    result = await rig.approve(approval["hash"])
    assert result["outcome"] == {"outcome": "already_attempted", "emails": ["M1"]}
    assert updates(rig.server) == 1 and rig.server.by_id("M1")["keywords"] == {}


# 26
async def test_mail_changed_since_the_proposal_is_not_updated(scripted: ScriptFactory) -> None:
    rig = Rig(server(), scripted, *search_then(ARCHIVE))
    approval = (await rig.act())["approval"]
    rig.server.by_id("M2")["mailboxIds"] = {"junk": True}  # moved meanwhile in aerc
    result = await rig.approve(approval["hash"])
    assert result["outcome"] == {"outcome": "mail_changed"} and updates(rig.server) == 0


# 27
async def test_failed_proposals_leave_no_content_in_traces(
    scripted: ScriptFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    monkeypatch.setattr(graph_module, "_tracer", provider.get_tracer("test"))
    subject = "SYNTHETIC_PRIVATE_SUBJECT_47"
    bad = call("propose_update", {"email_ids": ["M1"], "move_to": subject}, id="p1")
    rig = Rig(server(), scripted, *search_then(bad), AIMessage(content="No such box. [no-action]"))
    await rig.act()
    spans = exporter.get_finished_spans()
    failed = [s for s in spans if s.name == "tool propose_update"]
    assert failed and failed[0].attributes == {
        "s3mail.tool": "propose_update",
        "s3mail.error": "ProposalError",
    }
    dump = repr(
        [
            (s.attributes, [(e.name, e.attributes) for e in s.events], s.status.description)
            for s in spans
        ]
    )
    assert subject not in dump and "Traceback" not in dump
