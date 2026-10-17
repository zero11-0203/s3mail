package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/send"
)

// Submission limits (docs/specs/jmap.md#limits-and-blobs).
const (
	maxSubmissionSize = 20 << 20
	maxRecipients     = 100
)

var submissionProperties = []string{"id", "identityId", "emailId", "threadId", "envelope", "sendAt", "undoStatus", "deliveryStatus", "dsnBlobIds", "mdnBlobIds"}

type smtpAddress struct {
	Email      string         `json:"email"`
	Parameters map[string]any `json:"parameters"`
}

type envelope struct {
	MailFrom smtpAddress   `json:"mailFrom"`
	RcptTo   []smtpAddress `json:"rcptTo"`
}

func submissionObject(x model.Submission) map[string]any {
	rcpt := []smtpAddress{}
	for _, r := range x.RcptTo {
		rcpt = append(rcpt, smtpAddress{Email: r})
	}
	// Relay acceptance is "queued", not delivery; displayed is never
	// known without MDNs (RFC 8621 §7).
	ds := map[string]any{}
	for r, st := range x.Recipients {
		ds[r] = map[string]any{"smtpReply": st.SMTPReply, "delivered": st.Delivered, "displayed": "unknown"}
	}
	return map[string]any{
		"id": x.ID, "identityId": x.IdentityID, "emailId": x.EmailID, "threadId": x.ThreadID,
		"envelope": envelope{MailFrom: smtpAddress{Email: x.MailFrom}, RcptTo: rcpt},
		"sendAt":   utcDate(x.SendAt), "undoStatus": x.UndoStatus(), "deliveryStatus": ds,
		"dsnBlobIds": []string{}, "mdnBlobIds": []string{},
	}
}

// visibleSubmissions reads the submissions JMAP shows, by id.
func (s *Server) visibleSubmissions(ctx context.Context) (map[string]model.Submission, State, *MethodError) {
	var out map[string]model.Submission
	st, e := s.consistent(ctx, func(State) error {
		all, err := s.Store.ListSubmissions(ctx)
		out = map[string]model.Submission{}
		for _, x := range all {
			if !x.Destroyed {
				out[x.ID] = x
			}
		}
		return err
	})
	return out, st, e
}

func submissionGet(ctx context.Context, c *call) (any, *MethodError) {
	var a getArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(a.Properties, submissionProperties)
	if e != nil {
		return nil, e
	}
	if a.IDs != nil && len(*a.IDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	all, st, e := c.s.visibleSubmissions(ctx)
	if e != nil {
		return nil, e
	}
	if a.IDs == nil && len(all) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "more submissions than maxObjectsInGet; query for ids")
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st, "EmailSubmission"), List: []map[string]any{}, NotFound: []string{}}
	ids := sortedSubmissions(all, nil)
	if a.IDs != nil {
		ids = nil
		for _, raw := range *a.IDs {
			id, _ := c.resolveID(raw)
			if _, ok := all[id]; !ok {
				if !slices.Contains(resp.NotFound, raw) {
					resp.NotFound = append(resp.NotFound, raw)
				}
			} else if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		resp.List = append(resp.List, pick(submissionObject(all[id]), props))
	}
	return resp, nil
}

func submissionChanges(ctx context.Context, c *call) (any, *MethodError) {
	resp, _, e := objectChanges(ctx, c, "EmailSubmission", func(ch Change) ([]string, []string, []string) {
		return ch.Created["EmailSubmission"], ch.Updated["EmailSubmission"], ch.Destroyed["EmailSubmission"]
	})
	if e != nil {
		return nil, e
	}
	return resp, nil
}

type submissionPredicate = func(model.Submission) bool

func parseSubmissionFilter(raw json.RawMessage, depth int) (submissionPredicate, *MethodError) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return func(model.Submission) bool { return true }, nil
	}
	if depth > 10 {
		return nil, methodErr("unsupportedFilter", "filter nesting too deep")
	}
	if p, ok, e := parseOperator(raw, depth, parseSubmissionFilter); ok || e != nil {
		return p, e
	}
	var f struct {
		IdentityIDs *[]string `json:"identityIds"`
		EmailIDs    *[]string `json:"emailIds"`
		ThreadIDs   *[]string `json:"threadIds"`
		UndoStatus  *string   `json:"undoStatus"`
		Before      *string   `json:"before"`
		After       *string   `json:"after"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, methodErr("unsupportedFilter", err.Error())
	}
	var before, after time.Time
	for _, d := range []struct {
		v   *string
		out *time.Time
	}{{f.Before, &before}, {f.After, &after}} {
		if d.v == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, *d.v)
		if err != nil || !strings.HasSuffix(*d.v, "Z") {
			return nil, methodErr("invalidArguments", "before and after are UTCDates")
		}
		*d.out = t
	}
	if f.UndoStatus != nil && !slices.Contains([]string{"pending", "final", "canceled"}, *f.UndoStatus) {
		return nil, methodErr("invalidArguments", "undoStatus is pending, final or canceled")
	}
	return func(x model.Submission) bool {
		switch {
		case f.IdentityIDs != nil && !slices.Contains(*f.IdentityIDs, x.IdentityID),
			f.EmailIDs != nil && !slices.Contains(*f.EmailIDs, x.EmailID),
			f.ThreadIDs != nil && !slices.Contains(*f.ThreadIDs, x.ThreadID),
			f.UndoStatus != nil && *f.UndoStatus != x.UndoStatus(),
			f.Before != nil && !x.SendAt.Before(before),
			f.After != nil && x.SendAt.Before(after):
			return false
		}
		return true
	}, nil
}

func submissionOrder(sorts []comparator) (func(a, b model.Submission) int, *MethodError) {
	var cmps []func(a, b model.Submission) int
	for _, s := range sorts {
		if s.Collation != "" && s.Collation != "i;unicode-casemap" {
			return nil, methodErr("unsupportedSort", "collation "+s.Collation)
		}
		var cmp func(a, b model.Submission) int
		switch s.Property {
		case "emailId":
			cmp = func(a, b model.Submission) int { return strings.Compare(a.EmailID, b.EmailID) }
		case "threadId":
			cmp = func(a, b model.Submission) int { return strings.Compare(a.ThreadID, b.ThreadID) }
		case "sentAt":
			cmp = func(a, b model.Submission) int { return a.SendAt.Compare(b.SendAt) }
		default:
			return nil, methodErr("unsupportedSort", s.Property)
		}
		if !s.ascending() {
			inner := cmp
			cmp = func(a, b model.Submission) int { return -inner(a, b) }
		}
		cmps = append(cmps, cmp)
	}
	return func(a, b model.Submission) int {
		for _, cmp := range cmps {
			if r := cmp(a, b); r != 0 {
				return r
			}
		}
		// Ties: send time, then id.
		if r := a.SendAt.Compare(b.SendAt); r != 0 {
			return r
		}
		return strings.Compare(a.ID, b.ID)
	}, nil
}

func sortedSubmissions(all map[string]model.Submission, order func(a, b model.Submission) int) []string {
	if order == nil {
		order, _ = submissionOrder(nil)
	}
	list := slices.Collect(maps.Values(all))
	slices.SortFunc(list, order)
	var ids []string
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	return ids
}

func submissionResults(all map[string]model.Submission, filter json.RawMessage, sorts []comparator) ([]string, *MethodError) {
	match, e := parseSubmissionFilter(filter, 0)
	if e != nil {
		return nil, e
	}
	order, e := submissionOrder(sorts)
	if e != nil {
		return nil, e
	}
	matched := map[string]model.Submission{}
	for id, x := range all {
		if match(x) {
			matched[id] = x
		}
	}
	return sortedSubmissions(matched, order), nil
}

func submissionQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a struct {
		AccountID      string          `json:"accountId"`
		Filter         json.RawMessage `json:"filter"`
		Sort           []comparator    `json:"sort"`
		Position       int64           `json:"position"`
		Anchor         *string         `json:"anchor"`
		AnchorOffset   int64           `json:"anchorOffset"`
		Limit          *int64          `json:"limit"`
		CalculateTotal bool            `json:"calculateTotal"`
	}
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	all, st, e := c.s.visibleSubmissions(ctx)
	if e != nil {
		return nil, e
	}
	ids, e := submissionResults(all, a.Filter, a.Sort)
	if e != nil {
		return nil, e
	}
	qs := queryState(st.Epoch, st.Submission, compactJSON(a.Filter), a.Sort)
	return page(ids, a.Position, a.Anchor, a.AnchorOffset, a.Limit, a.CalculateTotal, a.AccountID, qs, true)
}

func submissionQueryChanges(ctx context.Context, c *call) (any, *MethodError) {
	var a struct {
		AccountID       string          `json:"accountId"`
		Filter          json.RawMessage `json:"filter"`
		Sort            []comparator    `json:"sort"`
		SinceQueryState string          `json:"sinceQueryState"`
		MaxChanges      *int64          `json:"maxChanges"`
		UpToID          *string         `json:"upToId"`
		CalculateTotal  bool            `json:"calculateTotal"`
	}
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	all, st, e := c.s.visibleSubmissions(ctx)
	if e != nil {
		return nil, e
	}
	args := []any{compactJSON(a.Filter), a.Sort}
	since, ok := parseQueryState(a.SinceQueryState, st, args...)
	if !ok {
		return nil, methodErr("cannotCalculateChanges", "unknown query state, or different query arguments")
	}
	ids, e := submissionResults(all, a.Filter, a.Sort)
	if e != nil {
		return nil, e
	}
	changes, err := c.s.Store.Changes(ctx, since, st.Submission)
	if err != nil {
		return nil, c.s.fail("read changes", err)
	}
	// Every filter and sort input is a submission property, so changed
	// submissions are exactly the ones whose position may have moved.
	changed := map[string]bool{}
	for _, ch := range changes {
		for _, list := range [][]string{ch.Created["EmailSubmission"], ch.Updated["EmailSubmission"], ch.Destroyed["EmailSubmission"]} {
			for _, id := range list {
				changed[id] = true
			}
		}
	}
	return queryChangesResult(a.AccountID, a.SinceQueryState, queryState(st.Epoch, st.Submission, args...), ids, changed, a.MaxChanges, a.CalculateTotal)
}

type submissionSetArgs struct {
	AccountID             string                                `json:"accountId"`
	IfInState             *string                               `json:"ifInState"`
	Create                map[string]json.RawMessage            `json:"create"`
	Update                map[string]map[string]json.RawMessage `json:"update"`
	Destroy               []string                              `json:"destroy"`
	OnSuccessUpdateEmail  map[string]json.RawMessage            `json:"onSuccessUpdateEmail"`
	OnSuccessDestroyEmail []string                              `json:"onSuccessDestroyEmail"`
}

func submissionSet(ctx context.Context, c *call) (any, *MethodError) {
	var a submissionSetArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if len(a.Create)+len(a.Update)+len(a.Destroy) > MaxObjectsInSet {
		return nil, methodErr("requestTooLarge", "")
	}
	// Success hooks are Email/set changes: authorize them before any
	// submission is created (docs/specs/mail-flow.md#outbound-email).
	if (len(a.OnSuccessUpdateEmail) > 0 || len(a.OnSuccessDestroyEmail) > 0) && !c.acct.has(ScopeWrite) {
		return nil, methodErr("forbidden", "success hooks need "+ScopeWrite)
	}
	identities, start, e := c.s.identities(ctx)
	if e != nil {
		return nil, e
	}
	if a.IfInState != nil && !inState(*a.IfInState, start, "EmailSubmission") {
		return nil, methodErr("stateMismatch", "")
	}
	resp := newSetResponse(a.AccountID, stateToken(start, "EmailSubmission"))
	last := start
	advance := func(st State) {
		if st.Seq > last.Seq {
			last = st
		}
	}
	// succeeded maps hook keys (ids and "#creationId") to Email ids.
	succeeded := map[string]string{}
	for _, cid := range slices.Sorted(maps.Keys(a.Create)) {
		x, st, serr := c.s.createSubmission(ctx, c, identities, a.Create[cid])
		if serr != nil {
			resp.NotCreated[cid] = serr
			continue
		}
		advance(st)
		c.created[cid] = x.ID
		succeeded["#"+cid] = x.EmailID
		resp.Created[cid] = map[string]any{"id": x.ID, "threadId": x.ThreadID, "sendAt": utcDate(x.SendAt), "undoStatus": x.UndoStatus()}
	}
	for _, rawID := range slices.Sorted(maps.Keys(a.Update)) {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotUpdated[rawID] = &SetError{Type: "notFound"}
			continue
		}
		x, st, serr := c.s.updateSubmission(ctx, id, a.Update[rawID])
		if serr != nil {
			resp.NotUpdated[rawID] = serr
			continue
		}
		advance(st)
		succeeded[id] = x.EmailID
		resp.Updated[id] = nil
	}
	for _, rawID := range a.Destroy {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotDestroyed[rawID] = &SetError{Type: "notFound"}
			continue
		}
		x, st, serr := c.s.destroySubmission(ctx, id)
		if serr != nil {
			resp.NotDestroyed[rawID] = serr
			continue
		}
		advance(st)
		succeeded[id] = x.EmailID
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = stateToken(last, "EmailSubmission")
	c.successHooks(ctx, a, succeeded)
	return resp, nil
}

// successHooks makes the implicit Email/set call for the hooks of
// submissions that succeeded (RFC 8621 §7.5). Its failures are reported in
// its own response and never undo the submissions.
func (c *call) successHooks(ctx context.Context, a submissionSetArgs, succeeded map[string]string) {
	update := map[string]json.RawMessage{}
	for _, k := range slices.Sorted(maps.Keys(a.OnSuccessUpdateEmail)) {
		if emailID, ok := succeeded[k]; ok {
			update[emailID] = a.OnSuccessUpdateEmail[k]
		}
	}
	destroy := []string{}
	for _, k := range a.OnSuccessDestroyEmail {
		if emailID, ok := succeeded[k]; ok && !slices.Contains(destroy, emailID) {
			destroy = append(destroy, emailID)
		}
	}
	if len(update) == 0 && len(destroy) == 0 {
		return
	}
	args, _ := json.Marshal(map[string]any{"accountId": a.AccountID, "update": update, "destroy": destroy})
	result, merr := emailSet(ctx, &call{s: c.s, acct: c.acct, args: args, created: c.created})
	if merr != nil {
		c.followUps = append(c.followUps, followUp{name: "error", result: merr})
		return
	}
	c.followUps = append(c.followUps, followUp{name: "Email/set", result: result})
}

// createSubmission validates one creation and commits the submission, its
// quota reservation, its first attempt record and the dispatch outbox in
// one guarded transaction.
func (s *Server) createSubmission(ctx context.Context, c *call, identities map[string]model.Identity, raw json.RawMessage) (model.Submission, State, *SetError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return model.Submission{}, State{}, &SetError{Type: "invalidArguments", Description: "creation is not an object"}
	}
	var identityID, emailRef string
	var env *envelope
	for k, v := range obj {
		var err error
		switch k {
		case "identityId":
			err = json.Unmarshal(v, &identityID)
		case "emailId":
			err = json.Unmarshal(v, &emailRef)
		case "envelope":
			err = json.Unmarshal(v, &env)
		case "undoStatus":
			var u string
			if err = json.Unmarshal(v, &u); err == nil && u != "pending" {
				return model.Submission{}, State{}, invalidProps("a new submission is pending", k)
			}
		default:
			// sendAt included: maxDelayedSend is 0.
			return model.Submission{}, State{}, invalidProps("property is server-set or unknown", k)
		}
		if err != nil {
			return model.Submission{}, State{}, invalidProps("wrong type", k)
		}
	}
	ident, ok := identities[identityID]
	if !ok {
		return model.Submission{}, State{}, invalidProps("no such identity", "identityId")
	}
	emailID, ok := c.resolveID(emailRef)
	if !ok || emailRef == "" {
		return model.Submission{}, State{}, invalidProps("no such Email", "emailId")
	}
	email, found, serr := s.getEmail(ctx, emailID)
	if serr != nil {
		return model.Submission{}, State{}, serr
	}
	if !found {
		return model.Submission{}, State{}, invalidProps("no such Email", "emailId")
	}
	if email.Size > maxSubmissionSize {
		return model.Submission{}, State{}, &SetError{Type: "tooLarge", Description: "the message exceeds the relay size limit", MaxSize: maxSubmissionSize}
	}
	rawMIME, err := s.Originals.Fetch(ctx, email.Blob)
	if err != nil {
		s.Log.Error("jmap", "outcome", "serverFail", "step", "read submission message", "error", err)
		return model.Submission{}, State{}, &SetError{Type: "serverFail"}
	}
	hdr := mailparse.Parse(rawMIME).Headers
	if serr := checkSendHeaders(hdr, ident.Email); serr != nil {
		return model.Submission{}, State{}, serr
	}
	mailFrom, rcpts, serr := submissionEnvelope(env, hdr, ident.Email)
	if serr != nil {
		return model.Submission{}, State{}, serr
	}

	now := s.now()
	x := model.Submission{
		ID: model.NewID("S"), IdentityID: ident.ID, EmailID: email.ID, ThreadID: email.ThreadID, MailFrom: mailFrom, RcptTo: rcpts,
		SendAt: now, Blob: email.Blob, Size: int64(len(rawMIME)), Relay: s.Sending.Policy.Relay,
		Status: model.SubmissionQueued, NextAttemptAt: now,
	}
	x.SetRecipients("queued", "")
	var deltas []model.QuotaDelta
	x.Reservation, deltas = s.Sending.Policy.Reserve(now, len(rcpts))
	st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		return &model.Mutation{
			Submissions: []model.SubmissionChange{{After: &x}},
			Quota:       deltas,
			Attempts:    []model.Attempt{{SubmissionID: x.ID, Seq: 1, Phase: "created", At: now, Detail: fmt.Sprintf("%d recipients", len(rcpts))}},
			Send:        []string{x.ID},
		}, nil
	})
	return x, st, serr
}

// checkSendHeaders requires the message to come from the identity and to
// carry the headers relays and recipients need.
func checkSendHeaders(h mailparse.Headers, identity string) *SetError {
	for _, name := range []string{"From", "Sender"} {
		values := h.All(name)
		if len(values) == 0 && name == "Sender" {
			continue
		}
		if len(values) != 1 {
			return &SetError{Type: "forbiddenFrom", Description: "the message needs exactly one " + name + " header"}
		}
		addrs := mailparse.AsAddresses(values[0])
		if len(addrs) != 1 || !strings.EqualFold(addrs[0].Email, identity) {
			return &SetError{Type: "forbiddenFrom", Description: name + " must be the identity's address " + identity}
		}
	}
	if v, ok := h.Last("Message-ID"); !ok || len(mailparse.AsMessageIds(v)) != 1 {
		return &SetError{Type: "invalidEmail", Description: "the message needs one Message-ID", Properties: []string{"header:Message-ID"}}
	}
	if v, ok := h.Last("Date"); !ok || mailparse.AsDate(v) == nil {
		return &SetError{Type: "invalidEmail", Description: "the message needs a Date", Properties: []string{"header:Date"}}
	}
	return nil
}

// submissionEnvelope validates the client's envelope, or derives one from
// the To, Cc and Bcc headers (RFC 8621 §7).
func submissionEnvelope(env *envelope, h mailparse.Headers, identity string) (string, []string, *SetError) {
	var candidates []string
	mailFrom := identity
	if env != nil {
		if len(env.MailFrom.Parameters) > 0 {
			return "", nil, invalidProps("no SMTP extensions are supported", "envelope")
		}
		if !strings.EqualFold(env.MailFrom.Email, identity) {
			return "", nil, &SetError{Type: "forbiddenMailFrom", Description: "MAIL FROM must be the identity's address " + identity}
		}
		for _, r := range env.RcptTo {
			if len(r.Parameters) > 0 {
				return "", nil, invalidProps("no SMTP extensions are supported", "envelope")
			}
			candidates = append(candidates, r.Email)
		}
	} else {
		for _, name := range []string{"To", "Cc", "Bcc"} {
			for _, v := range h.All(name) {
				for _, a := range mailparse.AsAddresses(v) {
					candidates = append(candidates, a.Email)
				}
			}
		}
	}
	var rcpts, invalid []string
	seen := map[string]bool{}
	for _, r := range candidates {
		if !validAddress(r) {
			invalid = append(invalid, r)
			continue
		}
		if k := strings.ToLower(r); !seen[k] {
			seen[k] = true
			rcpts = append(rcpts, r)
		}
	}
	switch {
	case len(invalid) > 0:
		return "", nil, &SetError{Type: "invalidRecipients", InvalidRecipients: invalid}
	case len(rcpts) == 0:
		return "", nil, &SetError{Type: "noRecipients"}
	case len(rcpts) > maxRecipients:
		return "", nil, &SetError{Type: "tooManyRecipients", MaxRecipients: maxRecipients}
	}
	return mailFrom, rcpts, nil
}

// updateSubmission applies the one permitted change: undoStatus canceled.
func (s *Server) updateSubmission(ctx context.Context, id string, patch map[string]json.RawMessage) (model.Submission, State, *SetError) {
	for k, v := range patch {
		if k != "undoStatus" || strings.TrimSpace(string(v)) != `"canceled"` {
			return model.Submission{}, State{}, invalidProps("only undoStatus can change, to canceled", strings.SplitN(k, "/", 2)[0])
		}
	}
	var x model.Submission
	st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		cur, ok, err := s.Store.GetSubmission(ctx, id)
		if err != nil {
			return nil, &SetError{Type: "serverFail"}
		}
		if !ok || cur.Destroyed {
			return nil, &SetError{Type: "notFound"}
		}
		x = cur
		if len(patch) == 0 || cur.Status == model.SubmissionCanceled {
			return nil, nil
		}
		m, ok := send.Cancel(cur, s.now())
		if !ok {
			return nil, &SetError{Type: "cannotUnsend", Description: "the message may already have been handed to the relay"}
		}
		return m, nil
	})
	return x, st, serr
}

// destroySubmission hides the submission; it does not cancel it, and its
// send state and attempt history remain.
func (s *Server) destroySubmission(ctx context.Context, id string) (model.Submission, State, *SetError) {
	var x model.Submission
	st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		cur, ok, err := s.Store.GetSubmission(ctx, id)
		if err != nil {
			return nil, &SetError{Type: "serverFail"}
		}
		if !ok || cur.Destroyed {
			return nil, &SetError{Type: "notFound"}
		}
		x = cur
		after := cur.Clone()
		after.Destroyed = true
		return &model.Mutation{Submissions: []model.SubmissionChange{{Before: &cur, After: &after}}}, nil
	})
	return x, st, serr
}
