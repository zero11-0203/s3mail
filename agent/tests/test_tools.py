from __future__ import annotations

import json
from datetime import date

from s3mail_agent.jmap import MAX_BODY_VALUE_BYTES, JmapClient
from s3mail_agent.tools import (
    ASK_TOOLS,
    DATA_CLOSE,
    DATA_OPEN,
    html_to_text,
    read_email,
    read_thread,
    run_read_email,
    run_read_thread,
    run_search_mail,
    search_mail,
)

from .conftest import FakeJmap


def payload(content: str) -> dict:
    assert content.startswith(DATA_OPEN + "\n") and content.endswith("\n" + DATA_CLOSE)
    return json.loads(content[len(DATA_OPEN) : -len(DATA_CLOSE)])


async def test_search_is_one_batched_request(client: JmapClient, fake: FakeJmap) -> None:
    result = await run_search_mail(client, search_mail(text="dinner"))
    assert [name for name, _ in fake.calls] == ["Email/query", "Email/get"]
    assert {e["id"] for e in payload(result.content)["emails"]} == {"M1", "M2"}
    assert result.email_ids == {"M1", "M2"}


async def test_search_filters_by_sender_and_dates(client: JmapClient, fake: FakeJmap) -> None:
    args = search_mail.model_validate(
        {"from": "billing", "after": "2026-10-02", "before": date(2026, 10, 6)}
    )
    result = await run_search_mail(client, args)
    assert result.email_ids == {"M3"}
    condition = fake.calls[0][1]["filter"]
    assert condition == {
        "from": "billing",
        "after": "2026-10-02T00:00:00Z",
        "before": "2026-10-06T00:00:00Z",
    }


async def test_read_email_caps_bodies_and_converts_html(client: JmapClient, fake: FakeJmap) -> None:
    result = await run_read_email(client, read_email(ids=["M3", "missing"]))
    args = fake.calls[0][1]
    assert args["maxBodyValueBytes"] == MAX_BODY_VALUE_BYTES and args["fetchTextBodyValues"]
    data = payload(result.content)
    assert data["emails"][0]["body"] == "Your invoice total is 42.00 EUR."
    assert data["notFound"] == ["missing"]
    assert result.email_ids == {"M3"}


async def test_read_thread_uses_result_reference(client: JmapClient, fake: FakeJmap) -> None:
    result = await run_read_thread(client, read_thread(thread_id="T1"))
    assert [name for name, _ in fake.calls] == ["Thread/get", "Email/get"]
    assert [e["id"] for e in payload(result.content)["emails"]] == ["M1", "M2"]


async def test_mail_cannot_close_the_data_block(client: JmapClient) -> None:
    result = await run_read_email(client, read_email(ids=["M4"]))
    inner = result.content[len(DATA_OPEN) : -len(DATA_CLOSE)]
    assert "<" not in inner
    assert "IGNORE ALL PREVIOUS INSTRUCTIONS" in payload(result.content)["emails"][0]["body"]


async def test_cjk_text_survives(client: JmapClient) -> None:
    result = await run_read_email(client, read_email(ids=["M5"]))
    assert "京都" in result.content


def test_html_to_text_drops_scripts_and_keeps_links_as_text() -> None:
    text = html_to_text('<p>Hi</p><script>x()</script><a href="https://example.com/a">here</a>')
    assert text == "Hi\n<https://example.com/a> here"


def test_tool_arguments_reject_raw_jmap() -> None:
    for spec in ASK_TOOLS.values():
        schema = spec.args.model_json_schema()
        assert schema.get("additionalProperties") is False
        assert "filter" not in schema.get("properties", {})
