package mailparse

import (
	"bytes"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Field is one header field in message order. Value is the raw text after
// the colon, folding included, without the final line break (RFC 8621
// §4.1.2.1 Raw form).
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// splitHeader returns the header block and the body. A missing blank line
// means the whole entity is header.
func splitHeader(b []byte) (header, body []byte) {
	if bytes.HasPrefix(b, []byte("\r\n")) {
		return nil, b[2:]
	}
	if bytes.HasPrefix(b, []byte("\n")) {
		return nil, b[1:]
	}
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	lf := bytes.Index(b, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return b[:crlf+2], b[crlf+4:]
	case lf >= 0:
		return b[:lf+1], b[lf+2:]
	}
	return b, nil
}

// parseFields splits a header block into fields. Lines without a colon
// are kept as continuation of the previous field rather than dropped.
func parseFields(h []byte) []Field {
	var fields []Field
	for len(h) > 0 {
		end := bytes.IndexByte(h, '\n')
		var line []byte
		if end < 0 {
			line, h = h, nil
		} else {
			line, h = h[:end+1], h[end+1:]
		}
		trimmed := strings.TrimRight(string(line), "\r\n")
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1].Value += "\r\n" + trimmed
			continue
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(name) == "" {
			if len(fields) > 0 {
				fields[len(fields)-1].Value += "\r\n " + trimmed
			}
			continue
		}
		fields = append(fields, Field{Name: strings.TrimRight(name, " \t"), Value: value})
	}
	return fields
}

// Headers is a field list with case-insensitive lookup.
type Headers []Field

// All returns the raw values of every field named name, in order.
func (h Headers) All(name string) []string {
	var out []string
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value)
		}
	}
	return out
}

// Last returns the raw value of the last field named name.
func (h Headers) Last(name string) (string, bool) {
	all := h.All(name)
	if len(all) == 0 {
		return "", false
	}
	return all[len(all)-1], true
}

func unfold(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "")
	return strings.ReplaceAll(v, "\n", "")
}

// AsText is the Text form: unfolded, RFC 2047 words decoded, trimmed.
func AsText(raw string) string {
	v := strings.TrimSpace(unfold(raw))
	if d, err := WordDecoder.DecodeHeader(v); err == nil {
		v = d
	}
	return cleanText(v)
}

// cleanText makes decoded header text valid UTF-8 without control
// characters (encoded words can carry NUL and C0/C1 controls; folding
// leaves tabs, which stay) and in NFC (RFC 8621 §4.1.2.2).
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	return norm.NFC.String(s)
}

// Address is a JMAP EmailAddress.
type Address struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// AddressGroup is a JMAP EmailAddressGroup.
type AddressGroup struct {
	Name      *string   `json:"name"`
	Addresses []Address `json:"addresses"`
}

// AsGroupedAddresses parses an address-list header, keeping RFC 5322
// groups ("name: a, b;"). Unparseable members are skipped rather than
// failing the field.
func AsGroupedAddresses(raw string) []AddressGroup {
	v := strings.TrimSpace(unfold(raw))
	var groups []AddressGroup
	for _, chunk := range splitTopLevel(v, ';') {
		items := splitTopLevel(chunk, ',')
		at := -1
		var name, first string
		for i, it := range items {
			if p := splitTopLevel(it, ':'); len(p) > 1 && !strings.ContainsAny(p[0], "<>@\"") {
				at, name, first = i, p[0], strings.Join(p[1:], ":")
				break
			}
		}
		if at < 0 {
			groups = append(groups, AddressGroup{Addresses: parseAddresses(chunk)})
			continue
		}
		if at > 0 {
			groups = append(groups, AddressGroup{Addresses: parseAddresses(strings.Join(items[:at], ","))})
		}
		n := AsText(name)
		members := strings.Join(append([]string{first}, items[at+1:]...), ",")
		groups = append(groups, AddressGroup{Name: &n, Addresses: parseAddresses(members)})
	}
	return mergeUngrouped(groups)
}

// mergeUngrouped joins adjacent null-named groups, as RFC 8621 requires.
func mergeUngrouped(gs []AddressGroup) []AddressGroup {
	out := []AddressGroup{}
	for _, g := range gs {
		if g.Name == nil && len(g.Addresses) == 0 {
			continue
		}
		if g.Name == nil && len(out) > 0 && out[len(out)-1].Name == nil {
			out[len(out)-1].Addresses = append(out[len(out)-1].Addresses, g.Addresses...)
			continue
		}
		out = append(out, g)
	}
	return out
}

// AsAddresses is the Addresses form: every mailbox, groups flattened.
func AsAddresses(raw string) []Address {
	out := []Address{}
	for _, g := range AsGroupedAddresses(raw) {
		out = append(out, g.Addresses...)
	}
	return out
}

func parseAddresses(v string) []Address {
	out := []Address{}
	p := mail.AddressParser{WordDecoder: WordDecoder}
	if list, err := p.ParseList(v); err == nil {
		for _, a := range list {
			out = append(out, toAddress(a))
		}
		return out
	}
	// Lenient fallback: parse each comma-separated member on its own.
	for _, part := range splitTopLevel(v, ',') {
		if strings.TrimSpace(part) == "" {
			continue
		}
		if a, err := p.Parse(part); err == nil {
			out = append(out, toAddress(a))
		}
	}
	return out
}

func toAddress(a *mail.Address) Address {
	addr := Address{Email: a.Address}
	if a.Name != "" {
		n := cleanText(a.Name)
		addr.Name = &n
	}
	return addr
}

// splitTopLevel splits on sep outside quotes, comments and angle brackets.
func splitTopLevel(s string, sep rune) []string {
	var parts []string
	var depthParen, depthAngle int
	var quoted, escaped bool
	start := 0
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case quoted:
		case r == '(':
			depthParen++
		case r == ')' && depthParen > 0:
			depthParen--
		case r == '<':
			depthAngle++
		case r == '>' && depthAngle > 0:
			depthAngle--
		case r == sep && depthParen == 0 && depthAngle == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

var msgIDPattern = regexp.MustCompile(`<([^<>\s]+)>`)

// AsMessageIds is the MessageIds form, or nil when there are none.
func AsMessageIds(raw string) []string {
	m := msgIDPattern.FindAllStringSubmatch(unfold(raw), -1)
	if len(m) == 0 {
		return nil
	}
	out := make([]string, len(m))
	for i, x := range m {
		out[i] = x[1]
	}
	return out
}

// AsDate is the Date form, or nil when it does not parse.
func AsDate(raw string) *time.Time {
	t, err := mail.ParseDate(strings.TrimSpace(unfold(raw)))
	if err != nil {
		return nil
	}
	return &t
}

// AsURLs is the URLs form (List-* headers), or nil when there are none.
func AsURLs(raw string) []string {
	m := msgIDPattern.FindAllStringSubmatch(unfold(raw), -1)
	if len(m) == 0 {
		return nil
	}
	out := make([]string, len(m))
	for i, x := range m {
		out[i] = x[1]
	}
	return out
}
