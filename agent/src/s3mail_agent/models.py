"""The one model factory, so a model change is configuration plus an evaluation run."""

from __future__ import annotations

from langchain_aws import ChatBedrockConverse
from langchain_core.language_models import BaseChatModel


def chat_model(model_id: str, region: str, *, max_tokens: int = 2048) -> BaseChatModel:
    """A Bedrock Converse model by pinned inference profile id."""
    return ChatBedrockConverse(
        model=model_id,
        region_name=region,
        temperature=0,
        max_tokens=max_tokens,
    )
