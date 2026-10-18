"""Attachment text (A4, docs/specs/agent.md#features).

read_attachment downloads one attachment of an email found by the model,
within size caps, and extracts its text: plain text and HTML, PDF (pypdf,
first pages only) and Office Open XML documents (docx, xlsx, pptx). Office
XML is read with patterns rather than an XML parser, so entity expansion
cannot occur, and archive members are size-checked before they are read.
The text reaches the model as untrusted data like any mail body.
"""

from __future__ import annotations

import asyncio
import html
import io
import re
import zipfile
from typing import Any

from pydantic import Field

from .jmap import JmapClient
from .tools import ToolResult, _Args, envelope, html_to_text

MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024
MAX_TEXT_BYTES = 32 * 1024
MAX_PDF_PAGES = 30
MAX_UNZIPPED_BYTES = 40 * 1024 * 1024
EXTRACT_SECONDS = 20

TEXT_TYPES = frozenset({"text/plain", "text/csv", "text/markdown", "application/json"})
OOXML = {
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
    "application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
}
_TAGS = {
    "docx": (
        re.compile(r"^word/document\.xml$"),
        re.compile(r"<w:(t|p)\b[^>]*?(/?)>(?:([^<]*)</w:t>)?"),
    ),
    "pptx": (
        re.compile(r"^ppt/slides/slide\d+\.xml$"),
        re.compile(r"<a:(t|p)\b[^>]*?(/?)>(?:([^<]*)</a:t>)?"),
    ),
    "xlsx": (
        re.compile(r"^xl/(sharedStrings|worksheets/sheet\d+)\.xml$"),
        re.compile(r"<(t|v|row)\b[^>]*?(/?)>(?:([^<]*)</(?:t|v)>)?"),
    ),
}


class read_attachment(_Args):
    """Read the text of one attachment of an email returned by a tool in this run.
    Supports text, HTML, PDF, docx, xlsx and pptx."""

    email_id: str = Field(description="The email's id.")
    index: int = Field(0, ge=0, le=50, description="The attachment's position, from 0.")


class ExtractError(Exception):
    pass


def _kind(type: str, name: str) -> str:
    type = type.lower().split(";")[0].strip()
    suffix = name.lower().rsplit(".", 1)[-1] if "." in name else ""
    if type in TEXT_TYPES or suffix in ("txt", "csv", "md", "json"):
        return "text"
    if type == "text/html" or suffix in ("html", "htm"):
        return "html"
    if type == "application/pdf" or suffix == "pdf":
        return "pdf"
    if type in OOXML:
        return OOXML[type]
    if suffix in ("docx", "xlsx", "pptx"):
        return suffix
    raise ExtractError("unsupported attachment type")


def _ooxml(data: bytes, kind: str) -> str:
    members, pattern = _TAGS[kind]
    try:
        archive = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile as exc:
        raise ExtractError("not a valid document") from exc
    with archive:
        infos = sorted(
            (i for i in archive.infolist() if members.match(i.filename)),
            key=lambda i: [int(n) if n.isdigit() else n for n in re.split(r"(\d+)", i.filename)],
        )
        if sum(i.file_size for i in infos) > MAX_UNZIPPED_BYTES:
            raise ExtractError("document expands too far")
        parts: list[str] = []
        for info in infos:
            xml = archive.read(info).decode("utf-8", errors="replace")
            for tag, _, text in pattern.findall(xml):
                if tag in ("p", "row"):
                    parts.append("\n")
                elif text:
                    parts.append(html.unescape(text) + (" " if kind == "xlsx" else ""))
            parts.append("\n")
    lines = (" ".join(line.split()) for line in "".join(parts).splitlines())
    return "\n".join(line for line in lines if line)


def _pdf(data: bytes) -> str:
    from pypdf import PdfReader

    try:
        reader = PdfReader(io.BytesIO(data))
        pages = [p.extract_text() or "" for p in reader.pages[:MAX_PDF_PAGES]]
    except Exception as exc:  # pypdf raises many types on malformed input
        raise ExtractError("could not read the PDF") from exc
    return "\n\n".join(pages)


def extract_text(data: bytes, type: str, name: str) -> str:
    kind = _kind(type, name)
    if kind == "text":
        return data.decode("utf-8", errors="replace")
    if kind == "html":
        return html_to_text(data.decode("utf-8", errors="replace"))
    if kind == "pdf":
        return _pdf(data)
    return _ooxml(data, kind)


async def run_read_attachment(client: JmapClient, args: read_attachment) -> ToolResult:
    out = await client.call(
        [("Email/get", {"ids": [args.email_id], "properties": ["id", "attachments"]})]
    )
    found = out["c0"]["list"]
    attachments: list[dict[str, Any]] = found[0].get("attachments") or [] if found else []
    if args.index >= len(attachments):
        payload: dict[str, Any] = {"emailId": args.email_id, "error": "no such attachment"}
        return ToolResult(envelope(payload), frozenset())
    part = attachments[args.index]
    name, type = str(part.get("name") or ""), str(part.get("type") or "")
    payload = {"emailId": args.email_id, "index": args.index, "name": name, "type": type}
    try:
        if int(part.get("size") or 0) > MAX_ATTACHMENT_BYTES:
            raise ExtractError("attachment too large")
        _kind(type, name)
        data = await client.download(str(part["blobId"]), name, MAX_ATTACHMENT_BYTES)
        text = await asyncio.wait_for(
            asyncio.to_thread(extract_text, data, type, name), EXTRACT_SECONDS
        )
    except (ExtractError, TimeoutError) as exc:
        payload["error"] = str(exc) or "extraction timed out"
        return ToolResult(envelope(payload), frozenset({args.email_id}))
    raw = text.encode()
    payload["truncated"] = len(raw) > MAX_TEXT_BYTES
    payload["text"] = raw[:MAX_TEXT_BYTES].decode(errors="ignore")
    return ToolResult(envelope(payload), frozenset({args.email_id}))
