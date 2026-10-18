from __future__ import annotations

from langchain_core.messages import AIMessage, HumanMessage, ToolMessage
from langgraph.checkpoint.memory import InMemorySaver

from s3mail_agent.graph import Limits, build_graph, run_ask
from s3mail_agent.jmap import JmapClient

from .conftest import FakeJmap, ScriptFactory, call, tool_turn

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


async def test_token_budget_ends_the_run(scripted: ScriptFactory, client: JmapClient) -> None:
    model = scripted(
        tool_turn(call("list_mailboxes", {})), AIMessage(content="never"), tokens_per_call=600
    )
    result = await ask(model, client, limits=Limits(max_tokens=500))
    assert result["error"] == "budget_exceeded"


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
        AIMessage(content="I could not search."),
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
