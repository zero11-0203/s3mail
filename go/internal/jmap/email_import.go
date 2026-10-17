package jmap

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/ingest"
	"github.com/zero11-0203/s3mail/go/internal/mailparse"
	"github.com/zero11-0203/s3mail/go/internal/model"
)

// maxNewEmailBytes bounds composed and imported MIME
// (docs/specs/jmap.md#limits-and-blobs).
const maxNewEmailBytes = 20 << 20

// newEmail is a validated Email about to be archived and committed.
type newEmail struct {
	raw        []byte
	mailboxIDs map[string]bool
	keywords   map[string]bool
	receivedAt time.Time
	source     string
}

// createFromRaw archives the complete MIME first, then commits the Email
// with its thread, so an acknowledged Email always has its original
// (docs/specs/jmap.md#core-capabilities-to-implement). A failed commit
// leaves an unreferenced archived object, which is never deleted.
func (s *Server) createFromRaw(ctx context.Context, n newEmail) (EmailMeta, State, *SetError) {
	if len(n.raw) > maxNewEmailBytes {
		return EmailMeta{}, State{}, &SetError{Type: "tooLarge", Description: "messages are limited to 20 MiB"}
	}
	if serr := s.checkMailboxIDs(ctx, n.mailboxIDs); serr != nil {
		return EmailMeta{}, State{}, serr
	}
	prefix := n.raw[:min(len(n.raw), ingest.MaxHeaderBytes+1)]
	h, err := ingest.ParseHeaders(prefix, len(n.raw) > len(prefix))
	if err != nil || len(mailparse.Parse(prefix).Headers) == 0 {
		return EmailMeta{}, State{}, &SetError{Type: "invalidEmail", Description: "not an RFC 5322 message"}
	}
	id := model.NewID("M")
	kind := "imported"
	if n.source == model.SourceComposed {
		kind = "composed"
	}
	ref, err := s.Archive.Put(ctx, kind, id, n.raw)
	if err != nil {
		s.Log.Error("jmap", "outcome", "serverFail", "step", "archive", "error", err)
		return EmailMeta{}, State{}, &SetError{Type: "serverFail"}
	}
	e := model.Email{
		ID: id, MailboxIDs: maps.Clone(n.mailboxIDs), Keywords: maps.Clone(n.keywords),
		ReceivedAt: n.receivedAt.UTC().Truncate(time.Second), Size: int64(len(n.raw)), Blob: ref, Source: n.source,
		Subject: h.Subject, From: h.From, To: h.To, Cc: h.Cc, SentAt: h.Date,
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
	}
	if e.Keywords == nil {
		e.Keywords = map[string]bool{}
	}
	st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		thread, err := model.AssignThread(ctx, &e, s.Store)
		if err != nil {
			return nil, &SetError{Type: "serverFail"}
		}
		e.ThreadID = thread
		return &model.Mutation{Emails: []model.EmailChange{{After: &e}}}, nil
	})
	if serr != nil {
		return EmailMeta{}, State{}, serr
	}
	return e, st, nil
}

func createdEmail(e EmailMeta) map[string]any {
	return map[string]any{"id": e.ID, "blobId": rawBlobID(e.ID), "threadId": e.ThreadID, "size": e.Size}
}

// parseKeywords validates a create-time keywords map.
func parseKeywords(raw json.RawMessage) (map[string]bool, *SetError) {
	out := map[string]bool{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var kw map[string]bool
	if err := json.Unmarshal(raw, &kw); err != nil {
		return nil, invalidProps("keywords must be an object of true values", "keywords")
	}
	for k, v := range kw {
		if !v || !validKeyword(k) {
			return nil, invalidProps("invalid keyword "+k, "keywords")
		}
		out[strings.ToLower(k)] = true
	}
	return out, nil
}

func parseMailboxIDs(c *call, raw json.RawMessage) (map[string]bool, *SetError) {
	var ids map[string]bool
	if len(raw) == 0 || json.Unmarshal(raw, &ids) != nil {
		return nil, invalidProps("mailboxIds is required: an object of true values", "mailboxIds")
	}
	out := map[string]bool{}
	for id, v := range ids {
		if !v {
			return nil, invalidProps("mailboxIds values must be true", "mailboxIds")
		}
		out[resolveMailbox(c, id)] = true
	}
	return out, nil
}

func parseReceivedAt(raw json.RawMessage, now time.Time) (time.Time, *SetError) {
	if len(raw) == 0 || string(raw) == "null" {
		return now, nil
	}
	var t time.Time
	if err := json.Unmarshal(raw, &t); err != nil {
		return time.Time{}, invalidProps("receivedAt is a UTCDate", "receivedAt")
	}
	return t, nil
}

type importArgs struct {
	AccountID string                                `json:"accountId"`
	IfInState *string                               `json:"ifInState"`
	Emails    map[string]map[string]json.RawMessage `json:"emails"`
}

type importResponse struct {
	AccountID  string         `json:"accountId"`
	OldState   string         `json:"oldState"`
	NewState   string         `json:"newState"`
	Created    map[string]any `json:"created"`
	NotCreated setErrors      `json:"notCreated"`
}

// emailImport implements Email/import (RFC 8621 §4.8) from uploads or
// existing blobs.
func emailImport(ctx context.Context, c *call) (any, *MethodError) {
	var a importArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if len(a.Emails) > MaxObjectsInSet {
		return nil, methodErr("requestTooLarge", "")
	}
	start, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	if a.IfInState != nil && !inState(*a.IfInState, start, "Email") {
		return nil, methodErr("stateMismatch", "")
	}
	resp := importResponse{AccountID: a.AccountID, OldState: stateToken(start, "Email"), Created: map[string]any{}, NotCreated: setErrors{}}
	last := start
	for _, cid := range slices.Sorted(maps.Keys(a.Emails)) {
		e, st, serr := c.s.importOne(ctx, c, a.Emails[cid])
		if serr != nil {
			resp.NotCreated[cid] = serr
			continue
		}
		if st.Seq > last.Seq {
			last = st
		}
		c.created[cid] = e.ID
		resp.Created[cid] = createdEmail(e)
	}
	resp.NewState = stateToken(last, "Email")
	return resp, nil
}

func (s *Server) importOne(ctx context.Context, c *call, obj map[string]json.RawMessage) (EmailMeta, State, *SetError) {
	for k := range obj {
		if !slices.Contains([]string{"blobId", "mailboxIds", "keywords", "receivedAt"}, k) {
			return EmailMeta{}, State{}, invalidProps("unknown property", k)
		}
	}
	var blobID string
	if json.Unmarshal(obj["blobId"], &blobID) != nil || blobID == "" {
		return EmailMeta{}, State{}, invalidProps("blobId is required", "blobId")
	}
	if r, ok := c.resolveID(blobID); ok {
		blobID = r
	}
	mailboxes, serr := parseMailboxIDs(c, obj["mailboxIds"])
	if serr != nil {
		return EmailMeta{}, State{}, serr
	}
	keywords, serr := parseKeywords(obj["keywords"])
	if serr != nil {
		return EmailMeta{}, State{}, serr
	}
	received, serr := parseReceivedAt(obj["receivedAt"], s.now())
	if serr != nil {
		return EmailMeta{}, State{}, serr
	}
	b, ok, err := s.readBlob(ctx, blobID)
	if err != nil {
		s.Log.Error("jmap", "outcome", "serverFail", "step", "read blob", "error", err)
		return EmailMeta{}, State{}, &SetError{Type: "serverFail"}
	}
	if !ok {
		return EmailMeta{}, State{}, &SetError{Type: "blobNotFound", NotFound: []string{blobID}}
	}
	return s.createFromRaw(ctx, newEmail{raw: b.data, mailboxIDs: mailboxes, keywords: keywords, receivedAt: received, source: model.SourceImported})
}

type parseArgs struct {
	AccountID           string    `json:"accountId"`
	BlobIDs             []string  `json:"blobIds"`
	Properties          *[]string `json:"properties"`
	BodyProperties      *[]string `json:"bodyProperties"`
	FetchTextBodyValues bool      `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool      `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool      `json:"fetchAllBodyValues"`
	MaxBodyValueBytes   int       `json:"maxBodyValueBytes"`
}

// Email/parse returns no object-only properties.
var parseProperties = []string{"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt",
	"hasAttachment", "preview", "bodyValues", "textBody", "htmlBody", "attachments"}

// emailParse implements Email/parse (RFC 8621 §4.9) for message blobs.
func emailParse(ctx context.Context, c *call) (any, *MethodError) {
	var a parseArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if len(a.BlobIDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	props := append([]string{"blobId", "size"}, parseProperties...)
	if a.Properties != nil {
		props = *a.Properties
	}
	allowed := slices.Concat([]string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt"}, headerProperties, bodyProperties)
	if e := validProps(props, allowed); e != nil {
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
	getArgs := emailGetArgs{FetchTextBodyValues: a.FetchTextBodyValues, FetchHTMLBodyValues: a.FetchHTMLBodyValues,
		FetchAllBodyValues: a.FetchAllBodyValues, MaxBodyValueBytes: a.MaxBodyValueBytes}
	resp := map[string]any{"accountId": a.AccountID, "parsed": map[string]any{}, "notParsable": []string{}, "notFound": []string{}}
	parsed := resp["parsed"].(map[string]any)
	for _, id := range a.BlobIDs {
		if _, done := parsed[id]; done {
			continue
		}
		b, ok, err := c.s.readBlob(ctx, id)
		if err != nil {
			return nil, c.s.fail("read blob", err)
		}
		if !ok {
			resp["notFound"] = append(resp["notFound"].([]string), id)
			continue
		}
		msg := mailparse.Parse(b.data)
		if len(msg.Headers) == 0 {
			resp["notParsable"] = append(resp["notParsable"].([]string), id)
			continue
		}
		ref, _ := ParseBlobID(id)
		obj := buildEmail(EmailMeta{Size: int64(len(b.data))}, ref.names(id), msg, props, partProps, getArgs)
		for _, p := range []string{"id", "threadId", "mailboxIds", "keywords", "receivedAt"} {
			if _, ok := obj[p]; ok {
				obj[p] = nil
			}
		}
		parsed[id] = obj
	}
	return resp, nil
}
