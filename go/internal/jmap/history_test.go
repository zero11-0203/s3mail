package jmap

import (
	"testing"
	"time"
)

// States whose change records may have been pruned need a full
// resynchronization (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
func TestHistoryFloor(t *testing.T) {
	s, m := fixture()
	old := stateOf(t, s, "Email")
	q := result(t, run(t, s, owner, `[["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"inbox"}},"0"]]`), 0, "Email/query")
	qs := q["queryState"].(string)
	m.add(EmailMeta{ID: "Mdddddddddddddddddddddddd", MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: t0.Add(3 * time.Hour)}, "r")
	current := stateOf(t, s, "Email")
	m.mu.Lock()
	m.st.Floor = m.st.Seq
	m.mu.Unlock()
	out := run(t, s, owner, `[
		["Email/changes",{"accountId":"$ACCT","sinceState":"`+old+`"},"0"],
		["Email/queryChanges",{"accountId":"$ACCT","filter":{"inMailbox":"inbox"},"sinceQueryState":"`+qs+`"},"1"],
		["Email/changes",{"accountId":"$ACCT","sinceState":"`+current+`"},"2"]]`)
	if errType(t, out, 0) != "cannotCalculateChanges" {
		t.Error("Email/changes from below the floor")
	}
	if errType(t, out, 1) != "cannotCalculateChanges" {
		t.Error("Email/queryChanges from below the floor")
	}
	if r := result(t, out, 2, "Email/changes"); r["newState"] != current {
		t.Errorf("changes from the floor %v", r)
	}
	// A type unchanged since below the floor needs no pruned records.
	if !retained(2, State{Seq: 9, Floor: 5, Identity: 2}, "Identity") || retained(2, State{Seq: 9, Floor: 5, Identity: 3}, "Identity") {
		t.Error("retained")
	}
}
