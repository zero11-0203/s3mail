package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
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
	if a.IfInState != nil && *a.IfInState != stateToken(start) {
		return nil, methodErr("stateMismatch", "")
	}
	resp := newSetResponse(a.AccountID, start)
	for id := range a.Create {
		resp.NotCreated[id] = &SetError{Type: "forbidden", Description: "Email creation is not supported yet"}
	}
	for _, id := range a.Destroy {
		// Destruction needs deletion markers and derived-data cleanup
		// (docs/operations/recovery.md); until then, move to Trash.
		resp.NotDestroyed[id] = &SetError{Type: "forbidden", Description: "Email destruction is not supported yet; move it to Trash"}
	}
	last := start
	for id, patch := range a.Update {
		st, serr := c.s.updateEmail(ctx, id, patch)
		if serr != nil {
			resp.NotUpdated[id] = serr
			continue
		}
		resp.Updated[id] = nil
		if st.Seq > last.Seq {
			last = st
		}
	}
	resp.NewState = stateToken(last)
	return resp, nil
}

// updateEmail applies one patch under the account sequence guard, retrying
// from a fresh read when another commit wins.
func (s *Server) updateEmail(ctx context.Context, id string, patch map[string]json.RawMessage) (State, *SetError) {
	for range maxSetAttempts {
		st, err := s.Store.State(ctx)
		if err != nil {
			return State{}, &SetError{Type: "serverFail"}
		}
		found, err := s.Store.GetEmails(ctx, uniqueValid([]string{id}))
		if err != nil {
			return State{}, &SetError{Type: "serverFail"}
		}
		before, ok := found[id]
		if !ok {
			return State{}, &SetError{Type: "notFound"}
		}
		after, serr := applyPatch(before, patch)
		if serr != nil {
			return State{}, serr
		}
		if maps.Equal(after.Keywords, before.Keywords) && maps.Equal(after.MailboxIDs, before.MailboxIDs) {
			return st, nil
		}
		err = s.Store.UpdateEmail(ctx, st, before, after)
		if errors.Is(err, ErrStateChanged) {
			continue
		}
		if err != nil {
			s.Log.Error("jmap", "outcome", "serverFail", "step", "update email", "error", err)
			return State{}, &SetError{Type: "serverFail"}
		}
		return State{Seq: st.Seq + 1, Epoch: st.Epoch}, nil
	}
	return State{}, &SetError{Type: "serverFail", Description: "account busy; retry"}
}

// applyPatch applies a PatchObject to the mutable Email properties:
// keywords and mailboxIds (RFC 8620 §5.3, RFC 8621 §4.6).
func applyPatch(m EmailMeta, patch map[string]json.RawMessage) (EmailMeta, *SetError) {
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
				out.MailboxIDs[id] = true
			}
		case root == "mailboxIds" && strings.Contains(key, "/"):
			return m, &SetError{Type: "invalidPatch", Description: "nested mailbox path"}
		case root == "mailboxIds":
			set, ok := patchFlag(raw)
			if !ok {
				return m, invalid(path, "mailboxIds values are true or null")
			}
			if set {
				out.MailboxIDs[key] = true
			} else {
				delete(out.MailboxIDs, key)
			}
		default:
			return m, invalid(root, "property is immutable or unknown")
		}
	}
	for id := range out.MailboxIDs {
		if !mailboxExists(id) {
			return m, invalid("mailboxIds", "no mailbox "+id)
		}
	}
	if len(out.MailboxIDs) == 0 {
		return m, invalid("mailboxIds", "an Email must stay in at least one mailbox")
	}
	if len(out.MailboxIDs) > MaxMailboxesPerEmail {
		return m, &SetError{Type: "tooManyMailboxes"}
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
