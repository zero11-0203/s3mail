package jmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Composition limits besides the 20 MiB message cap.
const (
	maxComposeParts   = 64
	maxComposeDepth   = 8
	maxHeaderValueLen = 8192
)

// emailAddress is an RFC 8621 §4.1.2.3 EmailAddress.
type emailAddress struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// composer turns an Email/set create object into MIME (RFC 8621 §4.6).
type composer struct {
	s      *Server
	ctx    context.Context
	c      *call
	values map[string]struct {
		Value             string `json:"value"`
		IsEncodingProblem bool   `json:"isEncodingProblem"`
		IsTruncated       bool   `json:"isTruncated"`
	}
	usedValues map[string]bool
	parts      int
	headers    []headerLine
	from       string
}

type headerLine struct{ name, value string }

// mimePart is a composed entity before serialization.
type mimePart struct {
	headers []headerLine
	body    []byte
	subs    []*mimePart
	mtype   string
}

var settableEmailProperties = []string{"mailboxIds", "keywords", "receivedAt", "messageId", "inReplyTo", "references", "sender", "from",
	"to", "cc", "bcc", "replyTo", "subject", "sentAt", "headers", "bodyStructure", "bodyValues", "textBody", "htmlBody", "attachments"}

// createEmail composes and archives a new Email (RFC 8621 §4.6).
func (s *Server) createEmail(ctx context.Context, c *call, raw json.RawMessage) (map[string]any, State, *SetError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, State{}, invalidProps("not an object")
	}
	for k := range obj {
		if !slices.Contains(settableEmailProperties, k) && !strings.HasPrefix(k, "header:") {
			return nil, State{}, invalidProps("property is server-set or unknown", k)
		}
	}
	mailboxes, serr := parseMailboxIDs(c, obj["mailboxIds"])
	if serr != nil {
		return nil, State{}, serr
	}
	keywords, serr := parseKeywords(obj["keywords"])
	if serr != nil {
		return nil, State{}, serr
	}
	received, serr := parseReceivedAt(obj["receivedAt"], s.now())
	if serr != nil {
		return nil, State{}, serr
	}
	cp := &composer{s: s, ctx: ctx, c: c, usedValues: map[string]bool{}}
	msg, serr := cp.compose(obj)
	if serr != nil {
		return nil, State{}, serr
	}
	e, st, serr := s.createFromRaw(ctx, newEmail{raw: msg, mailboxIDs: mailboxes, keywords: keywords, receivedAt: received, source: model.SourceComposed})
	if serr != nil {
		return nil, State{}, serr
	}
	return createdEmail(e), st, nil
}

func (cp *composer) compose(obj map[string]json.RawMessage) ([]byte, *SetError) {
	if raw, ok := obj["bodyValues"]; ok {
		if err := json.Unmarshal(raw, &cp.values); err != nil {
			return nil, invalidProps("bodyValues is a map of EmailBodyValue", "bodyValues")
		}
		for id, v := range cp.values {
			if v.IsEncodingProblem || v.IsTruncated {
				return nil, invalidProps("bodyValues "+id+" must not be truncated or have an encoding problem", "bodyValues")
			}
		}
	}
	if serr := cp.topHeaders(obj); serr != nil {
		return nil, serr
	}
	root, serr := cp.body(obj)
	if serr != nil {
		return nil, serr
	}
	for id := range cp.values {
		if !cp.usedValues[id] {
			return nil, invalidProps("bodyValues "+id+" is not referenced by any part", "bodyValues")
		}
	}
	var buf bytes.Buffer
	for _, h := range cp.headers {
		writeHeader(&buf, h)
	}
	writeHeader(&buf, headerLine{"MIME-Version", "1.0"})
	root.write(&buf)
	return buf.Bytes(), nil
}

// topHeaders renders the Email-level header properties in a fixed order.
func (cp *composer) topHeaders(obj map[string]json.RawMessage) *SetError {
	seen := map[string]string{}
	add := func(prop, name, value string) *SetError {
		key := strings.ToLower(name)
		if strings.HasPrefix(key, "content-") || key == "mime-version" {
			return invalidProps(name+" belongs to body parts", prop)
		}
		if other, dup := seen[key]; dup && other != prop {
			return invalidProps(name+" is set by both "+other+" and "+prop, prop, other)
		}
		seen[key] = prop
		cp.headers = append(cp.headers, headerLine{name, value})
		return nil
	}
	sentAt := cp.s.now()
	if raw, ok := obj["sentAt"]; ok && string(raw) != "null" {
		var v string
		t, err := time.Parse(time.RFC3339, unquote(raw, &v))
		if err != nil {
			return invalidProps("sentAt is a Date", "sentAt")
		}
		sentAt = t
	}
	if _, custom := obj["header:Date"]; !custom {
		if serr := add("sentAt", "Date", sentAt.Format(time.RFC1123Z)); serr != nil {
			return serr
		}
	}
	for _, f := range []struct{ prop, name string }{{"from", "From"}, {"sender", "Sender"}, {"replyTo", "Reply-To"}, {"to", "To"}, {"cc", "Cc"}, {"bcc", "Bcc"}} {
		raw, ok := obj[f.prop]
		if !ok || string(raw) == "null" {
			continue
		}
		var list []emailAddress
		if err := json.Unmarshal(raw, &list); err != nil {
			return invalidProps(f.prop+" is a list of EmailAddress", f.prop)
		}
		v, serr := formatAddresses(f.prop, list)
		if serr != nil {
			return serr
		}
		if f.prop == "from" && len(list) > 0 {
			cp.from = list[0].Email
		}
		if serr := add(f.prop, f.name, v); serr != nil {
			return serr
		}
	}
	if raw, ok := obj["subject"]; ok && string(raw) != "null" {
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return invalidProps("subject is a string", "subject")
		}
		if serr := add("subject", "Subject", encodeText(v)); serr != nil {
			return serr
		}
	}
	for _, f := range []struct{ prop, name string }{{"messageId", "Message-ID"}, {"inReplyTo", "In-Reply-To"}, {"references", "References"}} {
		raw, ok := obj[f.prop]
		if !ok || string(raw) == "null" {
			continue
		}
		v, serr := formatMessageIDs(f.prop, raw)
		if serr != nil {
			return serr
		}
		if serr := add(f.prop, f.name, v); serr != nil {
			return serr
		}
	}
	if _, ok := seen["message-id"]; !ok {
		if _, custom := obj["header:Message-ID"]; !custom {
			if serr := add("messageId", "Message-ID", cp.newMessageID()); serr != nil {
				return serr
			}
		}
	}
	lines, serr := customHeaders(obj, "")
	if serr != nil {
		return serr
	}
	for _, h := range lines {
		if serr := add(h.prop, h.name, h.value); serr != nil {
			return serr
		}
	}
	return nil
}

func unquote(raw json.RawMessage, v *string) string {
	_ = json.Unmarshal(raw, v)
	return *v
}

func (cp *composer) newMessageID() string {
	domain := "s3mail.invalid"
	if _, d, ok := strings.Cut(cp.from, "@"); ok && d != "" {
		domain = strings.ToLower(d)
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

type propLine struct{ prop, name, value string }

// customHeaders reads "headers" and "header:{name}[:as{form}][:all]"
// properties of an Email or body part.
func customHeaders(obj map[string]json.RawMessage, where string) ([]propLine, *SetError) {
	var out []propLine
	if raw, ok := obj["headers"]; ok && string(raw) != "null" {
		var list []struct{ Name, Value string }
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, invalidProps("headers is a list of EmailHeader", where+"headers")
		}
		for _, h := range list {
			if !validHeaderName(h.Name) || !validRawValue(h.Value) {
				return nil, invalidProps("invalid header "+h.Name, where+"headers")
			}
			out = append(out, propLine{"headers", h.Name, strings.TrimSpace(h.Value)})
		}
	}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range obj {
			if strings.HasPrefix(k, "header:") && !yield(k) {
				return
			}
		}
	}) {
		hp, ok := parseHeaderProp(k)
		if !ok || !validHeaderName(hp.name) {
			return nil, invalidProps("invalid header property", where+k)
		}
		var raws []json.RawMessage
		if hp.all {
			if err := json.Unmarshal(obj[k], &raws); err != nil {
				return nil, invalidProps(":all takes a list", where+k)
			}
		} else {
			raws = []json.RawMessage{obj[k]}
		}
		for _, raw := range raws {
			v, serr := formatHeaderForm(where+k, hp.form, raw)
			if serr != nil {
				return nil, serr
			}
			out = append(out, propLine{k, hp.name, v})
		}
	}
	return out, nil
}

func formatHeaderForm(prop, form string, raw json.RawMessage) (string, *SetError) {
	switch form {
	case "asRaw":
		var v string
		if json.Unmarshal(raw, &v) != nil || !validRawValue(v) {
			return "", invalidProps("asRaw takes header text without bare line breaks", prop)
		}
		return strings.TrimSpace(v), nil
	case "asText":
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", invalidProps("asText takes a string", prop)
		}
		return encodeText(v), nil
	case "asAddresses":
		var list []emailAddress
		if json.Unmarshal(raw, &list) != nil {
			return "", invalidProps("asAddresses takes a list of EmailAddress", prop)
		}
		return formatAddresses(prop, list)
	case "asMessageIds":
		return formatMessageIDs(prop, raw)
	case "asDate":
		var v string
		t, err := time.Parse(time.RFC3339, unquote(raw, &v))
		if err != nil {
			return "", invalidProps("asDate takes a Date", prop)
		}
		return t.Format(time.RFC1123Z), nil
	case "asURLs":
		var list []string
		if json.Unmarshal(raw, &list) != nil {
			return "", invalidProps("asURLs takes a list of strings", prop)
		}
		for i, u := range list {
			if strings.ContainsAny(u, "<>\r\n \t") {
				return "", invalidProps("invalid URL", prop)
			}
			list[i] = "<" + u + ">"
		}
		return strings.Join(list, ", "), nil
	}
	return "", invalidProps("unsupported header form "+form, prop)
}

func validHeaderName(n string) bool {
	if n == "" || len(n) > 76 {
		return false
	}
	for _, r := range n {
		if r < 33 || r > 126 || r == ':' {
			return false
		}
	}
	return true
}

// validRawValue accepts folded header text: line breaks only as CRLF
// followed by whitespace.
func validRawValue(v string) bool {
	if len(v) > maxHeaderValueLen || !utf8.ValidString(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\r':
			if i+2 >= len(v) || v[i+1] != '\n' || (v[i+2] != ' ' && v[i+2] != '\t') {
				return false
			}
			i++
		case '\n', 0:
			return false
		}
	}
	return true
}

func formatAddresses(prop string, list []emailAddress) (string, *SetError) {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if _, err := mail.ParseAddress("<" + a.Email + ">"); err != nil || strings.ContainsAny(a.Email, "\r\n<>") {
			return "", invalidProps("invalid email address", prop)
		}
		addr := mail.Address{Address: a.Email}
		if a.Name != nil {
			addr.Name = strings.Map(dropControls, *a.Name)
		}
		out = append(out, addr.String())
	}
	return strings.Join(out, ", "), nil
}

func formatMessageIDs(prop string, raw json.RawMessage) (string, *SetError) {
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil || len(ids) == 0 {
		return "", invalidProps(prop+" is a list of message ids", prop)
	}
	for i, id := range ids {
		local, domain, ok := strings.Cut(id, "@")
		if !ok || local == "" || domain == "" || strings.ContainsAny(id, "<> \t\r\n\"") {
			return "", invalidProps("invalid message id "+id, prop)
		}
		ids[i] = "<" + id + ">"
	}
	return strings.Join(ids, " "), nil
}

func dropControls(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

// encodeText encodes unstructured header text (RFC 2047) when needed.
func encodeText(s string) string {
	s = strings.Map(dropControls, s)
	for _, r := range s {
		if r >= 0x80 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// body builds the part tree from bodyStructure or the textBody, htmlBody
// and attachments shortcut (RFC 8621 §4.6).
func (cp *composer) body(obj map[string]json.RawMessage) (*mimePart, *SetError) {
	_, structured := obj["bodyStructure"]
	_, text := obj["textBody"]
	_, html := obj["htmlBody"]
	_, atts := obj["attachments"]
	if structured {
		if text || html || atts {
			return nil, invalidProps("bodyStructure excludes textBody, htmlBody and attachments", "bodyStructure")
		}
		return cp.part(obj["bodyStructure"], "bodyStructure", 0)
	}
	one := func(prop, want string) (*mimePart, *SetError) {
		raw, ok := obj[prop]
		if !ok || string(raw) == "null" {
			return nil, nil
		}
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil || len(list) > 1 {
			return nil, invalidProps(prop+" holds at most one part", prop)
		}
		if len(list) == 0 {
			return nil, nil
		}
		p, serr := cp.part(list[0], prop, 1)
		if serr == nil && p.mtype != want {
			serr = invalidProps(prop+" must be "+want, prop)
		}
		return p, serr
	}
	textPart, serr := one("textBody", "text/plain")
	if serr != nil {
		return nil, serr
	}
	htmlPart, serr := one("htmlBody", "text/html")
	if serr != nil {
		return nil, serr
	}
	var inline, attached []*mimePart
	if raw, ok := obj["attachments"]; ok && string(raw) != "null" {
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, invalidProps("attachments is a list of EmailBodyPart", "attachments")
		}
		for _, r := range list {
			p, serr := cp.part(r, "attachments", 1)
			if serr != nil {
				return nil, serr
			}
			if htmlPart != nil && p.isInline() {
				inline = append(inline, p)
			} else {
				attached = append(attached, p)
			}
		}
	}
	if htmlPart != nil && len(inline) > 0 {
		htmlPart = multipart("multipart/related", append([]*mimePart{htmlPart}, inline...))
	}
	var root *mimePart
	switch {
	case textPart != nil && htmlPart != nil:
		root = multipart("multipart/alternative", []*mimePart{textPart, htmlPart})
	case textPart != nil:
		root = textPart
	case htmlPart != nil:
		root = htmlPart
	}
	if len(attached) > 0 {
		if root != nil {
			attached = append([]*mimePart{root}, attached...)
		}
		root = multipart("multipart/mixed", attached)
	}
	if root == nil {
		root = &mimePart{mtype: "text/plain", headers: []headerLine{{"Content-Type", "text/plain; charset=utf-8"}}}
	}
	return root, nil
}

func (p *mimePart) isInline() bool {
	var cid, inline bool
	for _, h := range p.headers {
		switch strings.ToLower(h.name) {
		case "content-id":
			cid = true
		case "content-disposition":
			inline = strings.HasPrefix(strings.ToLower(h.value), "inline")
		}
	}
	return cid && inline
}

func multipart(mtype string, subs []*mimePart) *mimePart {
	return &mimePart{mtype: mtype, subs: subs, headers: []headerLine{{"Content-Type", mime.FormatMediaType(mtype, map[string]string{"boundary": newBoundary()})}}}
}

func newBoundary() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "=_" + hex.EncodeToString(b[:])
}

var partKeys = []string{"partId", "blobId", "size", "headers", "name", "type", "charset", "disposition", "cid", "language", "location", "subParts"}

// part builds one EmailBodyPart.
func (cp *composer) part(raw json.RawMessage, prop string, depth int) (*mimePart, *SetError) {
	cp.parts++
	if cp.parts > maxComposeParts || depth > maxComposeDepth {
		return nil, invalidProps("too many or too deeply nested body parts", prop)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, invalidProps("body part is not an object", prop)
	}
	for k := range obj {
		if !slices.Contains(partKeys, k) && !strings.HasPrefix(k, "header:") {
			return nil, invalidProps("unknown body part property "+k, prop)
		}
	}
	str := func(k string) string {
		var v string
		if raw, ok := obj[k]; ok {
			_ = json.Unmarshal(raw, &v)
		}
		return v
	}
	mtype := strings.ToLower(str("type"))
	if _, has := obj["subParts"]; has && string(obj["subParts"]) != "null" {
		if !strings.HasPrefix(mtype, "multipart/") {
			return nil, invalidProps("a part with subParts must be multipart/*", prop)
		}
		var list []json.RawMessage
		if err := json.Unmarshal(obj["subParts"], &list); err != nil || len(list) == 0 {
			return nil, invalidProps("subParts is a non-empty list", prop)
		}
		subs := make([]*mimePart, 0, len(list))
		for _, r := range list {
			p, serr := cp.part(r, prop, depth+1)
			if serr != nil {
				return nil, serr
			}
			subs = append(subs, p)
		}
		return multipart(mtype, subs), nil
	}
	if strings.HasPrefix(mtype, "multipart/") {
		return nil, invalidProps("a multipart part needs subParts", prop)
	}
	partID, blobID := str("partId"), str("blobId")
	var content []byte
	params := map[string]string{}
	switch {
	case partID != "" && blobID != "":
		return nil, invalidProps("a part has either partId or blobId", prop)
	case partID != "":
		v, ok := cp.values[partID]
		if !ok {
			return nil, invalidProps("no bodyValues entry "+partID, prop)
		}
		if _, set := obj["charset"]; set {
			return nil, invalidProps("charset is implied (UTF-8) for bodyValues parts", prop)
		}
		if mtype == "" {
			mtype = "text/plain"
		}
		if !strings.HasPrefix(mtype, "text/") {
			return nil, invalidProps("bodyValues parts are text/*", prop)
		}
		cp.usedValues[partID] = true
		content = []byte(v.Value)
		params["charset"] = "utf-8"
	case blobID != "":
		if r, ok := cp.c.resolveID(blobID); ok {
			blobID = r
		}
		b, ok, err := cp.s.readBlob(cp.ctx, blobID)
		if err != nil {
			return nil, &SetError{Type: "serverFail"}
		}
		if !ok {
			return nil, &SetError{Type: "blobNotFound", NotFound: []string{blobID}}
		}
		content = b.data
		if mtype == "" {
			mtype = b.typ
		}
		if mtype == "" {
			mtype = "application/octet-stream"
		}
		if cs := str("charset"); cs != "" && strings.HasPrefix(mtype, "text/") {
			params["charset"] = cs
		}
	default:
		return nil, invalidProps("a leaf part needs partId or blobId", prop)
	}
	if _, _, err := mime.ParseMediaType(mtype); err != nil || strings.ContainsAny(mtype, ";\r\n") {
		return nil, invalidProps("invalid media type "+mtype, prop)
	}
	name := strings.Map(dropControls, str("name"))
	if name != "" && str("disposition") == "" {
		params["name"] = name
	}
	p := &mimePart{mtype: mtype, headers: []headerLine{{"Content-Type", mime.FormatMediaType(mtype, params)}}}
	if d := strings.ToLower(str("disposition")); d != "" {
		if d != "inline" && d != "attachment" {
			return nil, invalidProps("disposition is inline or attachment", prop)
		}
		dp := map[string]string{}
		if name != "" {
			dp["filename"] = name
		}
		p.headers = append(p.headers, headerLine{"Content-Disposition", mime.FormatMediaType(d, dp)})
	}
	if cid := str("cid"); cid != "" {
		if strings.ContainsAny(cid, "<> \t\r\n") {
			return nil, invalidProps("invalid cid", prop)
		}
		p.headers = append(p.headers, headerLine{"Content-ID", "<" + cid + ">"})
	}
	if raw, ok := obj["language"]; ok && string(raw) != "null" {
		var langs []string
		if json.Unmarshal(raw, &langs) != nil || slices.ContainsFunc(langs, func(l string) bool { return !validHeaderName(l) }) {
			return nil, invalidProps("language is a list of tags", prop)
		}
		p.headers = append(p.headers, headerLine{"Content-Language", strings.Join(langs, ", ")})
	}
	if loc := str("location"); loc != "" {
		if !validRawValue(loc) {
			return nil, invalidProps("invalid location", prop)
		}
		p.headers = append(p.headers, headerLine{"Content-Location", loc})
	}
	lines, serr := customHeaders(obj, prop+"/")
	if serr != nil {
		return nil, serr
	}
	for _, h := range lines {
		lower := strings.ToLower(h.name)
		if lower == "content-transfer-encoding" || slices.ContainsFunc(p.headers, func(x headerLine) bool { return strings.EqualFold(x.name, h.name) }) {
			return nil, invalidProps(h.name+" is set by the server or by another property", prop)
		}
		p.headers = append(p.headers, headerLine{h.name, h.value})
	}
	p.encode(content, partID != "")
	return p, nil
}

// encode picks the transfer encoding: text from bodyValues is 7bit or
// quoted-printable with CRLF lines; message/rfc822 stays 7bit/8bit as
// RFC 2046 requires; other blobs are base64 so bytes are preserved.
func (p *mimePart) encode(content []byte, text bool) {
	switch {
	case text:
		content = bytes.ReplaceAll(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
		if plain7bit(content) {
			p.headers = append(p.headers, headerLine{"Content-Transfer-Encoding", "7bit"})
			p.body = content
			return
		}
		var buf bytes.Buffer
		w := quotedprintable.NewWriter(&buf)
		_, _ = w.Write(content)
		_ = w.Close()
		p.headers = append(p.headers, headerLine{"Content-Transfer-Encoding", "quoted-printable"})
		p.body = buf.Bytes()
	case p.mtype == "message/rfc822" || p.mtype == "message/global":
		cte := "7bit"
		if !plain7bit(content) {
			cte = "8bit"
		}
		p.headers = append(p.headers, headerLine{"Content-Transfer-Encoding", cte})
		p.body = content
	default:
		p.headers = append(p.headers, headerLine{"Content-Transfer-Encoding", "base64"})
		enc := base64.StdEncoding.EncodeToString(content)
		var buf bytes.Buffer
		for len(enc) > 76 {
			buf.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		buf.WriteString(enc)
		p.body = buf.Bytes()
	}
}

func plain7bit(b []byte) bool {
	line := 0
	for _, c := range b {
		switch {
		case c == '\n':
			line = 0
		case c >= 0x80 || c == 0:
			return false
		default:
			line++
			if line > 998 {
				return false
			}
		}
	}
	return true
}

func (p *mimePart) write(buf *bytes.Buffer) {
	for _, h := range p.headers {
		writeHeader(buf, h)
	}
	buf.WriteString("\r\n")
	if len(p.subs) == 0 {
		buf.Write(p.body)
		if len(p.body) > 0 && !bytes.HasSuffix(p.body, []byte("\r\n")) {
			buf.WriteString("\r\n")
		}
		return
	}
	_, params, _ := mime.ParseMediaType(p.headers[0].value)
	b := params["boundary"]
	for _, s := range p.subs {
		fmt.Fprintf(buf, "--%s\r\n", b)
		s.write(buf)
	}
	fmt.Fprintf(buf, "--%s--\r\n", b)
}

// writeHeader writes one field, folding at spaces to keep lines short.
func writeHeader(buf *bytes.Buffer, h headerLine) {
	line := h.name + ": "
	col := len(line)
	buf.WriteString(line)
	words := strings.Split(h.value, " ")
	for i, w := range words {
		if i > 0 {
			if col+1+len(w) > 76 && col > len(h.name)+2 {
				buf.WriteString("\r\n ")
				col = 1
			} else {
				buf.WriteByte(' ')
				col++
			}
		}
		buf.WriteString(w)
		col += len(w)
	}
	buf.WriteString("\r\n")
}
