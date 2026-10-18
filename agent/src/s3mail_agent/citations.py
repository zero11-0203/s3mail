"""Answer evidence (docs/specs/agent.md#answer-evidence).

Every answer carries at least one marker, and each marker must be backed by
a tool that ran in this run:

    [email:<id>]  an email a tool returned in this run
    [mailboxes]   mailbox names or counts from list_mailboxes
    [searched]    a search in this run that found nothing relevant
    [no-mail]     the answer makes no statement about mail

An answer without a marker, a malformed email marker, or a marker without
its tool fails the check. Membership is all the check can prove: whether a
cited email supports the claim is measured by the evaluation suite.
"""

from __future__ import annotations

import re
from collections.abc import Iterable
from dataclasses import dataclass

# JMAP ids use the URL-safe base64 alphabet (RFC 8620 section 1.2).
CITATION = re.compile(r"\[email:([A-Za-z0-9_-]{1,255})\]")
# Anything that looks like an attempt at an email marker.
EMAIL_MARKER = re.compile(r"\[\s*email\s*:[^\]\n]{0,300}\]?", re.IGNORECASE)
MAILBOXES = "[mailboxes]"
SEARCHED = "[searched]"
NO_MAIL = "[no-mail]"


@dataclass(frozen=True)
class Evidence:
    emails: list[str]
    kinds: list[str]
    problems: list[str]


def cited_ids(text: str) -> list[str]:
    """Cited ids in order of first appearance."""
    return list(dict.fromkeys(CITATION.findall(text)))


def check_evidence(text: str, fetched: Iterable[str], tools_used: Iterable[str]) -> Evidence:
    allowed, used = set(fetched), set(tools_used)
    emails = cited_ids(text)
    problems: list[str] = []
    kinds: list[str] = []

    malformed = len(EMAIL_MARKER.findall(text)) - len(CITATION.findall(text))
    if malformed > 0:
        problems.append("a citation is malformed; write [email:<exact id>]")
    unfetched = [i for i in emails if i not in allowed]
    if unfetched:
        problems.append(f"{', '.join(unfetched)} not returned by a tool in this run")
    if emails:
        kinds.append("email")
    if MAILBOXES in text:
        if "list_mailboxes" in used:
            kinds.append("mailboxes")
        else:
            problems.append("[mailboxes] needs list_mailboxes in this run")
    if SEARCHED in text:
        if "search_mail" in used:
            kinds.append("search")
        else:
            problems.append("[searched] needs search_mail in this run")
    if NO_MAIL in text:
        kinds.append("none")
    if not kinds and not problems:
        problems.append("the answer has no evidence marker")
    return Evidence(emails, kinds, problems)
