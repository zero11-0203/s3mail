"""Token metering shared by every mode (docs/specs/agent.md#langgraph-design).

The token cap is a hard admission limit: before a call, a deliberately high
estimate of the request plus the completion reserve must fit, and a reply
whose reported usage overruns the cap is not used. Missing usage is charged
at the estimate, never as zero.
"""

from __future__ import annotations

import json
from collections.abc import Sequence
from dataclasses import dataclass, field

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import AIMessage, BaseMessage


class BudgetExceeded(Exception):
    """The run's model-call or token budget is spent."""


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


def reported_tokens(ai: AIMessage, estimate: int) -> int:
    usage = getattr(ai, "usage_metadata", None) or {}
    return int(usage.get("total_tokens") or 0) or estimate + estimate_tokens([ai])


@dataclass
class Meter:
    """Meters the model calls of one non-graph run (triage, digest, draft)."""

    max_calls: int
    max_tokens: int
    reserve_tokens: int = 2048
    calls: int = 0
    tokens: int = 0
    _log: list[int] = field(default_factory=list)

    async def invoke(self, model: BaseChatModel, messages: Sequence[BaseMessage]) -> AIMessage:
        if self.calls >= self.max_calls:
            raise BudgetExceeded
        estimate = estimate_tokens(messages)
        if self.tokens + estimate + self.reserve_tokens > self.max_tokens:
            raise BudgetExceeded
        ai = await model.ainvoke(list(messages))
        assert isinstance(ai, AIMessage)
        self.calls += 1
        self.tokens += reported_tokens(ai, estimate)
        if self.tokens > self.max_tokens:
            raise BudgetExceeded
        return ai
