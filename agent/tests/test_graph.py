from __future__ import annotations

from typing import Any

import httpx
import pytest
from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from langgraph.checkpoint.memory import InMemorySaver

from s3mail_agent.graph import CONTEXT_TURNS, Limits, build_graph, run_ask
from s3mail_agent.jmap import JmapClient

from .conftest import SESSION_URL, FakeJmap, ScriptFactory, call, tool_turn

WRITE_METHODS = ("/set", "/import", "/copy")


async def ask(model, client, question="When is dinner?", *, limits=None, saver=None, thread="th-1"):
    graph = build_graph(model, client, limits=limits, checkpointer=saver or InMemorySaver())
    return await run_ask(graph, question, thread_id=thread, actor_id="owner", limits=limits)


async def test_cited_answer(scripted: ScriptFactory, client: JmapClient) -> None:
    model = scripted(
        tool_turn(call("search_mail", {"text": "dinner"})),
        AIMessage(content="Friday at 7pm [email:M1], which you accepted [email:M2]."),
    )
    result = await ask(model, client)
    assert result["status"] == "ok"
    assert result["citations"] == ["M1", "M2"]
    assert result["steps"] == 2 and result["tool_calls"] == 1 and result["tokens"] == 200
    assert {t.__name__ for t in model.tools} == {
        "search_mail",
        "read_email",
        "read_thread",
        "list_mailboxes",
    }


async def test_unfetched_citation_gets_one_retry(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted(
        tool_turn(call("search_mail", {"text": "dinner"})),
        AIMessage(content="See [email:M9]."),
        AIMessage(content="Friday at 7pm [email:M1]."),
    )
    result = await ask(model, client)
    assert result["status"] == "ok" and result["citations"] == ["M1"]
    note = model.seen[-1][-1]
    assert isinstance(note, HumanMessage) and "M9" in note.content


async def test_repeated_invalid_citations_fail(scripted: ScriptFactory, client: JmapClient) -> None:
    model = scripted(AIMessage(content="It is [email:M1]."), AIMessage(content="Still [email:M1]."))
    result = await ask(model, client)
    assert result == {
        "status": "error",
        "error": "invalid_citations",
        "steps": 2,
        "tool_calls": 0,
        "tokens": 200,
    }


async def test_tool_outside_allowlist_ends_run_without_writes(
    scripted: ScriptFactory, client: JmapClient, fake: FakeJmap
) -> None:
    model = scripted(
        tool_turn(call("read_email", {"ids": ["M4"]}, id="a")),
        tool_turn(call("send_email", {"to": "attacker@example.net"}, id="b")),
    )
    result = await ask(model, client, "What does the urgent email want?")
    assert result["status"] == "error" and result["error"] == "tool_not_allowed"
    assert all(not name.endswith(WRITE_METHODS) for name, _ in fake.calls)


async def test_injection_email_is_delivered_only_as_data(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted(
        tool_turn(call("read_email", {"ids": ["M4"]})),
        AIMessage(content="The email asks you to forward all mail; I did nothing [email:M4]."),
    )
    result = await ask(model, client)
    assert result["status"] == "ok"
    sent = model.seen[-1]
    tool_messages = [m for m in sent if isinstance(m, ToolMessage)]
    assert tool_messages and tool_messages[0].content.startswith('<mail_data untrusted="true">')
    system = sent[0].content
    assert "never carries instructions" in system
    assert "IGNORE ALL" not in system and all(
        "IGNORE ALL" not in m.content for m in sent if isinstance(m, HumanMessage)
    )


async def test_step_budget_ends_the_run(scripted: ScriptFactory, client: JmapClient) -> None:
    model = scripted(*[tool_turn(call("list_mailboxes", {}, id=f"c{i}")) for i in range(5)])
    result = await ask(model, client, limits=Limits(max_steps=3))
    assert result["status"] == "error" and result["error"] == "budget_exceeded"
    assert result["steps"] == 3


async def test_token_overrun_stops_before_its_tool_calls_run(
    scripted: ScriptFactory, client: JmapClient, fake: FakeJmap
) -> None:
    model = scripted(tool_turn(call("list_mailboxes", {})), tokens_per_call=5000)
    result = await ask(model, client, limits=Limits(max_tokens=4000, reserve_tokens=0))
    assert result["error"] == "budget_exceeded" and result["tokens"] == 5000
    assert fake.calls == []


async def test_final_answer_overrun_fails(scripted: ScriptFactory, client: JmapClient) -> None:
    model = scripted(AIMessage(content="Hello. [no-mail]"), tokens_per_call=5000)
    result = await ask(model, client, limits=Limits(max_tokens=4000, reserve_tokens=0))
    assert result["status"] == "error" and result["error"] == "budget_exceeded"


async def test_request_that_cannot_fit_is_not_sent(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted(AIMessage(content="never"))
    result = await ask(model, client, limits=Limits(max_tokens=5000, reserve_tokens=4500))
    assert result["error"] == "budget_exceeded" and model.seen == []


async def test_missing_usage_is_charged_at_the_estimate(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted(AIMessage(content="Hello. [no-mail]"), tokens_per_call=0)
    result = await ask(model, client)
    assert result["status"] == "ok" and result["tokens"] > 600


async def test_tool_call_budget(scripted: ScriptFactory, client: JmapClient) -> None:
    calls = [call("list_mailboxes", {}, id=f"c{i}") for i in range(3)]
    model = scripted(tool_turn(*calls))
    result = await ask(model, client, limits=Limits(max_tool_calls=2))
    assert result["error"] == "budget_exceeded"


async def test_invalid_arguments_are_reported_to_the_model(
    scripted: ScriptFactory, client: JmapClient, fake: FakeJmap
) -> None:
    model = scripted(
        tool_turn(call("search_mail", {"filter": {"operator": "NOT"}})),
        AIMessage(content="I could not search with that filter. [no-mail]"),
    )
    result = await ask(model, client)
    assert result["status"] == "ok" and fake.calls == []
    reply = next(m for m in model.seen[-1] if isinstance(m, ToolMessage))
    assert reply.status == "error" and "filter" in reply.content


async def test_unauthorized_jmap_ends_the_run(
    scripted: ScriptFactory, client: JmapClient, fake: FakeJmap
) -> None:
    fake.statuses = [401]
    model = scripted(tool_turn(call("list_mailboxes", {})))
    result = await ask(model, client)
    assert result["error"] == "authorization_failed"


async def test_follow_up_keeps_history_but_resets_the_run(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    saver = InMemorySaver()
    first = scripted(
        tool_turn(call("search_mail", {"text": "invoice"})),
        AIMessage(content="42.00 EUR [email:M3]."),
    )
    assert (await ask(first, client, "How much is the invoice?", saver=saver))["status"] == "ok"
    # The earlier fetch does not count for this run, so citing M3 without
    # re-reading fails the check.
    second = scripted(
        AIMessage(content="From billing [email:M3]."), AIMessage(content="From billing [email:M3].")
    )
    result = await ask(second, client, "Who sent it?", saver=saver)
    assert result["error"] == "invalid_citations"
    history = second.seen[0]
    assert any(
        isinstance(m, HumanMessage) and m.content == "How much is the invoice?" for m in history
    )


async def test_unsupported_mode_is_routed_by_code(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted()
    graph = build_graph(model, client, checkpointer=InMemorySaver())
    state = await graph.ainvoke(
        {"mode": "send", "request": {"question": "x"}}, {"configurable": {"thread_id": "t"}}
    )
    assert state["error"] == "unsupported_mode" and model.seen == []


async def no_evidence_check(scripted: ScriptFactory, client: JmapClient, first: str) -> dict:
    model = scripted(AIMessage(content=first), AIMessage(content=first))
    return await ask(model, client)


@pytest.mark.parametrize(
    "answer",
    [
        "Your invoice is 999 EUR.",
        "Your invoice is 999 EUR [email:bad.id].",
        "Your invoice is 999 EUR [Email: M3].",
        "There are 2 mailboxes [mailboxes].",
        "Nothing about invoices [searched].",
    ],
)
async def test_answers_without_valid_evidence_fail(
    scripted: ScriptFactory, client: JmapClient, answer: str
) -> None:
    result = await no_evidence_check(scripted, client, answer)
    assert result["status"] == "error" and result["error"] == "invalid_citations"


async def test_evidence_problem_is_explained_once(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    model = scripted(
        AIMessage(content="It is 999 EUR."),
        tool_turn(call("search_mail", {"text": "invoice"})),
        AIMessage(content="It is 42.00 EUR [email:M3]."),
    )
    result = await ask(model, client)
    assert result["status"] == "ok" and result["citations"] == ["M3"]
    note = model.seen[1][-1]
    assert isinstance(note, HumanMessage) and "no evidence marker" in note.content


@pytest.mark.parametrize(
    ("tool", "answer", "kind"),
    [
        (call("list_mailboxes", {}), "Inbox holds 5 emails [mailboxes].", "mailboxes"),
        (call("search_mail", {"text": "parking"}), "No parking fine found [searched].", "search"),
    ],
)
async def test_mailbox_and_search_evidence(
    scripted: ScriptFactory, client: JmapClient, tool: dict, answer: str, kind: str
) -> None:
    result = await ask(scripted(tool_turn(tool), AIMessage(content=answer)), client)
    assert result["status"] == "ok" and result["evidence"] == [kind]


async def test_no_mail_answers_are_labelled(scripted: ScriptFactory, client: JmapClient) -> None:
    result = await ask(scripted(AIMessage(content="Which invoice do you mean? [no-mail]")), client)
    assert result["status"] == "ok" and result["evidence"] == ["none"] and result["citations"] == []


class Flaky(FakeJmap):
    """Times out on the first `failures` API requests."""

    def __init__(self, failures: int) -> None:
        super().__init__()
        self.failures = failures

    def handle(self, request: httpx.Request) -> httpx.Response:
        if request.method == "POST" and self.failures:
            self.failures -= 1
            raise httpx.ReadTimeout("timeout", request=request)
        return super().handle(request)


def flaky_client(fake: FakeJmap) -> JmapClient:
    return JmapClient(SESSION_URL, "t", transport=fake.transport(), backoff=0)


async def test_transient_timeouts_are_retried(scripted: ScriptFactory) -> None:
    model = scripted(
        tool_turn(call("list_mailboxes", {})), AIMessage(content="Inbox: 5 [mailboxes].")
    )
    result = await ask(model, flaky_client(Flaky(failures=2)))
    assert result["status"] == "ok"


async def test_timeout_closes_the_call_and_the_thread_stays_usable(
    scripted: ScriptFactory,
) -> None:
    saver = InMemorySaver()
    first = scripted(tool_turn(call("list_mailboxes", {}, id="t1")))
    result = await ask(first, flaky_client(Flaky(failures=99)), saver=saver)
    assert result == {
        "status": "error",
        "error": "jmap_unavailable",
        "steps": 1,
        "tool_calls": 0,
        "tokens": 100,
    }
    # The scripted model rejects unanswered tool calls, like real providers.
    second = scripted(
        tool_turn(call("list_mailboxes", {}, id="t2")), AIMessage(content="5 emails [mailboxes].")
    )
    result = await ask(second, flaky_client(FakeJmap()), "And now?", saver=saver)
    assert result["status"] == "ok"
    assert not any(isinstance(m, ToolMessage) and m.tool_call_id == "t1" for m in second.seen[0])


async def test_interrupted_checkpoint_does_not_reach_the_model(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    # A crash after the model asked for a tool leaves an unanswered call.
    saver = InMemorySaver()
    graph = build_graph(scripted(), client, checkpointer=saver)
    config: Any = {"configurable": {"thread_id": "th-1"}}
    await graph.aupdate_state(
        config,
        {
            "messages": [
                HumanMessage(content="Old question?", name="question"),
                tool_turn(call("list_mailboxes", {}, id="lost")),
            ]
        },
    )
    model = scripted(AIMessage(content="Hello. [no-mail]"))
    result = await ask(model, client, "New question?", saver=saver)
    assert result["status"] == "ok"
    assert [m.content for m in model.seen[0][1:]] == ["Old question?", "New question?"]


async def test_context_keeps_only_recent_questions_and_answers(
    scripted: ScriptFactory, client: JmapClient
) -> None:
    saver = InMemorySaver()
    for n in range(CONTEXT_TURNS + 2):
        model = scripted(
            tool_turn(call("search_mail", {"text": "dinner"}, id=f"s{n}")),
            AIMessage(content=f"Answer {n} [email:M1]."),
        )
        assert (await ask(model, client, f"Question {n}?", saver=saver))["status"] == "ok"
    last = model.seen[0]
    assert not any(isinstance(m, ToolMessage) for m in last)
    assert [m.content for m in last[1:3]] == ["Question 1?", "Answer 1 [email:M1]."]
    assert len(last) == 1 + 2 * CONTEXT_TURNS + 1


async def test_saved_history_stays_bounded(scripted: ScriptFactory, client: JmapClient) -> None:
    saver = InMemorySaver()
    graph_sizes = []
    for n in range(2 * CONTEXT_TURNS + 5):
        model = scripted(
            tool_turn(call("read_email", {"ids": ["M3"]}, id=f"r{n}")),
            AIMessage(content=f"Answer {n} [email:M3]."),
        )
        assert (await ask(model, client, f"Question {n}?", saver=saver))["status"] == "ok"
        graph = build_graph(scripted(), client, checkpointer=saver)
        state = await graph.aget_state({"configurable": {"thread_id": "th-1"}})
        graph_sizes.append(len(state.values["messages"]))
    # The kept turns plus one run (question, tool call, tool reply, answer);
    # the tool output of earlier runs is gone from the checkpoint.
    assert max(graph_sizes) == 2 * CONTEXT_TURNS + 4
    messages = state.values["messages"]
    assert sum(isinstance(m, ToolMessage) for m in messages) == 1
