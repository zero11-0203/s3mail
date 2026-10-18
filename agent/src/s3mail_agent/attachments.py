"""Attachment text (A4, docs/specs/agent.md#features).

read_attachment downloads one attachment of an email found by the model,
within size caps, and extracts its text: plain text and HTML, PDF (pypdf,
first pages only) and Office Open XML documents (docx, xlsx, pptx). Office
XML is read with patterns rather than an XML parser, so entity expansion
cannot occur, and archive members are size-checked before they are read.
Spreadsheet cells are resolved to their values, row by row. Extraction stops
at the text cap and runs in a child process with a memory limit, killed at
the deadline. The text reaches the model as untrusted data like any mail body.
"""

from __future__ import annotations

import asyncio
import html
import io
import multiprocessing
import re
import zipfile
from collections.abc import Callable
from multiprocessing.connection import Connection
from typing import Any

from pydantic import Field

from .jmap import JmapClient
from .tools import ToolResult, _Args, envelope, html_to_text

MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024
MAX_TEXT_BYTES = 32 * 1024
MAX_PDF_PAGES = 30
MAX_UNZIPPED_BYTES = 40 * 1024 * 1024
EXTRACT_SECONDS = 20
# The extraction process's address-space limit, where the OS enforces one.
EXTRACT_MEMORY_BYTES = 1024 * 1024 * 1024

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
}
_SHEET = re.compile(r"^xl/worksheets/sheet\d+\.xml$")
_SHARED = "xl/sharedStrings.xml"
_SI = re.compile(r"<si\b[^>]*>(.*?)</si>", re.S)
_PHONETIC = re.compile(r"<rPh\b.*?</rPh>", re.S)
_T = re.compile(r"<t\b[^>]*>([^<]*)</t>")
_ROW = re.compile(r"<row\b[^>]*>(.*?)</row>", re.S)
_CELL = re.compile(r"<c\b([^>]*?)(?:/>|>(.*?)</c>)", re.S)
_TYPE = re.compile(r"\bt=\"([^\"]*)\"")
_V = re.compile(r"<v\b[^>]*>([^<]*)</v>")


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


def _members(archive: zipfile.ZipFile, pattern: re.Pattern[str]) -> list[zipfile.ZipInfo]:
    return sorted(
        (i for i in archive.infolist() if pattern.match(i.filename)),
        key=lambda i: [int(n) if n.isdigit() else n for n in re.split(r"(\d+)", i.filename)],
    )


def _ooxml(data: bytes, kind: str) -> str:
    try:
        archive = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile as exc:
        raise ExtractError("not a valid document") from exc
    with archive:
        if kind == "xlsx":
            return _xlsx(archive)
        members, pattern = _TAGS[kind]
        infos = _members(archive, members)
        if sum(i.file_size for i in infos) > MAX_UNZIPPED_BYTES:
            raise ExtractError("document expands too far")
        parts: list[str] = []
        size = 0
        for info in infos:
            xml = archive.read(info).decode("utf-8", errors="replace")
            for tag, _, text in pattern.findall(xml):
                if tag == "p":
                    parts.append("\n")
                elif text:
                    parts.append(html.unescape(text))
                    size += len(text)
            parts.append("\n")
            if size > MAX_TEXT_BYTES:
                break
    return _lines("".join(parts))


def _lines(text: str) -> str:
    lines = (" ".join(line.split()) for line in text.splitlines())
    return "\n".join(line for line in lines if line)


def _xlsx(archive: zipfile.ZipFile) -> str:
    """One line per row, its cells' values joined by " | "; shared strings
    are looked up by index, inline strings read in place."""
    sheets = _members(archive, _SHEET)
    shared = [i for i in archive.infolist() if i.filename == _SHARED]
    if sum(i.file_size for i in [*sheets, *shared]) > MAX_UNZIPPED_BYTES:
        raise ExtractError("document expands too far")
    strings: list[str] = []
    if shared:
        xml = archive.read(shared[0]).decode("utf-8", errors="replace")
        strings = ["".join(_T.findall(_PHONETIC.sub("", si))) for si in _SI.findall(xml)]
    rows: list[str] = []
    size = 0
    for info in sheets:
        xml = archive.read(info).decode("utf-8", errors="replace")
        for row in _ROW.findall(xml):
            cells = [v for attrs, body in _CELL.findall(row) if (v := _cell(attrs, body, strings))]
            if cells:
                line = " | ".join(html.unescape(c) for c in cells)
                rows.append(line)
                size += len(line)
            if size > MAX_TEXT_BYTES:
                return _lines("\n".join(rows))
        rows.append("")
    return _lines("\n".join(rows))


def _cell(attrs: str, body: str, strings: list[str]) -> str:
    kind = m.group(1) if (m := _TYPE.search(attrs)) else "n"
    if kind == "inlineStr":
        return "".join(_T.findall(_PHONETIC.sub("", body)))
    value = m.group(1) if (m := _V.search(body)) else ""
    if kind == "s":
        try:
            return strings[int(value)]
        except (ValueError, IndexError):
            return ""
    return value


def _pdf(data: bytes) -> str:
    from pypdf import PdfReader

    try:
        reader = PdfReader(io.BytesIO(data))
        pages: list[str] = []
        size = 0
        for page in reader.pages[:MAX_PDF_PAGES]:
            pages.append(page.extract_text() or "")
            size += len(pages[-1])
            if size > MAX_TEXT_BYTES:
                break
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


def _extract_child(conn: Connection, data: bytes, type: str, name: str) -> None:
    try:
        import resource

        resource.setrlimit(resource.RLIMIT_AS, (EXTRACT_MEMORY_BYTES, EXTRACT_MEMORY_BYTES))
    except (ImportError, ValueError, OSError):
        pass  # not enforceable here (macOS); the deadline still applies
    try:
        conn.send(("ok", extract_text(data, type, name)))
    except ExtractError as exc:
        conn.send(("error", str(exc)))
    except MemoryError:
        conn.send(("error", "document too large to extract"))
    finally:
        conn.close()


async def extract_isolated(
    data: bytes,
    type: str,
    name: str,
    *,
    seconds: float = EXTRACT_SECONDS,
    target: Callable[[Connection, bytes, str, str], None] = _extract_child,
) -> str:
    """extract_text in a child process that is killed at the deadline, so
    an expensive document cannot keep running after the tool gives up."""
    ctx = multiprocessing.get_context("spawn")
    receiver, sender = ctx.Pipe(duplex=False)
    process = ctx.Process(target=target, args=(sender, data, type, name), daemon=True)
    process.start()
    sender.close()
    try:
        if not await asyncio.to_thread(receiver.poll, seconds):
            raise TimeoutError
        status, value = receiver.recv()
    except EOFError as exc:  # the child died, such as at its memory limit
        raise ExtractError("extraction failed") from exc
    finally:
        if process.is_alive():
            process.kill()
        await asyncio.to_thread(process.join)
        receiver.close()
    if status != "ok":
        raise ExtractError(value)
    return str(value)


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
        data = await client.download(str(part["blobId"]), MAX_ATTACHMENT_BYTES)
        text = await extract_isolated(data, type, name)
    except (ExtractError, TimeoutError) as exc:
        payload["error"] = str(exc) or "extraction timed out"
        return ToolResult(envelope(payload), frozenset({args.email_id}))
    raw = text.encode()
    payload["truncated"] = len(raw) > MAX_TEXT_BYTES
    payload["text"] = raw[:MAX_TEXT_BYTES].decode(errors="ignore")
    return ToolResult(envelope(payload), frozenset({args.email_id}))
