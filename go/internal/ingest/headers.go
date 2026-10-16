package ingest

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"
)

// Header-projection bounds (docs/specs/mail-flow.md#recipient-and-processing-policy).
// Anything beyond them stays in the raw MIME; the projection is marked
// incomplete instead of pretending to be whole.
const (
	MaxHeaderBytes   = 256 << 10
	MaxAddresses     = 100
	maxSubjectBytes  = 2048
	maxIDHeaderBytes = 8192
)

// ErrHeaderLimit means the header block did not end within MaxHeaderBytes.
var ErrHeaderLimit = errors.New("header block exceeds the limit")

// Headers is the stored metadata projection of the message header. Address
// lists are as written by the sender; routing never uses them.
type Headers struct {
	Subject    string
	From       []string
	To         []string
	Cc         []string
	ReplyTo    []string
	Date       time.Time // zero when missing or unparseable
	MessageID  string
	InReplyTo  string
	References string
	// Incomplete lists the reasons the projection is partial, such as
	// "charset", "address-count" or "subject-length".
	Incomplete []string
}

// ParseHeaders reads the header block from the start of a message. prefix
// holds at most MaxHeaderBytes+1 bytes; truncated reports whether the
// object is longer than prefix.
func ParseHeaders(prefix []byte, truncated bool) (Headers, error) {
	end := headerEnd(prefix)
	switch {
	case end < 0 && (truncated || len(prefix) > MaxHeaderBytes):
		return Headers{}, ErrHeaderLimit
	case end < 0:
		// A message with no body: the whole object is the header block.
		prefix = append(bytes.Clone(prefix), "\r\n\r\n"...)
		end = len(prefix)
	}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(prefix[:end])))
	raw, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return Headers{Incomplete: []string{"header-syntax"}}, nil
	}
	h := Headers{}
	hdr := mail.Header(raw)
	h.Subject = h.decode(hdr.Get("Subject"))
	h.Subject = h.capText(h.Subject, maxSubjectBytes, "subject-length")
	h.From = h.addresses(hdr, "From")
	h.To = h.addresses(hdr, "To")
	h.Cc = h.addresses(hdr, "Cc")
	h.ReplyTo = h.addresses(hdr, "Reply-To")
	if d, err := hdr.Date(); err == nil {
		h.Date = d.UTC()
	} else if hdr.Get("Date") != "" {
		h.mark("date")
	}
	h.MessageID = h.capText(strings.TrimSpace(hdr.Get("Message-Id")), maxIDHeaderBytes, "message-id-length")
	h.InReplyTo = h.capText(strings.TrimSpace(hdr.Get("In-Reply-To")), maxIDHeaderBytes, "in-reply-to-length")
	h.References = h.capText(strings.Join(strings.Fields(hdr.Get("References")), " "), maxIDHeaderBytes, "references-length")
	return h, nil
}

// headerEnd returns the length of the header block including its blank
// line, or -1 if the block does not end in b.
func headerEnd(b []byte) int {
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	lf := bytes.Index(b, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf + 4
	case lf >= 0:
		return lf + 2
	}
	return -1
}

// decoder handles RFC 2047 words in UTF-8, US-ASCII and ISO-8859-1. Legacy
// charsets (GBK, Shift_JIS, ...) need the full MIME parser; until then the
// raw text is kept and the projection marked incomplete.
var decoder = &mime.WordDecoder{}

func (h *Headers) decode(s string) string {
	d, err := decoder.DecodeHeader(s)
	if err != nil {
		h.mark("charset")
		d = s
	}
	if !utf8.ValidString(d) {
		h.mark("charset")
		d = strings.ToValidUTF8(d, "�")
	}
	return d
}

func (h *Headers) addresses(hdr mail.Header, name string) []string {
	v := hdr.Get(name)
	if v == "" {
		return nil
	}
	p := mail.AddressParser{WordDecoder: decoder}
	list, err := p.ParseList(v)
	if err != nil {
		h.mark("address-syntax")
		return nil
	}
	var out []string
	for _, a := range list {
		if len(out) == MaxAddresses {
			h.mark("address-count")
			break
		}
		out = append(out, a.String())
	}
	return out
}

func (h *Headers) capText(s string, limit int, reason string) string {
	if len(s) <= limit {
		return s
	}
	h.mark(reason)
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func (h *Headers) mark(reason string) {
	for _, r := range h.Incomplete {
		if r == reason {
			return
		}
	}
	h.Incomplete = append(h.Incomplete, reason)
}
