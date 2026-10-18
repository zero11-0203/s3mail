"""Citation parsing: every cited email id must have been fetched in this run."""

from __future__ import annotations

import re
from collections.abc import Iterable

# JMAP ids use the URL-safe base64 alphabet (RFC 8620 section 1.2).
CITATION = re.compile(r"\[email:([A-Za-z0-9_-]{1,255})\]")


def cited_ids(text: str) -> list[str]:
    """Cited ids in order of first appearance."""
    return list(dict.fromkeys(CITATION.findall(text)))


def invalid_citations(text: str, fetched: Iterable[str]) -> list[str]:
    allowed = set(fetched)
    return [i for i in cited_ids(text) if i not in allowed]
