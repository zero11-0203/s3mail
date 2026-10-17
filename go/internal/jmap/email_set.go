package jmap

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

const maxSetAttempts = 5

func emailSet(ctx context.Context, c *call) (any, *MethodError) {
	var a setArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if e := checkSetSize(a); e != nil {
		return nil, e
	}
	start, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	if a.IfInState != nil && !inState(*a.IfInState, start, "Email") {
		return nil, methodErr("stateMismatch", "")
	}
	resp := newSetResponse(a.AccountID, stateToken(start, "Email"))
	last := start
	advance := func(st State) {
		if st.Seq > last.Seq {
			last = st
		}
	}
	for _, cid := range slices.Sorted(maps.Keys(a.Create)) {
		created, st, serr := c.s.createEmail(ctx, c, a.Create[cid])
		if serr != nil {
			resp.NotCreated[cid] = serr
			continue
		}
		advance(st)
		c.created[cid] = created["id"].(string)
		resp.Created[cid] = created
	}
	for _, rawID := range slices.Sorted(maps.Keys(a.Update)) {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotUpdated[rawID] = &SetError{Type: "notFound"}
			continue
		}
		st, serr := c.s.updateEmail(ctx, c, id, a.Update[rawID])
		if serr != nil {
			resp.NotUpdated[rawID] = serr
			continue
		}
		advance(st)
		resp.Updated[id] = nil
	}
	for _, rawID := range a.Destroy {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotDestroyed[rawID] = &SetError{Type: "notFound"}
			continue
		}
		st, serr := c.s.destroyEmail(ctx, id)
		if serr != nil {
			resp.NotDestroyed[rawID] = serr
			continue
		}
		advance(st)
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = stateToken(last, "Email")
	return resp, nil
}

func (s *Server) getEmail(ctx context.Context, id string) (EmailMeta, bool, *SetError) {
	found, err := s.Store.GetEmails(ctx, uniqueValid([]string{id}))
	if err != nil {
		return EmailMeta{}, false, &SetError{Type: "serverFail"}
	}
	m, ok := found[id]
	return m, ok, nil
}

// updateEmail applies one patch under the account sequence guard.
func (s *Server) updateEmail(ctx context.Context, c *call, id string, patch map[string]json.RawMessage) (State, *SetError) {
	return s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		before, ok, serr := s.getEmail(ctx, id)
		if serr != nil {
			return nil, serr
		}
		if !ok {
			return nil, &SetError{Type: "notFound"}
		}
		after, serr := applyPatch(c, before, patch)
		if serr != nil {
			return nil, serr
		}
		if maps.Equal(after.Keywords, before.Keywords) && maps.Equal(after.MailboxIDs, before.MailboxIDs) {
			return nil, nil
		}
		if serr := s.checkMailboxIDs(ctx, after.MailboxIDs); serr != nil {
			return nil, serr
		}
		return &model.Mutation{Emails: []model.EmailChange{{Before: &before, After: &after}}}, nil
	})
}

// destroyEmail removes the Email and its derived records and writes a
// deletion marker; the original object is never deleted
// (docs/operations/recovery.md#original-mail-retention).
func (s *Server) destroyEmail(ctx context.Context, id string) (State, *SetError) {
	return s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		before, ok, serr := s.getEmail(ctx, id)
		if serr != nil {
			return nil, serr
		}
		if !ok {
			return nil, &SetError{Type: "notFound"}
		}
		return &model.Mutation{Emails: []model.EmailChange{{Before: &before}}}, nil
	})
}

func (s *Server) checkMailboxIDs(ctx context.Context, ids map[string]bool) *SetError {
	if len(ids) == 0 {
		return invalidProps("an Email must be in at least one mailbox", "mailboxIds")
	}
	if len(ids) > MaxMailboxesPerEmail {
		return &SetError{Type: "tooManyMailboxes"}
	}
	for id := range ids {
		ok, err := s.mailboxExists(ctx, id)
		if err != nil {
			return &SetError{Type: "serverFail"}
		}
		if !ok {
			return invalidProps("no mailbox "+id, "mailboxIds")
		}
	}
	return nil
}

// applyPatch applies a PatchObject to the mutable Email properties:
// keywords and mailboxIds (RFC 8620 §5.3, RFC 8621 §4.6).
func applyPatch(c *call, m EmailMeta, patch map[string]json.RawMessage) (EmailMeta, *SetError) {
	out := m
	out.Keywords = maps.Clone(m.Keywords)
	out.MailboxIDs = maps.Clone(m.MailboxIDs)
	if out.Keywords == nil {
		out.Keywords = map[string]bool{}
	}
	if out.MailboxIDs == nil {
		out.MailboxIDs = map[string]bool{}
	}
	invalid := func(prop, desc string) *SetError {
		return &SetError{Type: "invalidProperties", Description: desc, Properties: []string{prop}}
	}
	whole := map[string]bool{}
	for path := range patch {
		if path == "keywords" || path == "mailboxIds" {
			whole[path] = true
		}
	}
	for path, raw := range patch {
		root, key, sub := strings.Cut(path, "/")
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		if sub && whole[root] {
			return m, &SetError{Type: "invalidPatch", Description: path + " conflicts with " + root}
		}
		switch {
		case root == "keywords" && !sub:
			var kw map[string]bool
			if err := json.Unmarshal(raw, &kw); err != nil {
				return m, invalid(path, "keywords must be an object of true values")
			}
			out.Keywords = map[string]bool{}
			for k, v := range kw {
				if !v || !validKeyword(k) {
					return m, invalid(path, "invalid keyword "+k)
				}
				out.Keywords[strings.ToLower(k)] = true
			}
		case root == "keywords" && strings.Contains(key, "/"):
			return m, &SetError{Type: "invalidPatch", Description: "nested keyword path"}
		case root == "keywords":
			if !validKeyword(key) {
				return m, invalid(path, "invalid keyword "+key)
			}
			set, ok := patchFlag(raw)
			if !ok {
				return m, invalid(path, "keyword values are true or null")
			}
			if set {
				out.Keywords[strings.ToLower(key)] = true
			} else {
				delete(out.Keywords, strings.ToLower(key))
			}
		case root == "mailboxIds" && !sub:
			var ids map[string]bool
			if err := json.Unmarshal(raw, &ids); err != nil {
				return m, invalid(path, "mailboxIds must be an object of true values")
			}
			out.MailboxIDs = map[string]bool{}
			for id, v := range ids {
				if !v {
					return m, invalid(path, "mailboxIds values must be true")
				}
				out.MailboxIDs[resolveMailbox(c, id)] = true
			}
		case root == "mailboxIds" && strings.Contains(key, "/"):
			return m, &SetError{Type: "invalidPatch", Description: "nested mailbox path"}
		case root == "mailboxIds":
			set, ok := patchFlag(raw)
			if !ok {
				return m, invalid(path, "mailboxIds values are true or null")
			}
			if set {
				out.MailboxIDs[resolveMailbox(c, key)] = true
			} else {
				delete(out.MailboxIDs, resolveMailbox(c, key))
			}
		default:
			return m, invalid(root, "property is immutable or unknown")
		}
	}
	return out, nil
}

func patchFlag(raw json.RawMessage) (set, ok bool) {
	switch strings.TrimSpace(string(raw)) {
	case "true":
		return true, true
	case "null":
		return false, true
	}
	return false, false
}

// validKeyword follows the IMAP flag-keyword syntax RFC 8621 §4.1.1 uses.
func validKeyword(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7e || strings.ContainsRune(`(){]%*"\`, r) {
			return false
		}
	}
	return true
}

// resolveMailbox maps a "#creationId" to the created Mailbox id; unknown
// references stay as given and fail the existence check.
func resolveMailbox(c *call, id string) string {
	if r, ok := c.resolveID(id); ok {
		return r
	}
	return id
}
