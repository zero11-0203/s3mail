package jmap

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
)

var (
	metadataProperties = []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt"}
	headerProperties   = []string{"headers", "messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt"}
	bodyProperties     = []string{"bodyStructure", "bodyValues", "textBody", "htmlBody", "attachments", "hasAttachment", "preview"}

	defaultEmailProperties = []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt",
		"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt",
		"hasAttachment", "preview", "bodyValues", "textBody", "htmlBody", "attachments"}

	partProperties        = []string{"partId", "blobId", "size", "headers", "name", "type", "charset", "disposition", "cid", "language", "location", "subParts"}
	defaultPartProperties = []string{"partId", "blobId", "size", "name", "type", "charset", "disposition", "cid", "language", "location"}
)

type emailGetArgs struct {
	AccountID           string    `json:"accountId"`
	IDs                 *[]string `json:"ids"`
	Properties          *[]string `json:"properties"`
	BodyProperties      *[]string `json:"bodyProperties"`
	FetchTextBodyValues bool      `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool      `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool      `json:"fetchAllBodyValues"`
	MaxBodyValueBytes   int       `json:"maxBodyValueBytes"`
}

// headerProp is a parsed "header:{name}[:as{Form}][:all]" property.
type headerProp struct {
	name, form string
	all        bool
}

func parseHeaderProp(p string) (headerProp, bool) {
	rest, ok := strings.CutPrefix(p, "header:")
	if !ok || rest == "" {
		return headerProp{}, false
	}
	parts := strings.Split(rest, ":")
	h := headerProp{name: parts[0], form: "asRaw"}
	if h.name == "" || len(parts) > 3 {
		return headerProp{}, false
	}
	for _, x := range parts[1:] {
		switch {
		case x == "all" && !h.all:
			h.all = true
		case strings.HasPrefix(x, "as") && h.form == "asRaw" && !h.all:
			h.form = x
		default:
			return headerProp{}, false
		}
	}
	switch h.form {
	case "asRaw", "asText", "asAddresses", "asGroupedAddresses", "asMessageIds", "asDate", "asURLs":
		return h, true
	}
	return headerProp{}, false
}

func headerValue(fields mailparse.Headers, h headerProp) any {
	values := fields.All(h.name)
	conv := func(raw string) any {
		switch h.form {
		case "asText":
			return mailparse.AsText(raw)
		case "asAddresses":
			return mailparse.AsAddresses(raw)
		case "asGroupedAddresses":
			return mailparse.AsGroupedAddresses(raw)
		case "asMessageIds":
			if ids := mailparse.AsMessageIds(raw); ids != nil {
				return ids
			}
			return nil
		case "asDate":
			if d := mailparse.AsDate(raw); d != nil {
				return d.Format(time.RFC3339)
			}
			return nil
		case "asURLs":
			if u := mailparse.AsURLs(raw); u != nil {
				return u
			}
			return nil
		}
		return raw
	}
	if h.all {
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = conv(v)
		}
		return out
	}
	if len(values) == 0 {
		return nil
	}
	return conv(values[len(values)-1])
}

func validProps(req []string, known []string) *MethodError {
	for _, p := range req {
		if slices.Contains(known, p) {
			continue
		}
		if _, ok := parseHeaderProp(p); ok {
			continue
		}
		return methodErr("invalidArguments", "unknown property "+p)
	}
	return nil
}

func emailGet(ctx context.Context, c *call) (any, *MethodError) {
	var a emailGetArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	props := defaultEmailProperties
	if a.Properties != nil {
		props = append([]string{"id"}, *a.Properties...)
	}
	if e := validProps(props, slices.Concat(metadataProperties, headerProperties, bodyProperties)); e != nil {
		return nil, e
	}
	partProps := defaultPartProperties
	if a.BodyProperties != nil {
		partProps = *a.BodyProperties
	}
	if e := validProps(partProps, partProperties); e != nil {
		return nil, e
	}
	if a.MaxBodyValueBytes < 0 {
		return nil, methodErr("invalidArguments", "maxBodyValueBytes must not be negative")
	}

	var found map[string]EmailMeta
	var ids []string
	st, e := c.s.consistent(ctx, func(State) error {
		if a.IDs == nil {
			all, err := c.s.Store.ListEmails(ctx)
			if err != nil {
				return err
			}
			found = map[string]EmailMeta{}
			ids = nil
			for _, m := range all {
				found[m.ID] = m
				ids = append(ids, m.ID)
			}
			return nil
		}
		ids = *a.IDs
		if len(ids) > MaxObjectsInGet {
			return nil
		}
		var err error
		found, err = c.s.Store.GetEmails(ctx, uniqueValid(ids))
		return err
	})
	if e != nil {
		return nil, e
	}
	if len(ids) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "at most 64 ids per Email/get")
	}

	needRaw := slices.ContainsFunc(props, func(p string) bool {
		return !slices.Contains(metadataProperties, p)
	})
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st), List: []map[string]any{}, NotFound: []string{}}
	objs := make([]map[string]any, len(ids))
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	seen := map[string]bool{}
	for i, id := range ids {
		m, ok := found[id]
		if !ok || seen[id] {
			if !ok && !slices.Contains(resp.NotFound, id) {
				resp.NotFound = append(resp.NotFound, id)
			}
			continue
		}
		seen[id] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var msg *mailparse.Message
			if needRaw {
				raw, err := c.s.Originals.Fetch(ctx, m.Blob)
				if err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					return
				}
				msg = mailparse.Parse(raw)
			}
			objs[i] = buildEmail(m, msg, props, partProps, a)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, c.s.fail("read original", firstErr)
	}
	for _, o := range objs {
		if o != nil {
			resp.List = append(resp.List, o)
		}
	}
	return resp, nil
}

func uniqueValid(ids []string) []string {
	var out []string
	for _, id := range ids {
		if len(id) > 1 && id[0] == 'M' && validCore(id[1:]) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func boolMap(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		if v {
			out[k] = true
		}
	}
	return out
}

func addressesOrNil(h mailparse.Headers, name string) any {
	v, ok := h.Last(name)
	if !ok {
		return nil
	}
	return mailparse.AsAddresses(v)
}

func buildEmail(m EmailMeta, msg *mailparse.Message, props, partProps []string, a emailGetArgs) map[string]any {
	out := map[string]any{}
	var bodies mailparse.Bodies
	if msg != nil && slices.ContainsFunc(props, func(p string) bool { return slices.Contains(bodyProperties, p) }) {
		bodies = msg.BodyLists()
	}
	for _, p := range props {
		switch p {
		case "id":
			out[p] = m.ID
		case "blobId":
			out[p] = rawBlobID(m.ID)
		case "threadId":
			out[p] = m.ThreadID
		case "mailboxIds":
			out[p] = boolMap(m.MailboxIDs)
		case "keywords":
			out[p] = boolMap(m.Keywords)
		case "size":
			out[p] = m.Size
		case "receivedAt":
			out[p] = utcDate(m.ReceivedAt)
		case "headers":
			out[p] = fieldsOf(msg.Headers)
		case "messageId", "inReplyTo", "references":
			name := map[string]string{"messageId": "Message-ID", "inReplyTo": "In-Reply-To", "references": "References"}[p]
			out[p] = headerValue(msg.Headers, headerProp{name: name, form: "asMessageIds"})
		case "sender", "from", "to", "cc", "bcc", "replyTo":
			name := map[string]string{"sender": "Sender", "from": "From", "to": "To", "cc": "Cc", "bcc": "Bcc", "replyTo": "Reply-To"}[p]
			out[p] = addressesOrNil(msg.Headers, name)
		case "subject":
			out[p] = headerValue(msg.Headers, headerProp{name: "Subject", form: "asText"})
		case "sentAt":
			out[p] = headerValue(msg.Headers, headerProp{name: "Date", form: "asDate"})
		case "bodyStructure":
			out[p] = partObject(m.ID, msg.Body, partProps, true)
		case "textBody", "htmlBody", "attachments":
			list := map[string][]*mailparse.Part{"textBody": bodies.Text, "htmlBody": bodies.HTML, "attachments": bodies.Attachments}[p]
			objs := []map[string]any{}
			for _, part := range list {
				objs = append(objs, partObject(m.ID, part, partProps, false))
			}
			out[p] = objs
		case "bodyValues":
			out[p] = bodyValues(msg, bodies, a)
		case "hasAttachment":
			out[p] = len(bodies.Attachments) > 0
		case "preview":
			out[p] = msg.Preview(bodies)
		default:
			if h, ok := parseHeaderProp(p); ok {
				out[p] = headerValue(msg.Headers, h)
			}
		}
	}
	return out
}

func fieldsOf(h mailparse.Headers) []mailparse.Field {
	if h == nil {
		return []mailparse.Field{}
	}
	return h
}

func bodyValues(msg *mailparse.Message, b mailparse.Bodies, a emailGetArgs) map[string]mailparse.BodyValue {
	out := map[string]mailparse.BodyValue{}
	add := func(parts []*mailparse.Part) {
		for _, p := range parts {
			if strings.HasPrefix(p.Type, "text/") && p.PartID != "" {
				out[p.PartID] = p.Value(a.MaxBodyValueBytes)
			}
		}
	}
	switch {
	case a.FetchAllBodyValues:
		add(msg.Leaves())
	default:
		if a.FetchTextBodyValues {
			add(b.Text)
		}
		if a.FetchHTMLBodyValues {
			add(b.HTML)
		}
	}
	return out
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func partObject(emailID string, p *mailparse.Part, props []string, recurse bool) map[string]any {
	out := map[string]any{}
	for _, name := range props {
		switch name {
		case "partId":
			out[name] = strOrNil(p.PartID)
		case "blobId":
			if p.PartID == "" {
				out[name] = nil
			} else {
				out[name] = partBlobID(emailID, p.PartID)
			}
		case "size":
			out[name] = p.Size
		case "headers":
			out[name] = fieldsOf(p.Headers)
		case "name":
			out[name] = strOrNil(p.Name)
		case "type":
			out[name] = p.Type
		case "charset":
			out[name] = strOrNil(p.Charset)
		case "disposition":
			out[name] = strOrNil(p.Disposition)
		case "cid":
			out[name] = strOrNil(p.CID)
		case "language":
			if len(p.Language) == 0 {
				out[name] = nil
			} else {
				out[name] = p.Language
			}
		case "location":
			out[name] = strOrNil(p.Location)
		case "subParts":
			if !p.IsMultipart() {
				out[name] = nil
				continue
			}
			subs := []map[string]any{}
			if recurse {
				for _, s := range p.SubParts {
					subs = append(subs, partObject(emailID, s, props, true))
				}
			}
			out[name] = subs
		default:
			if h, ok := parseHeaderProp(name); ok {
				out[name] = headerValue(p.Headers, h)
			}
		}
	}
	// bodyStructure must expose the tree even when subParts was not
	// requested explicitly.
	if recurse && p.IsMultipart() && !slices.Contains(props, "subParts") {
		subs := []map[string]any{}
		for _, s := range p.SubParts {
			subs = append(subs, partObject(emailID, s, props, true))
		}
		out["subParts"] = subs
	}
	return out
}
