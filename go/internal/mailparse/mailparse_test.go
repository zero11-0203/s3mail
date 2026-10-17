package mailparse

import (
	"strings"
	"testing"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func types(ps []*Part) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.PartID+":"+p.Type)
	}
	return strings.Join(out, " ")
}

func TestPlainMessage(t *testing.T) {
	m := Parse(crlf("From: a@example.net\nSubject: hi\n\nhello\n"))
	if m.Body.PartID != "1" || m.Body.Type != "text/plain" || m.Body.Charset != "us-ascii" {
		t.Fatalf("body = %+v", m.Body)
	}
	if got := string(m.Body.Content()); got != "hello\r\n" || m.Body.Size != 7 {
		t.Errorf("content %q size %d", got, m.Body.Size)
	}
	b := m.BodyLists()
	if types(b.Text) != "1:text/plain" || types(b.HTML) != "1:text/plain" || len(b.Attachments) != 0 {
		t.Errorf("lists text=%q html=%q att=%d", types(b.Text), types(b.HTML), len(b.Attachments))
	}
	if m.Preview(b) != "hello" {
		t.Errorf("preview %q", m.Preview(b))
	}
}

const mixed = `From: a@example.net
Content-Type: multipart/mixed; boundary="outer"

preamble
--outer
Content-Type: multipart/alternative; boundary=alt

--alt
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

caf=C3=A9
--alt
Content-Type: multipart/related; boundary=rel

--rel
Content-Type: text/html; charset=utf-8

<p>caf&eacute; <img src="cid:logo@x"></p>
--rel
Content-Type: image/png
Content-Id: <logo@x>
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--rel--
--alt--
--outer
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf
Content-Transfer-Encoding: base64

JVBERi0x
LjQK
--outer--
epilogue
`

func TestMixedAlternativeRelated(t *testing.T) {
	m := Parse(crlf(mixed))
	if m.Truncated {
		t.Fatal("unexpected truncation")
	}
	if got := types(m.Leaves()); got != "1:text/plain 2:text/html 3:image/png 4:application/pdf" {
		t.Fatalf("leaves %q", got)
	}
	b := m.BodyLists()
	if got := types(b.Text); got != "1:text/plain" {
		t.Errorf("textBody %q", got)
	}
	if got := types(b.HTML); got != "2:text/html" {
		t.Errorf("htmlBody %q", got)
	}
	if got := types(b.Attachments); got != "3:image/png 4:application/pdf" {
		t.Errorf("attachments %q", got)
	}
	if v := m.Find("1").Value(0); v.Value != "café" || v.IsEncodingProblem {
		t.Errorf("qp text %+v", v)
	}
	img := m.Find("3")
	if img.CID != "logo@x" || img.Size != 8 {
		t.Errorf("image cid %q size %d", img.CID, img.Size)
	}
	pdf := m.Find("4")
	if pdf.Name != "résumé.pdf" || pdf.Disposition != "attachment" || string(pdf.Content()) != "%PDF-1.4\n" {
		t.Errorf("pdf name %q disposition %q content %q", pdf.Name, pdf.Disposition, pdf.Content())
	}
	if m.Body.PartID != "" || len(m.Body.SubParts) != 2 {
		t.Errorf("multipart root has partId %q and %d subparts", m.Body.PartID, len(m.Body.SubParts))
	}
}

func TestHTMLOnlyAlternativeFillsTextBody(t *testing.T) {
	msg := "Content-Type: multipart/alternative; boundary=b\n\n--b\nContent-Type: text/html\n\n<b>x</b>\n--b--\n"
	b := Parse(crlf(msg)).BodyLists()
	if types(b.Text) != "1:text/html" || types(b.HTML) != "1:text/html" {
		t.Errorf("text %q html %q", types(b.Text), types(b.HTML))
	}
}

// The RFC 8621 §4.1.4 pseudo-code stops filling a list inside an
// alternative and then hands the null list to a nested alternative.
const nestedAlternative = "Content-Type: multipart/alternative; boundary=a\n\n" +
	"--a\nContent-Type: multipart/mixed; boundary=m\n\n" +
	"--m\nContent-Type: text/plain\n\nhi\n" +
	"--m\nContent-Type: multipart/alternative; boundary=n\n\n" +
	"--n\nContent-Type: text/plain\n\nx\n--n\nContent-Type: text/html\n\n<b>x</b>\n--n--\n" +
	"--m--\n--a--\n"

func TestNestedAlternativeAfterInlineText(t *testing.T) {
	b := Parse(crlf(nestedAlternative)).BodyLists()
	if types(b.Text) != "1:text/plain 2:text/plain" || types(b.HTML) != "1:text/plain 2:text/plain" || len(b.Attachments) != 0 {
		t.Errorf("text %q html %q att %q", types(b.Text), types(b.HTML), types(b.Attachments))
	}
}

func TestNamedTextAfterFirstIsAttachment(t *testing.T) {
	msg := "Content-Type: multipart/mixed; boundary=b\n\n--b\n\nbody\n--b\nContent-Type: text/plain; name=notes.txt\n\nnotes\n--b--\n"
	b := Parse(crlf(msg)).BodyLists()
	if types(b.Text) != "1:text/plain" || types(b.Attachments) != "2:text/plain" {
		t.Errorf("text %q att %q", types(b.Text), types(b.Attachments))
	}
}

func TestLegacyCharsets(t *testing.T) {
	// Subject "日本" in ISO-2022-JP, body "ＡＡ" in Shift_JIS.
	msg := "Subject: =?ISO-2022-JP?B?GyRCRnxLXBsoQg==?=\nContent-Type: text/plain; charset=Shift_JIS\n\n\x82\x60\x82\x60"
	m := Parse([]byte(msg))
	subj, _ := m.Headers.Last("Subject")
	if AsText(subj) != "日本" {
		t.Errorf("subject %q", AsText(subj))
	}
	if v := m.Body.Value(0); v.Value != "ＡＡ" || v.IsEncodingProblem {
		t.Errorf("body %+v", v)
	}
	if v := (&Part{Charset: "x-unknown", content: []byte("abc")}).Value(0); !v.IsEncodingProblem || v.Value != "abc" {
		t.Errorf("unknown charset %+v", v)
	}
}

func TestTextFormNormalizesAndDropsControls(t *testing.T) {
	if got := AsText("café"); got != "café" {
		t.Errorf("not NFC: %q", got)
	}
	if got := AsText("=?utf-8?Q?hi=00=01?= there\tnow"); got != "hi there\tnow" {
		t.Errorf("controls kept: %q", got)
	}
	if a := AsAddresses("=?utf-8?Q?J=C3=B6rg=07?= <j@example.com>"); len(a) != 1 || *a[0].Name != "Jörg" {
		t.Errorf("address name %+v", a)
	}
}

func TestUnknownTransferEncodingIsAnEncodingProblem(t *testing.T) {
	m := Parse(crlf("Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: x-unknown\n\nabc\n"))
	if v := m.Body.Value(0); !v.IsEncodingProblem || v.Value != "abc\n" {
		t.Errorf("unknown transfer encoding %+v", v)
	}
	m = Parse(crlf("Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: 8bit\n\nabc\n"))
	if v := m.Body.Value(0); v.IsEncodingProblem {
		t.Errorf("8bit flagged %+v", v)
	}
}

func TestValueTruncatesOnRuneBoundary(t *testing.T) {
	v := (&Part{Charset: "utf-8", content: []byte("ééé")}).Value(3)
	if v.Value != "é" || !v.IsTruncated {
		t.Errorf("%+v", v)
	}
}

func TestHeaderForms(t *testing.T) {
	m := Parse(crlf("To: Team: =?utf-8?q?J=C3=B6rg?= <j@example.com>, b@example.com;, c@example.com\n" +
		"Subject:  folded\n  line \n" +
		"Message-ID: <id1@example.net>\n" +
		"References: <r1@x>\n <r2@x>\n" +
		"Date: Sat, 17 Oct 2026 15:40:12 +0900\n" +
		"List-Unsubscribe: <mailto:u@example.net>, <https://example.net/u>\n\nx"))
	to, _ := m.Headers.Last("To")
	groups := AsGroupedAddresses(to)
	if len(groups) != 2 || groups[0].Name == nil || *groups[0].Name != "Team" || len(groups[0].Addresses) != 2 || groups[1].Name != nil {
		t.Fatalf("groups %+v", groups)
	}
	if a := groups[0].Addresses[0]; a.Name == nil || *a.Name != "Jörg" || a.Email != "j@example.com" {
		t.Errorf("decoded address %+v", a)
	}
	if n := len(AsAddresses(to)); n != 3 {
		t.Errorf("flattened %d", n)
	}
	subj, _ := m.Headers.Last("Subject")
	if subj != "  folded\r\n  line " || AsText(subj) != "folded  line" {
		t.Errorf("raw %q text %q", subj, AsText(subj))
	}
	refs, _ := m.Headers.Last("References")
	if ids := AsMessageIds(refs); len(ids) != 2 || ids[1] != "r2@x" {
		t.Errorf("message ids %v", ids)
	}
	date, _ := m.Headers.Last("Date")
	if d := AsDate(date); d == nil || d.UTC().Hour() != 6 {
		t.Errorf("date %v", d)
	}
	lu, _ := m.Headers.Last("List-Unsubscribe")
	if u := AsURLs(lu); len(u) != 2 || u[0] != "mailto:u@example.net" {
		t.Errorf("urls %v", u)
	}
	if AsMessageIds(" no ids ") != nil || AsDate("garbage") != nil {
		t.Error("unparseable forms must be null")
	}
}

func TestStructureBounds(t *testing.T) {
	var b strings.Builder
	for i := range MaxDepth + 5 {
		b.WriteString("Content-Type: multipart/mixed; boundary=b" + strings.Repeat("x", i) + "\n\n--b" + strings.Repeat("x", i) + "\n")
	}
	b.WriteString("\nleaf\n")
	if m := Parse(crlf(b.String())); !m.Truncated {
		t.Error("nesting beyond MaxDepth must be reported")
	}

	var many strings.Builder
	many.WriteString("Content-Type: multipart/mixed; boundary=b\n\n")
	for range MaxParts + 10 {
		many.WriteString("--b\n\nx\n")
	}
	many.WriteString("--b--\n")
	m := Parse(crlf(many.String()))
	if !m.Truncated || len(m.Leaves()) >= MaxParts {
		t.Errorf("parts bound: truncated %v leaves %d", m.Truncated, len(m.Leaves()))
	}
}

func TestMissingClosingBoundaryKeepsLastPart(t *testing.T) {
	m := Parse(crlf("Content-Type: multipart/mixed; boundary=b\n\n--b\n\none\n--b\n\ntwo\n"))
	if got := types(m.Leaves()); got != "1:text/plain 2:text/plain" {
		t.Errorf("leaves %q", got)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(crlf(mixed))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=b\n\n--b\n--b--"))
	f.Add(crlf(nestedAlternative))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m := Parse(raw)
		for _, p := range m.Leaves() {
			_ = p.Value(1024)
		}
		_ = m.Preview(m.BodyLists())
	})
}
