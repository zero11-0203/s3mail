"""The agent graph (docs/specs/agent.md#langgraph-design). A0 builds the ask mode.

route (code) -> agent (model, read tools) <-> tools (code) -> check_evidence (code)

The router picks the mode from the payload, never from a model. Tool calls
outside the mode's allowlist, budget overruns and answers without valid
evidence end the run with an error code. The proposal, approval and send_key
state fields arrive with approved actions in A3.

Each run sends the model only the earlier turns' questions and accepted
answers plus the current run's messages. Earlier tool output and interrupted
tool calls stay in the checkpoint but never reach the model, so a failed run
cannot corrupt the next one and old excerpts are never fresh evidence.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from dataclasses import dataclass
from typing import Annotated, Any, Literal, TypedDict

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import (
    AIMessage,
    AnyMessage,
    BaseMessage,
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

from .citations import check_evidence
from .jmap import GuardError, JmapAuthError, JmapClient, JmapError
from .prompts import ASK_SYSTEM, EVIDENCE_RETRY
from .tools import ASK_TOOLS

MODES = frozenset({"ask"})
MAX_QUESTION_CHARS = 4000
EVIDENCE_RETRIES = 1
# Earlier question/answer pairs sent with each request.
CONTEXT_TURNS = 10
# Allowance for the tool schemas sent with every request.
TOOL_SCHEMA_TOKENS = 600
# Message names that mark turns kept in the model context.
QUESTION, ANSWER, CHECK = "question", "answer", "evidence_check"
# langgraph.graph.END, typed for the routing functions below.
STOP: Literal["__end__"] = "__end__"

log = logging.getLogger("s3mail_agent")
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
    run_start: int
    fetched_ids: list[str]
    tools_used: list[str]
    budget: Budget
    citation_retries: int
    answer: str | None
    citations: list[str]
    evidence: list[str]
    error: str | None


@dataclass(frozen=True)
class Limits:
    max_steps: int = 12
    max_tool_calls: int = 24
    max_tokens: int = 200_000
    # Completion room kept free before each call: the model's output cap.
    reserve_tokens: int = 2048

    @property
    def recursion_limit(self) -> int:
        # Each step visits agent and tools or check_evidence, plus slack.
        return 3 * self.max_steps + 10


def estimate_tokens(messages: Sequence[BaseMessage]) -> int:
    """A deliberately high estimate: ASCII at 3 characters a token, other
    characters (CJK above all) at one token each."""
    total = 0
    for m in messages:
        text = m.content if isinstance(m.content, str) else json.dumps(m.content)
        if isinstance(m, AIMessage) and m.tool_calls:
            text += json.dumps(m.tool_calls, default=str)
        ascii_chars = sum(c.isascii() for c in text)
        total += ascii_chars // 3 + (len(text) - ascii_chars) + 8
    return total


def model_context(messages: Sequence[AnyMessage], run_start: int) -> list[AnyMessage]:
    earlier = [m for m in messages[:run_start] if m.name in (QUESTION, ANSWER)]
    return [*earlier[-2 * CONTEXT_TURNS :], *messages[run_start:]]


def _tool_failures(ai: AIMessage, reason: str) -> list[ToolMessage]:
    # Answer every pending call so the run's history stays a valid
    # conversation for the model.
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
            "run_start": len(state.get("messages", [])),
            "fetched_ids": [],
            "tools_used": [],
            "budget": Budget(steps=0, tool_calls=0, tokens=0),
            "citation_retries": 0,
            "answer": None,
            "citations": [],
            "evidence": [],
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
        return {**reset, "messages": [HumanMessage(content=question, name=QUESTION)]}

    async def agent(state: AgentState) -> dict[str, Any]:
        budget = state["budget"]
        if budget["steps"] >= limits.max_steps:
            return {"error": ERR_BUDGET}
        prompt = [
            SystemMessage(content=ASK_SYSTEM),
            *model_context(state["messages"], state.get("run_start", 0)),
        ]
        estimate = estimate_tokens(prompt) + TOOL_SCHEMA_TOKENS
        # Admission: the request plus the completion room must still fit.
        if budget["tokens"] + estimate + limits.reserve_tokens > limits.max_tokens:
            return {"error": ERR_BUDGET}
        ai = await bound.ainvoke(prompt)
        usage = getattr(ai, "usage_metadata", None) or {}
        # Missing usage is charged at the estimate, never as zero.
        used = int(usage.get("total_tokens") or 0) or estimate + estimate_tokens([ai])
        spent = Budget(
            steps=budget["steps"] + 1,
            tool_calls=budget["tool_calls"],
            tokens=budget["tokens"] + used,
        )
        update: dict[str, Any] = {"messages": [ai], "budget": spent}
        if spent["tokens"] > limits.max_tokens:
            # An overrun ends the run before its tool calls or answer are used.
            update["error"] = ERR_BUDGET
        return update

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
        used = list(state.get("tools_used", []))
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
            except Exception as exc:
                if isinstance(exc, GuardError):
                    code = ERR_TOOL_NOT_ALLOWED
                elif isinstance(exc, JmapAuthError):
                    code = ERR_AUTH
                else:
                    code = ERR_JMAP
                    if not isinstance(exc, JmapError):
                        log.error(
                            json.dumps(
                                {
                                    "event": "tool_failed",
                                    "tool": call["name"],
                                    "exception": type(exc).__name__,
                                }
                            )
                        )
                # Close this call and every later one, so no tool call is
                # left without a reply.
                done = {r.tool_call_id for r in replies}
                rest = [c for c in calls if c["id"] not in done]
                failed = AIMessage(content="", tool_calls=rest)
                return {
                    "messages": [*replies, *_tool_failures(failed, "Tool failed.")],
                    "error": code,
                }
            fetched |= result.email_ids
            used.append(call["name"])
            replies.append(ToolMessage(content=result.content, tool_call_id=call["id"]))

        spent = Budget(
            steps=budget["steps"],
            tool_calls=budget["tool_calls"] + len(calls),
            tokens=budget["tokens"],
        )
        return {
            "messages": replies,
            "fetched_ids": sorted(fetched),
            "tools_used": sorted(set(used)),
            "budget": spent,
        }

    def check(state: AgentState) -> dict[str, Any]:
        ai = state["messages"][-1]
        text = ai.text if isinstance(ai, AIMessage) else ""
        evidence = check_evidence(text, state.get("fetched_ids", []), state.get("tools_used", []))
        if not evidence.problems and isinstance(ai, AIMessage):
            return {
                # Same id, so this replaces the message: named answers are
                # the ones later turns keep in their context.
                "messages": [ai.model_copy(update={"name": ANSWER})],
                "answer": text,
                "citations": evidence.emails,
                "evidence": evidence.kinds,
            }
        retries = state.get("citation_retries", 0)
        if retries >= EVIDENCE_RETRIES:
            return {"error": ERR_CITATIONS}
        note = EVIDENCE_RETRY.format(problems="; ".join(evidence.problems))
        return {
            "messages": [HumanMessage(content=note, name=CHECK)],
            "citation_retries": retries + 1,
        }

    def after_route(state: AgentState) -> Literal["agent", "__end__"]:
        return STOP if state.get("error") else "agent"

    def after_agent(state: AgentState) -> Literal["tools", "check_evidence", "__end__"]:
        if state.get("error"):
            return STOP
        last = state["messages"][-1]
        return "tools" if isinstance(last, AIMessage) and last.tool_calls else "check_evidence"

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
    graph.add_node("check_evidence", check)
    graph.add_edge(START, "route")
    graph.add_conditional_edges("route", after_route)
    graph.add_conditional_edges("agent", after_agent)
    graph.add_conditional_edges("tools", after_tools)
    graph.add_conditional_edges("check_evidence", after_check)
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
        result["evidence"] = state.get("evidence", [])
    return result
