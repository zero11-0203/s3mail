package mailparse

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"strconv"
	"strings"
)

// Parser bounds (docs/specs/mail-flow.md#recipient-and-processing-policy).
const (
	MaxDepth         = 30
	MaxParts         = 1000
	MaxDecodedOutput = 64 << 20
)

// ErrLimit means the message exceeds a structure bound; the projection is
// partial and the raw message remains authoritative.
var ErrLimit = errors.New("MIME structure limit exceeded")

// Part is one entity of the body structure. Leaf parts have a PartID and
// their decoded content; multipart containers have SubParts instead.
type Part struct {
	PartID      string
	Headers     Headers
	Type        string // lowercase media type
	Charset     string // empty when not applicable
	Disposition string // lowercase, empty when absent
	Name        string
	CID         string
	Language    []string
	Location    string
	Size        int64 // decoded octets
	SubParts    []*Part

	content []byte // decoded content of leaf parts
	// unknownTransfer marks a Content-Transfer-Encoding that could not be
	// removed; content is then the encoded bytes.
	unknownTransfer bool
}

// Content returns the decoded (transfer-encoding removed) bytes of a leaf.
func (p *Part) Content() []byte { return p.content }

// IsMultipart reports whether p is a multipart container.
func (p *Part) IsMultipart() bool { return strings.HasPrefix(p.Type, "multipart/") }

// Message is a parsed raw message.
type Message struct {
	Headers Headers
	Body    *Part
	// Truncated reports that a structure bound was hit; parts past the
	// bound are missing from Body.
	Truncated bool
}

// Parse parses a complete raw message held in memory.
func Parse(raw []byte) *Message {
	p := &parser{}
	body := p.entity(raw, "text/plain", 0)
	return &Message{Headers: body.Headers, Body: body, Truncated: p.truncated}
}

type parser struct {
	leaves    int
	parts     int
	output    int64
	truncated bool
}

func (p *parser) entity(raw []byte, defaultType string, depth int) *Part {
	p.parts++
	h, body := splitHeader(raw)
	part := &Part{Headers: parseFields(h)}
	ct, params := contentType(part.Headers, defaultType)
	part.Type = ct
	if strings.HasPrefix(ct, "text/") {
		part.Charset = params["charset"]
		if part.Charset == "" {
			part.Charset = "us-ascii"
		}
	}
	if v, ok := part.Headers.Last("Content-Disposition"); ok {
		if d, dp, err := mime.ParseMediaType(AsText(v)); err == nil {
			part.Disposition = strings.ToLower(d)
			part.Name = decodeName(dp["filename"])
		} else {
			part.Disposition = strings.ToLower(strings.TrimSpace(strings.SplitN(AsText(v), ";", 2)[0]))
		}
	}
	if part.Name == "" {
		part.Name = decodeName(params["name"])
	}
	if v, ok := part.Headers.Last("Content-Id"); ok {
		part.CID = strings.Trim(strings.TrimSpace(unfold(v)), "<>")
	}
	if v, ok := part.Headers.Last("Content-Language"); ok {
		for _, l := range strings.Split(unfold(v), ",") {
			if l = strings.TrimSpace(l); l != "" {
				part.Language = append(part.Language, l)
			}
		}
	}
	if v, ok := part.Headers.Last("Content-Location"); ok {
		part.Location = AsText(v)
	}

	if strings.HasPrefix(ct, "multipart/") && params["boundary"] != "" {
		if depth >= MaxDepth {
			p.truncated = true
			return part
		}
		sub := "text/plain"
		if ct == "multipart/digest" {
			sub = "message/rfc822"
		}
		for _, b := range splitMultipart(body, params["boundary"]) {
			if p.parts >= MaxParts {
				p.truncated = true
				break
			}
			part.SubParts = append(part.SubParts, p.entity(b, sub, depth+1))
		}
		part.SubParts = nonNil(part.SubParts)
		return part
	}
	if strings.HasPrefix(ct, "multipart/") {
		// A multipart without a boundary cannot be split; treat it as an
		// opaque leaf so its bytes stay reachable.
		part.Type = "application/octet-stream"
	}

	p.leaves++
	part.PartID = strconv.Itoa(p.leaves)
	decoded, known := decodeTransfer(part.Headers, body)
	part.unknownTransfer = !known
	part.Size = int64(len(decoded))
	if p.output+part.Size > MaxDecodedOutput {
		p.truncated = true
		return part
	}
	p.output += part.Size
	part.content = decoded
	return part
}

func nonNil(ps []*Part) []*Part {
	if ps == nil {
		return []*Part{}
	}
	return ps
}

func contentType(h Headers, def string) (string, map[string]string) {
	v, ok := h.Last("Content-Type")
	if !ok {
		return def, map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(strings.TrimSpace(unfold(v)))
	if err != nil || !strings.Contains(mt, "/") {
		// RFC 2045 §5.2: an unparseable Content-Type means text/plain.
		return "text/plain", map[string]string{}
	}
	return strings.ToLower(mt), params
}

func decodeName(n string) string {
	if n == "" {
		return ""
	}
	return AsText(n)
}

// decodeTransfer removes the transfer encoding and reports whether it was
// one this parser knows; an unknown one leaves the body as it is.
func decodeTransfer(h Headers, body []byte) ([]byte, bool) {
	cte, _ := h.Last("Content-Transfer-Encoding")
	switch strings.ToLower(strings.TrimSpace(unfold(cte))) {
	case "", "7bit", "8bit", "binary":
		return body, true
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		if n, err := base64.StdEncoding.Decode(out, clean); err == nil {
			return out[:n], true
		}
		// Missing padding or trailing garbage are common in the wild: keep
		// what decodes before the first invalid byte.
		raw := bytes.TrimRight(clean, "=")
		out = make([]byte, base64.RawStdEncoding.DecodedLen(len(raw)))
		n, _ := base64.RawStdEncoding.Decode(out, raw)
		return out[:n], true
	case "quoted-printable":
		out, _ := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		return out, true
	}
	return body, false
}

// splitMultipart returns the body parts between boundary delimiters,
// excluding the line break before each delimiter (RFC 2046 §5.1.1).
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := []byte("--" + boundary)
	var parts [][]byte
	start := -1
	pos := 0
	for pos <= len(body) {
		end := bytes.IndexByte(body[pos:], '\n')
		var line []byte
		next := len(body) + 1
		if end < 0 {
			line = body[pos:]
		} else {
			line = body[pos : pos+end+1]
			next = pos + end + 1
		}
		trimmed := bytes.TrimRight(line, " \t\r\n")
		if bytes.HasPrefix(trimmed, delim) {
			rest := trimmed[len(delim):]
			closing := bytes.Equal(rest, []byte("--"))
			if len(rest) == 0 || closing {
				if start >= 0 {
					parts = append(parts, trimBreak(body[start:pos]))
				}
				if closing {
					return parts
				}
				start = next
				if start > len(body) {
					start = len(body)
				}
			}
		}
		pos = next
	}
	if start >= 0 && start <= len(body) {
		// Missing closing delimiter: keep the last part.
		parts = append(parts, body[start:])
	}
	return parts
}

func trimBreak(b []byte) []byte {
	if bytes.HasSuffix(b, []byte("\r\n")) {
		return b[:len(b)-2]
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

// Leaves returns the leaf parts in partId order.
func (m *Message) Leaves() []*Part {
	var out []*Part
	var walk func(*Part)
	walk = func(p *Part) {
		if p.IsMultipart() {
			for _, s := range p.SubParts {
				walk(s)
			}
			return
		}
		out = append(out, p)
	}
	walk(m.Body)
	return out
}

// Find returns the leaf with partID, or nil.
func (m *Message) Find(partID string) *Part {
	for _, p := range m.Leaves() {
		if p.PartID == partID {
			return p
		}
	}
	return nil
}
