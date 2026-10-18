"""Event-driven triage (A1, docs/specs/agent.md#features).

fetch email (code) -> classify (model, at most 3 calls) -> write triage row (code)

One run per EmailReceived event, on a fresh, uncheckpointed run: only the
triage row survives. The row is keyed by email id and written only if
absent, so duplicate and replayed events, including a DLQ redrive, never
produce a second row, and a duplicate is detected before any model call.
"""

from __future__ import annotations

import json
import re
from typing import Any, Literal

from langchain_core.language_models import BaseChatModel
from langchain_core.messages import HumanMessage, SystemMessage
from pydantic import BaseModel, ConfigDict, Field, ValidationError

from .jmap import JmapClient
from .metering import Meter
from .prompts import TRIAGE_RETRY, TRIAGE_SYSTEM
from .store import AgentTable, TriageRow
from .tools import HEADER_PROPERTIES, body_text, envelope, header_of

TRIAGE_BODY_BYTES = 8 * 1024
TRIAGE_MODEL_CALLS = 3
TRIAGE_MAX_TOKENS = 30_000
# JMAP ids (RFC 8620 section 1.2) and s3mail event ids (<emailId>:received).
EMAIL_ID = re.compile(r"[A-Za-z0-9_-]{1,255}")
EVENT_ID = re.compile(r"[A-Za-z0-9_:-]{1,300}")

Category = Literal[
    "personal", "work", "finance", "travel", "shopping", "newsletter", "notification", "spam"
]
CATEGORIES: tuple[str, ...] = Category.__args__  # type: ignore[attr-defined]
CONTROL = re.compile(r"[\x00-\x1f\x7f-\x9f‪-‮⁦-⁩]")


class Triage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    category: Category
    priority: Literal["high", "normal", "low"]
    needs_reply: bool
    summary: str = Field(min_length=1, max_length=200)


class TriageFailed(Exception):
    """The model gave no valid classification within its calls."""


def parse_triage(text: str) -> Triage:
    """The model's JSON object, tolerating a Markdown code fence."""
    body = text.strip()
    fence = re.fullmatch(r"```(?:json)?\s*(.*?)\s*```", body, re.DOTALL)
    if fence:
        body = fence.group(1)
    triage = Triage.model_validate(json.loads(body))
    # One line of plain text: the digest prints it.
    summary = " ".join(CONTROL.sub(" ", triage.summary).split())
    return triage.model_copy(update={"summary": summary})


async def classify(model: BaseChatModel, meter: Meter, email: dict[str, Any]) -> Triage:
    messages: list[Any] = [
        SystemMessage(content=TRIAGE_SYSTEM),
        HumanMessage(content=envelope(email)),
    ]
    while True:
        ai = await meter.invoke(model, messages)
        try:
            return parse_triage(ai.text)
        except (ValueError, ValidationError):
            if meter.calls >= meter.max_calls:
                raise TriageFailed from None
            messages = [*messages, ai, HumanMessage(content=TRIAGE_RETRY)]


async def run_triage(
    jmap: JmapClient,
    model: BaseChatModel,
    table: AgentTable,
    event: dict[str, Any],
) -> dict[str, Any]:
    """Triage one event. Returns a content-free result."""
    email_id, event_id = event.get("emailId"), event.get("eventId")
    if not (isinstance(email_id, str) and EMAIL_ID.fullmatch(email_id)):
        return {"status": "error", "error": "bad_request"}
    if not (isinstance(event_id, str) and EVENT_ID.fullmatch(event_id)):
        return {"status": "error", "error": "bad_request"}
    if await table.has_triage(email_id):
        return {"status": "ok", "triage": "duplicate", "model_calls": 0, "tokens": 0}

    results = await jmap.call(
        [
            (
                "Email/get",
                {
                    "ids": [email_id],
                    "properties": [*HEADER_PROPERTIES, "textBody", "bodyValues"],
                    "fetchTextBodyValues": True,
                    "maxBodyValueBytes": TRIAGE_BODY_BYTES,
                },
            )
        ]
    )
    found = results["c0"].get("list", [])
    if not found:
        # Destroyed before triage: nothing to keep.
        return {"status": "ok", "triage": "not_found", "model_calls": 0, "tokens": 0}
    email = found[0]
    body, truncated = body_text(email, TRIAGE_BODY_BYTES)
    meter = Meter(max_calls=TRIAGE_MODEL_CALLS, max_tokens=TRIAGE_MAX_TOKENS)
    triage = await classify(
        model, meter, {**header_of(email), "body": body, "truncated": truncated}
    )
    row = TriageRow(
        email_id=email_id,
        event_id=event_id,
        thread_id=str(email.get("threadId") or ""),
        received_at=str(email.get("receivedAt") or ""),
        category=triage.category,
        priority=triage.priority,
        needs_reply=triage.needs_reply,
        summary=triage.summary,
    )
    stored = await table.put_triage(row)
    return {
        "status": "ok",
        # A concurrent duplicate may have won the conditional write.
        "triage": "stored" if stored else "duplicate",
        "model_calls": meter.calls,
        "tokens": meter.tokens,
    }
