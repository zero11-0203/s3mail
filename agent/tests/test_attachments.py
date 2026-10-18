from __future__ import annotations

import io
import json
import multiprocessing
import time
import zipfile
from multiprocessing.connection import Connection

import pytest

from s3mail_agent.attachments import (
    MAX_TEXT_BYTES,
    MAX_UNZIPPED_BYTES,
    ExtractError,
    extract_isolated,
    extract_text,
    read_attachment,
    run_read_attachment,
)
from s3mail_agent.jmap import JmapClient
from s3mail_agent.tools import DATA_OPEN

from .conftest import SESSION_URL
from .mailserver import MailServer, message

DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
XLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"


def ooxml(files: dict[str, str]) -> bytes:
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        for name, text in files.items():
            z.writestr(name, text)
    return buf.getvalue()


def pdf(text: str) -> bytes:
    """A minimal one-page PDF with a text stream."""
    stream = f"BT /F1 12 Tf 72 720 Td ({text}) Tj ET".encode()
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R "
        b"/Resources << /Font << /F1 5 0 R >> >> >>",
        b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = io.BytesIO()
    out.write(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objects, 1):
        offsets.append(out.tell())
        out.write(b"%d 0 obj\n" % i + body + b"\nendobj\n")
    xref = out.tell()
    out.write(b"xref\n0 %d\n0000000000 65535 f \n" % (len(objects) + 1))
    for o in offsets:
        out.write(b"%010d 00000 n \n" % o)
    out.write(
        b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objects) + 1, xref)
    )
    return out.getvalue()


def test_docx_text_with_entities_but_no_expansion() -> None:
    doc = ooxml(
        {
            "word/document.xml": '<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "boom">]>'
            "<w:document><w:body><w:p><w:r><w:t>Invoice &amp; total</w:t></w:r></w:p>"
            '<w:p><w:r><w:t xml:space="preserve">42.00 EUR &a;</w:t></w:r></w:p>'
            "</w:body></w:document>"
        }
    )
    assert extract_text(doc, DOCX, "inv.docx") == "Invoice & total\n42.00 EUR &a;"


def test_xlsx_cells_resolve_to_their_values_by_row() -> None:
    book = ooxml(
        {
            "xl/sharedStrings.xml": "<sst><si><t>Alpha</t></si><si><t>Beta</t></si>"
            "<si><r><t>Gam</t></r><r><t>ma</t></r><rPh><t>reading</t></rPh></si></sst>",
            "xl/worksheets/sheet1.xml": "<worksheet><sheetData>"
            '<row r="1"><c r="A1" t="s"><v>1</v></c><c r="B1"><v>42</v></c></row>'
            '<row r="2"><c r="A2" t="inlineStr"><is><t>Fish &amp; chips</t></is></c>'
            '<c r="B2" t="s"><v>2</v></c><c r="C2" t="s"><v>9</v></c><c r="D2"/></row>'
            "</sheetData></worksheet>",
        }
    )
    # Unused strings stay out; shared-string indices never appear as numbers.
    assert extract_text(book, XLSX, "b.xlsx") == "Beta | 42\nFish & chips | Gamma"


def test_extraction_stops_at_the_text_cap() -> None:
    rows = "".join(f"<row><c><v>{i:08d}</v></c></row>" for i in range(MAX_TEXT_BYTES))
    book = ooxml(
        {"xl/worksheets/sheet1.xml": f"<worksheet><sheetData>{rows}</sheetData></worksheet>"}
    )
    text = extract_text(book, XLSX, "b.xlsx")
    assert MAX_TEXT_BYTES < len(text) < 2 * MAX_TEXT_BYTES


def _hang(conn: Connection, data: bytes, type: str, name: str) -> None:
    time.sleep(60)


async def test_extraction_is_killed_at_the_deadline() -> None:
    started = time.monotonic()
    with pytest.raises(TimeoutError):
        await extract_isolated(b"x", "text/plain", "a.txt", seconds=0.5, target=_hang)
    assert time.monotonic() - started < 30
    assert multiprocessing.active_children() == []


async def test_isolated_extraction_returns_text_and_errors() -> None:
    assert await extract_isolated("café".encode(), "text/plain", "a.txt") == "café"
    with pytest.raises(ExtractError, match="unsupported"):
        await extract_isolated(b"MZ", "application/x-msdownload", "x.exe")


def test_pdf_text() -> None:
    assert "Boarding gate B12" in extract_text(pdf("Boarding gate B12"), "application/pdf", "p.pdf")


def test_html_and_plain() -> None:
    html = b"<p>Hello <b>there</b></p><script>x()</script>"
    assert extract_text(html, "text/html", "a.html") == "Hello there"
    assert extract_text("café".encode(), "text/plain", "a.txt") == "café"


def test_zip_bombs_and_unknown_types_are_refused() -> None:
    big = io.BytesIO()
    with zipfile.ZipFile(big, "w", zipfile.ZIP_DEFLATED) as z:
        z.writestr("word/document.xml", "a" * (MAX_UNZIPPED_BYTES + 1))
    with pytest.raises(ExtractError, match="expands"):
        extract_text(big.getvalue(), DOCX, "x.docx")
    with pytest.raises(ExtractError, match="unsupported"):
        extract_text(b"MZ", "application/x-msdownload", "x.exe")
    with pytest.raises(ExtractError):
        extract_text(b"not a zip", DOCX, "x.docx")
    with pytest.raises(ExtractError):
        extract_text(b"%PDF-1.4 garbage", "application/pdf", "x.pdf")


async def test_tool_reads_by_index_within_caps() -> None:
    s = MailServer()
    parts = [
        {
            "partId": "2",
            "blobId": "Bpdf",
            "name": "ticket.pdf",
            "type": "application/pdf",
            "size": 900,
        },
        {
            "partId": "3",
            "blobId": "Bbig",
            "name": "huge.pdf",
            "type": "application/pdf",
            "size": 11 << 20,
        },
    ]
    s.deliver(
        message(
            "M1",
            "T1",
            "air@example.com",
            "Ticket",
            "2026-10-01T00:00:00Z",
            "See attached",
            attachments=parts,
        )
    )
    s.blobs["Bpdf"] = pdf("Seat 14C")
    client = JmapClient(SESSION_URL, "t", transport=s.transport(), backoff=0)

    result = await run_read_attachment(client, read_attachment(email_id="M1", index=0))
    assert result.content.startswith(DATA_OPEN) and result.email_ids == {"M1"}
    payload = json.loads(result.content.split("\n", 2)[1])
    assert "Seat 14C" in payload["text"] and payload["truncated"] is False

    big = json.loads(
        (await run_read_attachment(client, read_attachment(email_id="M1", index=1))).content.split(
            "\n", 2
        )[1]
    )
    assert big["error"] == "attachment too large"
    missing = await run_read_attachment(client, read_attachment(email_id="M1", index=5))
    assert "no such attachment" in missing.content and missing.email_ids == frozenset()
    assert s.writes == []
