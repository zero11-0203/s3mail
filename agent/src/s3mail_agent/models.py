"""The one model factory, so a model change is configuration plus an evaluation run.

Models are served through OpenRouter's OpenAI-compatible API
(docs/specs/agent.md#architecture). Every request asks OpenRouter to route
only to providers that do not collect prompt data, because prompts carry
mail excerpts.
"""

from __future__ import annotations

import json

import boto3
import httpx
from langchain_core.language_models import BaseChatModel
from langchain_openai import ChatOpenAI
from pydantic import SecretStr

# OpenRouter provider routing: never send prompts to a provider that may
# store or train on them.
PROVIDER_ROUTING = {"provider": {"data_collection": "deny"}}


def chat_model(
    model: str,
    api_key: str,
    base_url: str,
    *,
    max_tokens: int = 2048,
    http_client: httpx.Client | None = None,
) -> BaseChatModel:
    """A chat model by its pinned OpenRouter id, for example openai/gpt-6.1-sol."""
    return ChatOpenAI(
        http_client=http_client,
        model=model,
        api_key=SecretStr(api_key),
        base_url=base_url,
        max_completion_tokens=max_tokens,
        timeout=60,
        max_retries=2,
        extra_body=PROVIDER_ROUTING,
        default_headers={"X-Title": "s3mail-agent"},
    )


def read_api_key(secret_arn: str, region: str) -> str:
    """The model API key from the owner-set {"api_key": ...} secret."""
    client = boto3.client("secretsmanager", region_name=region)
    value = client.get_secret_value(SecretId=secret_arn)["SecretString"]
    key: str = json.loads(value)["api_key"]
    return key
