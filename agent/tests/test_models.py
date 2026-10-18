from __future__ import annotations

import json
from typing import Any

import httpx

from s3mail_agent import models
from s3mail_agent.models import PROVIDER_ROUTING, chat_model


def test_requests_deny_data_collection_and_carry_tools(monkeypatch: Any) -> None:
    """Every OpenRouter request asks for no-data-collection providers."""
    sent: list[dict[str, Any]] = []

    def handle(request: httpx.Request) -> httpx.Response:
        sent.append(json.loads(request.content))
        assert request.headers["Authorization"] == "Bearer test-key"
        return httpx.Response(
            200,
            json={
                "id": "r1",
                "object": "chat.completion",
                "created": 0,
                "model": "openai/gpt-6.1-sol",
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": "ok"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
            },
        )

    model = chat_model(
        "openai/gpt-6.1-sol",
        "test-key",
        "https://openrouter.example.com/api/v1",
        http_client=httpx.Client(transport=httpx.MockTransport(handle)),
    )

    from s3mail_agent.tools import ASK_TOOLS

    reply = model.bind_tools([s.args for s in ASK_TOOLS.values()]).invoke("hi")
    assert reply.usage_metadata and reply.usage_metadata["total_tokens"] == 4
    body = sent[0]
    assert body["provider"] == PROVIDER_ROUTING["provider"]
    assert body["model"] == "openai/gpt-6.1-sol" and "temperature" not in body
    assert {t["function"]["name"] for t in body["tools"]} == set(ASK_TOOLS)


def test_reads_the_owner_set_key(monkeypatch: Any) -> None:
    class FakeSecrets:
        def get_secret_value(self, SecretId: str) -> dict[str, str]:
            assert SecretId == "arn:example"
            return {"SecretString": json.dumps({"api_key": "sk-or-test"})}

    monkeypatch.setattr(models.boto3, "client", lambda *a, **k: FakeSecrets())
    assert models.read_api_key("arn:example", "us-east-1") == "sk-or-test"
