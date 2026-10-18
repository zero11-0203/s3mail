"""The agent graph (docs/specs/agent.md#langgraph-design): the ask (A0) and
act (A3) modes, both checkpointed per CLI thread.

ask: route (code) -> agent (model, read tools) <-> tools (code) -> check_evidence (code)
act: route -> agent (model, read + propose tools) <-> tools
       -> approval (interrupt) -> execute (code, once, on a matching hash)

The router picks the mode from the payload, never from a model. Tool calls
outside the mode's allowlist, budget overruns and answers without valid
evidence end the run with an error code. A propose tool never acts: code
builds the exact calls and their hash, and the run stops at the approval
interrupt. `agent approve` resumes the thread; LangGraph re-runs an
interrupted node from its start, so proposing, approving and executing are
separate nodes, and the executor runs only if the resumed hash matches.

Each run sends the model only the earlier turns' questions and accepted
answers plus the current run's messages. Earlier tool output and interrupted
tool calls stay in the checkpoint but never reach the model, so a failed run
cannot corrupt the next one and old excerpts are never fresh evidence.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Mapping, Sequence
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
from langgraph.types import Command, interrupt
from opentelemetry import trace
from pydantic import BaseModel, ValidationError

from .citations import NO_MAIL, check_evidence
from .jmap import GuardError, JmapAuthError, JmapClient, JmapError
from .metering import estimate_tokens, reported_tokens
from .prompts import ACT_SYSTEM, ASK_SYSTEM, EVIDENCE_RETRY
from .proposals import ProposalError, build_send, build_update, execute
from .proposals import propose_send as propose_send_args
from .proposals import propose_update as propose_update_args
from .store import AgentTable
from .tools import ASK_TOOLS, ToolSpec

MODES = frozenset({"ask", "act"})
PROPOSE_TOOLS: dict[str, type[BaseModel]] = {
    "propose_update": propose_update_args,
    "propose_send": propose_send_args,
}
NO_ACTION = "[no-action]"
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
ERR_NO_APPROVAL = "no_pending_approval"


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
    # A3: the proposal with its exact calls and hash, the owner's decision
    # from the resumed interrupt, and what the executor did.
    proposal: dict[str, Any] | None
    decision: dict[str, Any] | None
    outcome: dict[str, Any] | None


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
    read_tools: Mapping[str, ToolSpec] | None = None,
    propose: frozenset[str] = frozenset(),
    table: AgentTable | None = None,
) -> CompiledStateGraph[Any, Any, Any, Any]:
    """read_tools defaults to the A0 tools; propose names the propose tools
    the act mode may offer, by the scopes granted (none disables act)."""
    limits = limits or Limits()
    reads = dict(read_tools if read_tools is not None else ASK_TOOLS)
    proposing = {n: PROPOSE_TOOLS[n] for n in sorted(propose)}
    allowed = {"ask": set(reads), "act": set(reads) | set(proposing)}
    systems = {"ask": ASK_SYSTEM, "act": ACT_SYSTEM}
    bound = {
        "ask": model.bind_tools([s.args for s in reads.values()]),
        "act": model.bind_tools([*(s.args for s in reads.values()), *proposing.values()]),
    }

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
            "proposal": None,
            "decision": None,
            "outcome": None,
        }
        mode = state.get("mode")
        if mode not in MODES or (mode == "act" and not proposing):
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
        mode = state["mode"]
        prompt = [
            SystemMessage(content=systems[mode]),
            *model_context(state["messages"], state.get("run_start", 0)),
        ]
        estimate = estimate_tokens(prompt) + TOOL_SCHEMA_TOKENS
        # Admission: the request plus the completion room must still fit.
        if budget["tokens"] + estimate + limits.reserve_tokens > limits.max_tokens:
            return {"error": ERR_BUDGET}
        ai = await bound[mode].ainvoke(prompt)
        # Missing usage is charged at the estimate, never as zero.
        used = reported_tokens(ai, estimate)
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
        if any(c["name"] not in allowed[state["mode"]] for c in calls):
            return {
                "messages": _tool_failures(ai, "Tool not allowed."),
                "error": ERR_TOOL_NOT_ALLOWED,
            }
        if budget["tool_calls"] + len(calls) > limits.max_tool_calls:
            return {"messages": _tool_failures(ai, "Tool budget exhausted."), "error": ERR_BUDGET}

        fetched = set(state.get("fetched_ids", []))
        used = list(state.get("tools_used", []))
        proposal = state.get("proposal")
        replies: list[ToolMessage] = []
        for call in calls:
            name = call["name"]
            spec_args: type[BaseModel] = proposing[name] if name in proposing else reads[name].args
            try:
                args: Any = spec_args.model_validate(call["args"])
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
                with _tracer.start_as_current_span(f"tool {name}") as span:
                    span.set_attribute("s3mail.tool", name)
                    if name in proposing:
                        if proposal is not None:
                            raise ProposalError("one proposal per request")
                        if name == "propose_update":
                            proposal = await build_update(jmap, args, fetched)
                        else:
                            proposal = await build_send(jmap, args)
                        used.append(name)
                        replies.append(
                            ToolMessage(
                                content="Proposal recorded; the owner will approve or reject it.",
                                tool_call_id=call["id"],
                            )
                        )
                        continue
                    result = await reads[name].run(jmap, args)
                    span.set_attribute("s3mail.emails", len(result.email_ids))
            except ProposalError as exc:
                replies.append(
                    ToolMessage(
                        content=f"Proposal refused: {exc}.", tool_call_id=call["id"], status="error"
                    )
                )
                continue
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
                                    "tool": name,
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
            used.append(name)
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
            "proposal": proposal,
        }

    def check(state: AgentState) -> dict[str, Any]:
        ai = state["messages"][-1]
        text = ai.text if isinstance(ai, AIMessage) else ""
        # In act mode, a reply without a proposal may say it makes no claim
        # about mail by acting on nothing.
        checked = text.replace(NO_ACTION, NO_MAIL) if state["mode"] == "act" else text
        evidence = check_evidence(
            checked, state.get("fetched_ids", []), state.get("tools_used", [])
        )
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

    def approval(state: AgentState) -> dict[str, Any]:
        proposal = state["proposal"]
        assert proposal is not None
        decision = interrupt(
            {
                "hash": proposal["hash"],
                "kind": proposal["kind"],
                "summary": proposal["summary"],
                "expires_at": proposal["expires_at"],
            }
        )
        return {"decision": decision}

    async def run_execute(state: AgentState) -> dict[str, Any]:
        proposal = state["proposal"]
        assert proposal is not None and table is not None
        outcome = await execute(jmap, table, proposal, state.get("decision"))
        return {"outcome": outcome}

    def after_route(state: AgentState) -> Literal["agent", "__end__"]:
        return STOP if state.get("error") else "agent"

    def after_agent(state: AgentState) -> Literal["tools", "check_evidence", "__end__"]:
        if state.get("error"):
            return STOP
        last = state["messages"][-1]
        return "tools" if isinstance(last, AIMessage) and last.tool_calls else "check_evidence"

    def after_tools(state: AgentState) -> Literal["agent", "approval", "__end__"]:
        if state.get("error"):
            return STOP
        return "approval" if state.get("proposal") else "agent"

    def after_check(state: AgentState) -> Literal["agent", "__end__"]:
        if state.get("error") or state.get("answer") is not None:
            return STOP
        return "agent"

    graph = StateGraph(AgentState)
    graph.add_node("route", route)
    graph.add_node("agent", agent)
    graph.add_node("tools", tools)
    graph.add_node("check_evidence", check)
    graph.add_node("approval", approval)
    graph.add_node("execute", run_execute)
    graph.add_edge(START, "route")
    graph.add_conditional_edges("route", after_route)
    graph.add_conditional_edges("agent", after_agent)
    graph.add_conditional_edges("tools", after_tools)
    graph.add_conditional_edges("check_evidence", after_check)
    graph.add_edge("approval", "execute")
    graph.add_edge("execute", STOP)
    return graph.compile(checkpointer=checkpointer)


def _config(thread_id: str, actor_id: str, limits: Limits) -> Any:
    return {
        "configurable": {"thread_id": thread_id, "actor_id": actor_id},
        "recursion_limit": limits.recursion_limit,
    }


def _result(state: Mapping[str, Any]) -> dict[str, Any]:
    budget = state.get("budget", {})
    result: dict[str, Any] = {
        "status": "error" if state.get("error") else "ok",
        "steps": budget.get("steps", 0),
        "tool_calls": budget.get("tool_calls", 0),
        "tokens": budget.get("tokens", 0),
    }
    if state.get("error"):
        result["error"] = state["error"]
        return result
    interrupts = state.get("__interrupt__") or []
    if interrupts:
        result["status"] = "approval_required"
        result["approval"] = interrupts[0].value
        return result
    if state.get("outcome") is not None:
        result["outcome"] = state["outcome"]
        return result
    result["answer"] = state.get("answer")
    result["citations"] = state.get("citations", [])
    result["evidence"] = state.get("evidence", [])
    return result


async def run_ask(
    graph: CompiledStateGraph[Any, Any, Any, Any],
    question: str,
    *,
    thread_id: str,
    actor_id: str,
    limits: Limits | None = None,
    mode: str = "ask",
) -> dict[str, Any]:
    """Run one question (ask) or request (act) on a thread; return a
    content-free status plus the answer or the approval to show."""
    limits = limits or Limits()
    try:
        state = await graph.ainvoke(
            {"mode": mode, "request": {"question": question}},
            _config(thread_id, actor_id, limits),
        )
    except GraphRecursionError:
        return {"status": "error", "error": ERR_BUDGET}
    return _result(state)


async def run_approve(
    graph: CompiledStateGraph[Any, Any, Any, Any],
    *,
    thread_id: str,
    actor_id: str,
    approval_hash: str,
    approve: bool,
    limits: Limits | None = None,
) -> dict[str, Any]:
    """Resume a thread's pending approval with the owner's decision."""
    limits = limits or Limits()
    config = _config(thread_id, actor_id, limits)
    snapshot = await graph.aget_state(config)
    pending = snapshot.next or ()
    if "execute" in pending:
        # The executor was interrupted after the approval was recorded: re-run
        # it. The recorded decision still has to match, and the send marker
        # turns a repeated send into a report of the earlier attempt.
        recorded = snapshot.values.get("decision") or {}
        if recorded.get("hash") != approval_hash:
            return {"status": "error", "error": ERR_NO_APPROVAL}
        return _result(await graph.ainvoke(None, config))
    if "approval" not in pending:
        return {"status": "error", "error": ERR_NO_APPROVAL}
    state = await graph.ainvoke(Command(resume={"hash": approval_hash, "approve": approve}), config)
    return _result(state)
