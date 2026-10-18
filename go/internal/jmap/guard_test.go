package jmap

import (
	"context"
	"testing"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// racingStore lets another writer commit at a chosen read of a call.
type racingStore struct {
	*memStore
	states  int
	onState func(n int)        // before the nth State read returns
	onGet   func(ids []string) // before an Email read
}

func (r *racingStore) State(ctx context.Context) (State, error) {
	r.states++
	if r.onState != nil {
		r.onState(r.states)
	}
	return r.memStore.State(ctx)
}

func (r *racingStore) GetEmails(ctx context.Context, ids []string) (map[string]EmailMeta, error) {
	if r.onGet != nil {
		r.onGet(ids)
	}
	return r.memStore.GetEmails(ctx, ids)
}

// move commits another client's move of an Email.
func (m *memStore) move(id string, mailboxes map[string]bool) {
	m.mu.Lock()
	st, before := m.st, m.emails[id].Clone()
	m.mu.Unlock()
	after := before.Clone()
	after.MailboxIDs = mailboxes
	if _, err := m.Commit(context.Background(), st, model.Mutation{Emails: []model.EmailChange{{Before: &before, After: &after}}}); err != nil {
		panic(err)
	}
}

func racing() (*Server, *memStore, *racingStore) {
	s, m := fixture()
	r := &racingStore{memStore: m}
	s.Store = r
	return s, m, r
}

func archive(t *testing.T, s *Server, state string, ids ...string) []any {
	t.Helper()
	update := ""
	for i, id := range ids {
		if i > 0 {
			update += ","
		}
		update += `"` + id + `":{"mailboxIds/inbox":null,"mailboxIds/archive":true}`
	}
	return run(t, s, owner, `[["Email/set",{"accountId":"$ACCT","ifInState":"`+state+`","update":{`+update+`}},"0"]]`)
}

// A change landing after the call's ifInState check, but before its
// commit reads the state, fails the call instead of being overwritten.
func TestIfInStateHoldsUntilTheFirstCommit(t *testing.T) {
	s, m, r := racing()
	state := stateOf(t, s, "Email")
	r.states = 0
	r.onState = func(n int) {
		if n == 2 { // the commit's read, after the method's own
			m.move(idA, map[string]bool{"trash": true})
		}
	}
	if got := errType(t, archive(t, s, state, idA), 0); got != "stateMismatch" {
		t.Fatalf("got %s", got)
	}
	if mb := m.emails[idA].MailboxIDs; len(mb) != 1 || !mb["trash"] {
		t.Errorf("the other client's move was overwritten: %v", mb)
	}
}

// A change that makes the commit lose the sequence race is seen by its
// retry, which refuses instead of re-applying the patch.
func TestIfInStateHoldsAcrossCommitRetries(t *testing.T) {
	s, m, r := racing()
	state := stateOf(t, s, "Email")
	once := true
	r.onGet = func(ids []string) {
		if once && ids[0] == idA {
			once = false
			m.move(idA, map[string]bool{"trash": true})
		}
	}
	if got := errType(t, archive(t, s, state, idA), 0); got != "stateMismatch" {
		t.Fatalf("got %s", got)
	}
	if mb := m.emails[idA].MailboxIDs; len(mb) != 1 || !mb["trash"] {
		t.Errorf("the retry applied the patch over the move: %v", mb)
	}
}

// Once an object is applied, a later change refuses the remaining
// objects one by one; the applied ones and the new state are reported.
func TestIfInStateRefusesLaterObjects(t *testing.T) {
	s, m, r := racing()
	state := stateOf(t, s, "Email")
	once := true
	r.onGet = func(ids []string) {
		if once && ids[0] == idB {
			once = false
			m.move(idB, map[string]bool{"trash": true})
		}
	}
	res := result(t, archive(t, s, state, idA, idB), 0, "Email/set")
	if _, ok := res["updated"].(map[string]any)[idA]; !ok {
		t.Fatalf("set %v", res)
	}
	if e := res["notUpdated"].(map[string]any)[idB].(map[string]any); e["type"] != "stateMismatch" {
		t.Errorf("notUpdated %v", e)
	}
	if res["newState"] != "1-4" || res["oldState"] != state {
		t.Errorf("states %v -> %v", res["oldState"], res["newState"])
	}
	if a, b := m.emails[idA].MailboxIDs, m.emails[idB].MailboxIDs; !a["archive"] || len(b) != 1 || !b["trash"] {
		t.Errorf("stored %v %v", a, b)
	}
}

// Changes to other types still retry: ifInState names the Email state.
func TestIfInStateIgnoresOtherTypes(t *testing.T) {
	s, m, r := racing()
	state := stateOf(t, s, "Email")
	once := true
	r.onGet = func([]string) {
		if once {
			once = false
			st, _ := m.State(context.Background())
			box := Mailbox{ID: "Bother", Name: "Other", IsSubscribed: true}
			if _, err := m.Commit(context.Background(), st, model.Mutation{Mailboxes: []model.MailboxChange{{After: &box}}}); err != nil {
				panic(err)
			}
		}
	}
	res := result(t, archive(t, s, state, idA), 0, "Email/set")
	if _, ok := res["updated"].(map[string]any)[idA]; !ok || !m.emails[idA].MailboxIDs["archive"] {
		t.Errorf("set %v", res)
	}
}

// The other /set methods share the guard: here Mailbox/set.
func TestMailboxIfInStateHoldsUntilTheFirstCommit(t *testing.T) {
	s, m, r := racing()
	state := stateOf(t, s, "Mailbox")
	r.states = 0
	r.onState = func(n int) {
		if n == 2 {
			st, _ := m.State(context.Background())
			box := Mailbox{ID: "Bother", Name: "Other", IsSubscribed: true}
			if _, err := m.Commit(context.Background(), st, model.Mutation{Mailboxes: []model.MailboxChange{{After: &box}}}); err != nil {
				panic(err)
			}
		}
	}
	out := run(t, s, owner, `[["Mailbox/set",{"accountId":"$ACCT","ifInState":"`+state+`","create":{"k":{"name":"Mine"}}},"0"]]`)
	if got := errType(t, out, 0); got != "stateMismatch" {
		t.Fatalf("got %s", got)
	}
	if len(m.mailboxes) != 1 {
		t.Errorf("mailboxes %v", m.mailboxes)
	}
}
