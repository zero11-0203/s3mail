package jmap

import (
	"slices"
	"testing"
	"time"
)

func TestMailboxSetTree(t *testing.T) {
	s, m := fixture()
	out := run(t, s, owner, `[
		["Mailbox/set",{"accountId":"$ACCT","create":{
			"child":{"name":"Receipts","parentId":"#top"},
			"top":{"name":"Finance","sortOrder":5},
			"dup":{"name":"Inbox"},
			"role":{"name":"R","role":"inbox"},
			"orphan":{"name":"O","parentId":"Bnone"}}},"0"],
		["Mailbox/set",{"accountId":"$ACCT","update":{
			"#top":{"parentId":"#child"},
			"inbox":{"name":"Posteingang","parentId":"#top"},
			"archive":{"name":"Old","isSubscribed":false}}},"1"],
		["Mailbox/query",{"accountId":"$ACCT","sortAsTree":true,"filterAsTree":true,"filter":{"isSubscribed":true}},"2"],
		["Mailbox/get",{"accountId":"$ACCT","ids":["#child"],"properties":["parentId","name","myRights"]},"3"]]`)
	r := result(t, out, 0, "Mailbox/set")
	created := r["created"].(map[string]any)
	if created["top"] == nil || created["child"] == nil {
		t.Fatalf("create %v", r)
	}
	for _, k := range []string{"dup", "role", "orphan"} {
		if r["notCreated"].(map[string]any)[k] == nil {
			t.Errorf("%s must fail: %v", k, r)
		}
	}
	top := created["top"].(map[string]any)["id"].(string)
	child := created["child"].(map[string]any)["id"].(string)
	if m.mailboxes[child].ParentID != top {
		t.Errorf("child parent %+v", m.mailboxes[child])
	}

	u := result(t, out, 1, "Mailbox/set")
	nu := u["notUpdated"].(map[string]any)
	if _, ok := u["updated"].(map[string]any)["archive"]; !ok || nu["#top"] == nil || nu["inbox"].(map[string]any)["type"] != "forbidden" {
		t.Errorf("updates %v", u)
	}
	ids := result(t, out, 2, "Mailbox/query")["ids"].([]any)
	if slices.Contains(ids, any("archive")) || !slices.Contains(ids, any(child)) {
		t.Errorf("tree query %v", ids)
	}
	if i, j := slices.Index(ids, any(top)), slices.Index(ids, any(child)); i < 0 || j != i+1 {
		t.Errorf("sortAsTree places children after parents: %v", ids)
	}
	got := result(t, out, 3, "Mailbox/get")["list"].([]any)[0].(map[string]any)
	if got["parentId"] != top || got["myRights"].(map[string]any)["mayDelete"] != true {
		t.Errorf("get %v", got)
	}

	// Destroy: children first, emails block unless removed.
	m.add(EmailMeta{ID: "Mdddddddddddddddddddddddd", MailboxIDs: map[string]bool{child: true}, ReceivedAt: t0}, "x")
	m.add(EmailMeta{ID: "Meeeeeeeeeeeeeeeeeeeeeeee", MailboxIDs: map[string]bool{child: true, "inbox": true}, ReceivedAt: t0}, "y")
	out = run(t, s, owner, `[
		["Mailbox/set",{"accountId":"$ACCT","destroy":["`+top+`","`+child+`"]},"0"],
		["Mailbox/set",{"accountId":"$ACCT","destroy":["`+child+`","`+top+`"],"onDestroyRemoveEmails":true},"1"]]`)
	nd := result(t, out, 0, "Mailbox/set")["notDestroyed"].(map[string]any)
	if nd[top].(map[string]any)["type"] != "mailboxHasChild" || nd[child].(map[string]any)["type"] != "mailboxHasEmail" {
		t.Errorf("guarded destroy %v", nd)
	}
	if d := result(t, out, 1, "Mailbox/set")["destroyed"].([]any); len(d) != 2 {
		t.Errorf("destroy with removal %v", d)
	}
	if _, ok := m.emails["Mdddddddddddddddddddddddd"]; ok {
		t.Error("an Email only in the destroyed mailbox is destroyed")
	}
	if e := m.emails["Meeeeeeeeeeeeeeeeeeeeeeee"]; len(e.MailboxIDs) != 1 || !e.MailboxIDs["inbox"] {
		t.Errorf("an Email in other mailboxes stays there: %+v", e)
	}
}

func TestEmailDestroyAndThreads(t *testing.T) {
	s, m := fixture()
	// A reply joins idA's thread through In-Reply-To.
	m.add(EmailMeta{ID: "Mdddddddddddddddddddddddd", ThreadID: "T" + idA[1:], MailboxIDs: map[string]bool{"inbox": true},
		ReceivedAt: t0.Add(3 * time.Hour), Keywords: map[string]bool{"$seen": true}}, "r")
	st := stateOf(t, s, "Email")
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT","collapseThreads":true,"filter":{"inMailbox":"inbox"}},"0"],
		["Email/query",{"accountId":"$ACCT","filter":{"someInThreadHaveKeyword":"$seen"}},"1"],
		["Mailbox/get",{"accountId":"$ACCT","ids":["inbox"]},"2"],
		["Email/set",{"accountId":"$ACCT","destroy":["Mdddddddddddddddddddddddd"]},"3"],
		["Email/queryChanges",{"accountId":"$ACCT","collapseThreads":true,"filter":{"inMailbox":"inbox"},"sinceQueryState":"#Q"},"4"],
		["Thread/get",{"accountId":"$ACCT","ids":["T`+idA[1:]+`"]},"5"],
		["Email/changes",{"accountId":"$ACCT","sinceState":"`+st+`"},"6"]]`)
	q := result(t, out, 0, "Email/query")
	if ids := q["ids"].([]any); len(ids) != 2 || ids[0] != "Mdddddddddddddddddddddddd" || ids[1] != idB {
		t.Errorf("collapsed %v", ids)
	}
	if ids := result(t, out, 1, "Email/query")["ids"].([]any); len(ids) != 3 {
		t.Errorf("thread keyword filter %v", ids)
	}
	inbox := result(t, out, 2, "Mailbox/get")["list"].([]any)[0].(map[string]any)
	if inbox["totalEmails"].(float64) != 3 || inbox["totalThreads"].(float64) != 2 || inbox["unreadThreads"].(float64) != 1 {
		t.Errorf("inbox counts %v", inbox)
	}
	if d := result(t, out, 3, "Email/set")["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroy %v", d)
	}
	// "#Q" is not a query state; re-run with the real one.
	if errType(t, out, 4) != "cannotCalculateChanges" {
		t.Error("unknown query state")
	}
	if ids := result(t, out, 5, "Thread/get")["list"].([]any)[0].(map[string]any)["emailIds"].([]any); len(ids) != 1 {
		t.Errorf("thread after destroy %v", ids)
	}
	if d := result(t, out, 6, "Email/changes")["destroyed"].([]any); len(d) != 1 {
		t.Errorf("changes %v", d)
	}
}

func TestQueryChanges(t *testing.T) {
	s, m := fixture()
	m.add(EmailMeta{ID: "Mdddddddddddddddddddddddd", ThreadID: "T" + idA[1:], MailboxIDs: map[string]bool{"inbox": true},
		ReceivedAt: t0.Add(3 * time.Hour)}, "r")
	q := result(t, run(t, s, owner, `[["Email/query",{"accountId":"$ACCT","collapseThreads":true,"filter":{"inMailbox":"inbox"}}, "0"]]`), 0, "Email/query")
	qs := q["queryState"].(string)
	out := run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","destroy":["Mdddddddddddddddddddddddd"],"update":{"`+idB+`":{"mailboxIds/inbox":null,"mailboxIds/archive":true}}},"0"],
		["Email/queryChanges",{"accountId":"$ACCT","collapseThreads":true,"filter":{"inMailbox":"inbox"},"sinceQueryState":"`+qs+`","calculateTotal":true},"1"],
		["Email/queryChanges",{"accountId":"$ACCT","filter":{"inMailbox":"inbox"},"sinceQueryState":"`+qs+`"},"2"],
		["Email/queryChanges",{"accountId":"$ACCT","collapseThreads":true,"filter":{"inMailbox":"inbox"},"sinceQueryState":"`+qs+`","maxChanges":1},"3"],
		["Mailbox/query",{"accountId":"$ACCT"},"4"]]`)
	qc := result(t, out, 1, "Email/queryChanges")
	removed := qc["removed"].([]any)
	added := qc["added"].([]any)
	// idA becomes the thread's representative again; idB left the inbox.
	if !slices.Contains(removed, any(idB)) || !slices.Contains(removed, any("Mdddddddddddddddddddddddd")) ||
		len(added) != 1 || added[0].(map[string]any)["id"] != idA || qc["total"].(float64) != 1 {
		t.Errorf("queryChanges %v", qc)
	}
	if errType(t, out, 2) != "cannotCalculateChanges" {
		t.Error("a state from different query arguments must fail")
	}
	if errType(t, out, 3) != "tooManyChanges" {
		t.Error("maxChanges")
	}
	mq := result(t, out, 4, "Mailbox/query")["queryState"].(string)
	out = run(t, s, owner, `[
		["Mailbox/set",{"accountId":"$ACCT","create":{"n":{"name":"New"}}},"0"],
		["Mailbox/queryChanges",{"accountId":"$ACCT","sinceQueryState":"`+mq+`"},"1"]]`)
	mc := result(t, out, 1, "Mailbox/queryChanges")
	if a := mc["added"].([]any); len(a) != 1 || a[0].(map[string]any)["index"].(float64) != 7 {
		t.Errorf("Mailbox/queryChanges %v", mc)
	}
}

func stateOf(t *testing.T, s *Server, typ string) string {
	t.Helper()
	st, err := s.Store.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stateToken(st, typ)
}
