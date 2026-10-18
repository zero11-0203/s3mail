"""The agent graph (docs/specs/agent.md#langgraph-design). A0 builds the ask mode.

route (code) -> agent (model, read tools) <-> tools (code) -> check_citations (code)

The router picks the mode from the payload, never from a model. Tool calls
outside the mode's allowlist, budget overruns and invalid citations end the
run with an error code. The proposal, approval and send_key state fields
arrive with approved actions in A3.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Annotated, Any, Literal, TypedDict

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import (
    AIMessage,
    AnyMessage,
    HumanMessage,
    SystemMessage,
    ToolMessage,
)
from langgraph.checkpoint.base import BaseCheckpointSaver
from langgraph.errors import GraphRecursionError
from langgraph.graph import START, StateGraph
from langgraph.graph.message import add_messages
from langgraph.graph.state import CompiledStateGraph
from opentelemetry import trace
from pydantic import ValidationError

from .citations import cited_ids, invalid_citations
from .jmap import GuardError, JmapAuthError, JmapClient, JmapError
from .prompts import ASK_SYSTEM, CITATION_RETRY
from .tools import ASK_TOOLS

MODES = frozenset({"ask"})
MAX_QUESTION_CHARS = 4000
CITATION_RETRIES = 1
# langgraph.graph.END, typed for the routing functions below.
STOP: Literal["__end__"] = "__end__"

_tracer = trace.get_tracer("s3mail_agent")

# Error codes returned to the caller; also the alarm dimensions.
ERR_UNSUPPORTED_MODE = "unsupported_mode"
ERR_BAD_REQUEST = "bad_request"
ERR_BUDGET = "budget_exceeded"
ERR_TOOL_NOT_ALLOWED = "tool_not_allowed"
ERR_CITATIONS = "invalid_citations"
ERR_AUTH = "authorization_failed"
ERR_JMAP = "jmap_unavailable"


class Budget(TypedDict):
    steps: int
    tool_calls: int
    tokens: int


class AgentState(TypedDict, total=False):
    mode: str
    request: dict[str, Any]
    messages: Annotated[list[AnyMessage], add_messages]
    fetched_ids: list[str]
    budget: Budget
    citation_retries: int
    answer: str | None
    citations: list[str]
    error: str | None


@dataclass(frozen=True)
class Limits:
    max_steps: int = 12
    max_tool_calls: int = 24
    max_tokens: int = 200_000

    @property
    def recursion_limit(self) -> int:
        # Each step visits agent and tools or check_citations, plus slack.
        return 3 * self.max_steps + 10


def _tool_failures(ai: AIMessage, reason: str) -> list[ToolMessage]:
    # Answer every pending call so the stored history stays a valid
    # conversation for the next turn in this thread.
    return [
        ToolMessage(content=reason, tool_call_id=c["id"], status="error") for c in ai.tool_calls
    ]


def build_graph(
    model: BaseChatModel,
    jmap: JmapClient,
    *,
    limits: Limits | None = None,
    checkpointer: BaseCheckpointSaver[Any] | None = None,
) -> CompiledStateGraph[Any, Any, Any, Any]:
    limits = limits or Limits()
    bound = model.bind_tools([spec.args for spec in ASK_TOOLS.values()])

    def route(state: AgentState) -> dict[str, Any]:
        reset: dict[str, Any] = {
            "fetched_ids": [],
            "budget": Budget(steps=0, tool_calls=0, tokens=0),
            "citation_retries": 0,
            "answer": None,
            "citations": [],
            "error": None,
        }
        mode = state.get("mode")
        if mode not in MODES:
            return {**reset, "error": ERR_UNSUPPORTED_MODE}
        question = state.get("request", {}).get("question")
        if not isinstance(question, str) or not question.strip():
            return {**reset, "error": ERR_BAD_REQUEST}
        if len(question) > MAX_QUESTION_CHARS:
            return {**reset, "error": ERR_BAD_REQUEST}
        return {**reset, "messages": [HumanMessage(content=question)]}

    async def agent(state: AgentState) -> dict[str, Any]:
        budget = state["budget"]
        if budget["steps"] >= limits.max_steps or budget["tokens"] >= limits.max_tokens:
            return {"error": ERR_BUDGET}
        ai = await bound.ainvoke([SystemMessage(content=ASK_SYSTEM), *state["messages"]])
        usage = getattr(ai, "usage_metadata", None) or {}
        spent = Budget(
            steps=budget["steps"] + 1,
            tool_calls=budget["tool_calls"],
            tokens=budget["tokens"] + int(usage.get("total_tokens", 0)),
        )
        return {"messages": [ai], "budget": spent}

    async def tools(state: AgentState) -> dict[str, Any]:
        ai = state["messages"][-1]
        assert isinstance(ai, AIMessage)
        budget = state["budget"]
        calls = ai.tool_calls
        if any(c["name"] not in ASK_TOOLS for c in calls):
            return {
                "messages": _tool_failures(ai, "Tool not allowed."),
                "error": ERR_TOOL_NOT_ALLOWED,
            }
        if budget["tool_calls"] + len(calls) > limits.max_tool_calls:
            return {"messages": _tool_failures(ai, "Tool budget exhausted."), "error": ERR_BUDGET}

        fetched = set(state.get("fetched_ids", []))
        replies: list[ToolMessage] = []
        for call in calls:
            spec = ASK_TOOLS[call["name"]]
            try:
                args = spec.args.model_validate(call["args"])
            except ValidationError as exc:
                fields = ", ".join(str(e["loc"][0]) for e in exc.errors() if e["loc"])
                replies.append(
                    ToolMessage(
                        content=f"Invalid arguments: {fields or 'unknown'}.",
                        tool_call_id=call["id"],
                        status="error",
                    )
                )
                continue
            try:
                # Content-free span: the tool name and result count only.
                with _tracer.start_as_current_span(f"tool {call['name']}") as span:
                    span.set_attribute("s3mail.tool", call["name"])
                    result = await spec.run(jmap, args)
                    span.set_attribute("s3mail.emails", len(result.email_ids))
            except (JmapAuthError, GuardError, JmapError) as exc:
                code = ERR_AUTH if isinstance(exc, JmapAuthError) else ERR_JMAP
                if isinstance(exc, GuardError):
                    code = ERR_TOOL_NOT_ALLOWED
                done = {r.tool_call_id for r in replies}
                rest = [c for c in calls if c["id"] not in done]
                failed = AIMessage(content="", tool_calls=rest)
                return {
                    "messages": [*replies, *_tool_failures(failed, "Tool failed.")],
                    "error": code,
                }
            fetched |= result.email_ids
            replies.append(ToolMessage(content=result.content, tool_call_id=call["id"]))

        spent = Budget(
            steps=budget["steps"],
            tool_calls=budget["tool_calls"] + len(calls),
            tokens=budget["tokens"],
        )
        return {"messages": replies, "fetched_ids": sorted(fetched), "budget": spent}

    def check_citations(state: AgentState) -> dict[str, Any]:
        ai = state["messages"][-1]
        text = ai.text if isinstance(ai, AIMessage) else ""
        bad = invalid_citations(text, state.get("fetched_ids", []))
        if not bad:
            return {"answer": text, "citations": cited_ids(text)}
        retries = state.get("citation_retries", 0)
        if retries >= CITATION_RETRIES:
            return {"error": ERR_CITATIONS}
        note = CITATION_RETRY.format(ids=", ".join(bad), verb="was" if len(bad) == 1 else "were")
        return {"messages": [HumanMessage(content=note)], "citation_retries": retries + 1}

    def after_route(state: AgentState) -> Literal["agent", "__end__"]:
        return STOP if state.get("error") else "agent"

    def after_agent(state: AgentState) -> Literal["tools", "check_citations", "__end__"]:
        if state.get("error"):
            return STOP
        last = state["messages"][-1]
        return "tools" if isinstance(last, AIMessage) and last.tool_calls else "check_citations"

    def after_tools(state: AgentState) -> Literal["agent", "__end__"]:
        return STOP if state.get("error") else "agent"

    def after_check(state: AgentState) -> Literal["agent", "__end__"]:
        if state.get("error") or state.get("answer") is not None:
            return STOP
        return "agent"

    graph = StateGraph(AgentState)
    graph.add_node("route", route)
    graph.add_node("agent", agent)
    graph.add_node("tools", tools)
    graph.add_node("check_citations", check_citations)
    graph.add_edge(START, "route")
    graph.add_conditional_edges("route", after_route)
    graph.add_conditional_edges("agent", after_agent)
    graph.add_conditional_edges("tools", after_tools)
    graph.add_conditional_edges("check_citations", after_check)
    return graph.compile(checkpointer=checkpointer)


async def run_ask(
    graph: CompiledStateGraph[Any, Any, Any, Any],
    question: str,
    *,
    thread_id: str,
    actor_id: str,
    limits: Limits | None = None,
) -> dict[str, Any]:
    """Run one question on a thread; return a content-free status plus the answer."""
    limits = limits or Limits()
    config: Any = {
        "configurable": {"thread_id": thread_id, "actor_id": actor_id},
        "recursion_limit": limits.recursion_limit,
    }
    try:
        state = await graph.ainvoke({"mode": "ask", "request": {"question": question}}, config)
    except GraphRecursionError:
        return {"status": "error", "error": ERR_BUDGET}
    budget = state.get("budget", {})
    result: dict[str, Any] = {
        "status": "error" if state.get("error") else "ok",
        "steps": budget.get("steps", 0),
        "tool_calls": budget.get("tool_calls", 0),
        "tokens": budget.get("tokens", 0),
    }
    if state.get("error"):
        result["error"] = state["error"]
    else:
        result["answer"] = state.get("answer")
        result["citations"] = state.get("citations", [])
    return result
