package jmap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/send"
)

func sendingFixture() (*Server, *memStore) {
	s, m := fixture()
	s.Sending = &Sending{Domain: "example.com", Aliases: []string{"me", "alias"}, Policy: send.Policy{Relay: "smtp2go", Daily: 3, Monthly: 10, PerRecipient: true}}
	s.Now = func() time.Time { return t0 }
	return s, m
}

// runSubmit executes a request using the submission capability.
func runSubmit(t *testing.T, s *Server, acct Account, calls string) []any {
	t.Helper()
	calls = strings.ReplaceAll(calls, "$ACCT", acctID())
	var req Request
	if err := json.Unmarshal([]byte(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":`+calls+`}`), &req); err != nil {
		t.Fatal(err)
	}
	resp := s.Execute(context.Background(), acct, &req)
	b, _ := json.Marshal(resp.MethodResponses)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
}

func TestSubmissionNotAdvertisedWithoutSending(t *testing.T) {
	s, _ := fixture()
	sess, _ := s.Session(context.Background(), owner)
	if _, ok := sess.Capabilities[CapSubmission]; ok {
		t.Error("submission advertised without sending")
	}
	var req Request
	_ = json.Unmarshal([]byte(`{"using":["urn:ietf:params:jmap:submission"],"methodCalls":[]}`), &req)
	if p := s.validate(&req); p == nil || !strings.HasSuffix(p.Type, "unknownCapability") {
		t.Errorf("validate %+v", p)
	}
	s, _ = sendingFixture()
	sess, _ = s.Session(context.Background(), owner)
	ac := sess.Accounts[acctID()].AccountCapabilities[CapSubmission].(map[string]any)
	if ac["maxDelayedSend"] != 0 || sess.PrimaryAccounts[CapSubmission] != acctID() {
		t.Errorf("submission capability %v", ac)
	}
}

func TestIdentities(t *testing.T) {
	s, m := sendingFixture()
	out := runSubmit(t, s, owner, `[["Identity/get",{"accountId":"$ACCT"},"0"]]`)
	r := result(t, out, 0, "Identity/get")
	list := r["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("identities %v", r)
	}
	me := model.IdentityID("me@example.com")
	state := r["state"].(string)
	out = runSubmit(t, s, owner, `[
		["Identity/set",{"accountId":"$ACCT","ifInState":"`+state+`","create":{"x":{"email":"other@example.com"}},
			"update":{"`+me+`":{"name":"Me Myself","replyTo":[{"name":"","email":"r@example.com"}]},"`+model.IdentityID("alias@example.com")+`":{"email":"x@example.com"}},
			"destroy":["`+me+`"]},"0"],
		["Identity/changes",{"accountId":"$ACCT","sinceState":"`+state+`"},"1"],
		["Identity/get",{"accountId":"$ACCT","ids":["`+me+`"]},"2"]]`)
	r = result(t, out, 0, "Identity/set")
	if r["notCreated"].(map[string]any)["x"].(map[string]any)["type"] != "forbidden" ||
		r["notDestroyed"].(map[string]any)[me].(map[string]any)["type"] != "forbidden" ||
		len(r["notUpdated"].(map[string]any)) != 1 || r["updated"].(map[string]any)[me] != nil {
		t.Errorf("set %v", r)
	}
	if ch := result(t, out, 1, "Identity/changes"); len(ch["updated"].([]any)) != 1 {
		t.Errorf("changes %v", ch)
	}
	g := result(t, out, 2, "Identity/get")["list"].([]any)[0].(map[string]any)
	if g["name"] != "Me Myself" || g["mayDelete"] != false || g["email"] != "me@example.com" {
		t.Errorf("get %v", g)
	}
	// A removed alias removes its identity.
	s.Sending.Aliases = []string{"me"}
	r = result(t, runSubmit(t, s, owner, `[["Identity/get",{"accountId":"$ACCT"},"0"]]`), 0, "Identity/get")
	if len(r["list"].([]any)) != 1 || len(m.idents) != 1 {
		t.Errorf("after alias removal %v", r)
	}
}

const draft = "From: Me <me@example.com>\r\nTo: Bob <bob@example.net>, carol@example.net\r\nCc: bob@example.net\r\nBcc: secret@example.net\r\n" +
	"Subject: hello\r\nDate: Sat, 17 Oct 2026 15:00:00 +0900\r\nMessage-ID: <d@example.com>\r\n\r\nhi\r\n"

const idD = "Mddddddddddddddddddddddddd"

func addDraft(m *memStore, id, raw string) {
	m.add(EmailMeta{ID: id, MailboxIDs: map[string]bool{"drafts": true}, Keywords: map[string]bool{"$draft": true, "$seen": true}, ReceivedAt: t0, Subject: "hello"}, raw)
}

// TestAercSendFlow follows aerc: an Email in Drafts, then one request with
// the submission and its onSuccessUpdateEmail hook.
func TestAercSendFlow(t *testing.T) {
	s, m := sendingFixture()
	addDraft(m, idD, draft)
	ident := model.IdentityID("me@example.com")
	out := runSubmit(t, s, owner, `[
		["EmailSubmission/set",{"accountId":"$ACCT","create":{"sub":{"identityId":"`+ident+`","emailId":"`+idD+`",
			"envelope":{"mailFrom":{"email":"me@example.com","parameters":null},"rcptTo":[{"email":"bob@example.net","parameters":null},{"email":"secret@example.net"}]}}},
			"onSuccessUpdateEmail":{"#sub":{"keywords/$draft":null,"mailboxIds/sent":true,"mailboxIds/drafts":null}}},"0"],
		["EmailSubmission/get",{"accountId":"$ACCT","ids":["#sub"]},"1"]]`)
	if len(out) != 3 {
		t.Fatalf("responses %v", out)
	}
	r := result(t, out, 0, "EmailSubmission/set")
	created := r["created"].(map[string]any)["sub"].(map[string]any)
	id := created["id"].(string)
	if created["undoStatus"] != "pending" || created["sendAt"] != "2026-10-17T06:00:00Z" {
		t.Errorf("created %v", created)
	}
	hook := result(t, out, 1, "Email/set")
	if _, ok := hook["updated"].(map[string]any)[idD]; !ok {
		t.Errorf("implicit Email/set %v", hook)
	}
	if out[1].([]any)[2] != "0" {
		t.Error("implicit response has another call id")
	}
	g := result(t, out, 2, "EmailSubmission/get")["list"].([]any)[0].(map[string]any)
	ds := g["deliveryStatus"].(map[string]any)
	if g["emailId"] != idD || g["threadId"] != m.emails[idD].ThreadID || len(ds) != 2 || ds["bob@example.net"].(map[string]any)["delivered"] != "queued" {
		t.Errorf("get %v", g)
	}
	e := m.emails[idD]
	if !e.MailboxIDs["sent"] || e.MailboxIDs["drafts"] || e.Keywords["$draft"] {
		t.Errorf("hook result %+v", e)
	}
	x := m.subs[id]
	if x.Blob != e.Blob || x.Status != model.SubmissionQueued || len(m.sends) != 1 || m.sends[0] != id || m.quota["smtp2go#day#2026-10-17"] != 2 {
		t.Errorf("stored %+v sends=%v quota=%v", x, m.sends, m.quota)
	}

	// Cancel, then hide it.
	out = runSubmit(t, s, owner, `[
		["EmailSubmission/set",{"accountId":"$ACCT","update":{"`+id+`":{"undoStatus":"canceled"}}},"0"],
		["EmailSubmission/query",{"accountId":"$ACCT","filter":{"undoStatus":"canceled"}},"1"],
		["EmailSubmission/set",{"accountId":"$ACCT","destroy":["`+id+`"]},"2"],
		["EmailSubmission/query",{"accountId":"$ACCT"},"3"]]`)
	if r := result(t, out, 0, "EmailSubmission/set"); len(r["updated"].(map[string]any)) != 1 {
		t.Errorf("cancel %v", r)
	}
	if q := result(t, out, 1, "EmailSubmission/query"); len(q["ids"].([]any)) != 1 {
		t.Errorf("query canceled %v", q)
	}
	if q := result(t, out, 3, "EmailSubmission/query"); len(q["ids"].([]any)) != 0 {
		t.Errorf("destroyed submission listed %v", q)
	}
	if m.quota["smtp2go#day#2026-10-17"] != 0 || m.subs[id].Status != model.SubmissionCanceled {
		t.Errorf("cancel released nothing %v %+v", m.quota, m.subs[id])
	}
}

func TestSubmissionCreateWithComposedEmail(t *testing.T) {
	s, m := sendingFixture()
	ident := model.IdentityID("me@example.com")
	// Envelope derived from To/Cc/Bcc, Email created in the same request.
	out := runSubmit(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","create":{"d":{"mailboxIds":{"drafts":true},"keywords":{"$draft":true},
			"from":[{"email":"me@example.com"}],"to":[{"email":"bob@example.net"}],"bcc":[{"email":"BOB@example.net"},{"email":"x@example.net"}],
			"subject":"s","bodyValues":{"t":{"value":"hi"}},"textBody":[{"partId":"t","type":"text/plain"}]}}},"0"],
		["EmailSubmission/set",{"accountId":"$ACCT","create":{"s":{"identityId":"`+ident+`","emailId":"#d"}},
			"onSuccessDestroyEmail":["#s"]},"1"]]`)
	r := result(t, out, 1, "EmailSubmission/set")
	id := r["created"].(map[string]any)["s"].(map[string]any)["id"].(string)
	if got := strings.Join(m.subs[id].RcptTo, ","); got != "bob@example.net,x@example.net" {
		t.Errorf("derived envelope %s", got)
	}
	if d := result(t, out, 2, "Email/set"); len(d["destroyed"].([]any)) != 1 {
		t.Errorf("destroy hook %v", d)
	}
	// The send snapshot survives the Email's destruction.
	if _, err := m.Fetch(context.Background(), m.subs[id].Blob); err != nil || len(m.emails) != 3 {
		t.Errorf("frozen copy %v", err)
	}
}

func TestSubmissionCreateErrors(t *testing.T) {
	s, m := sendingFixture()
	addDraft(m, idD, draft)
	addDraft(m, "Meeeeeeeeeeeeeeeeeeeeeeeee", strings.Replace(draft, "From: Me <me@example.com>", "From: other@example.org", 1))
	addDraft(m, "Mfffffffffffffffffffffffff", strings.Replace(draft, "Message-ID: <d@example.com>\r\n", "", 1))
	addDraft(m, "Mggggggggggggggggggggggggg", "From: me@example.com\r\nDate: Sat, 17 Oct 2026 15:00:00 +0900\r\nMessage-ID: <g@example.com>\r\n\r\nx\r\n")
	ident := model.IdentityID("me@example.com")
	sub := func(email, extra string) string {
		return `{"identityId":"` + ident + `","emailId":"` + email + `"` + extra + `}`
	}
	out := runSubmit(t, s, owner, `[["EmailSubmission/set",{"accountId":"$ACCT","create":{
		"noIdent":{"identityId":"Inope","emailId":"`+idD+`"},
		"noEmail":`+sub("Mzzzzzzzzzzzzzzzzzzzzzzzz", "")+`,
		"from":`+sub("Meeeeeeeeeeeeeeeeeeeeeeeee", "")+`,
		"msgid":`+sub("Mfffffffffffffffffffffffff", "")+`,
		"norcpt":`+sub("Mggggggggggggggggggggggggg", "")+`,
		"mailFrom":`+sub(idD, `,"envelope":{"mailFrom":{"email":"else@example.com"},"rcptTo":[{"email":"a@example.net"}]}`)+`,
		"badRcpt":`+sub(idD, `,"envelope":{"mailFrom":{"email":"me@example.com"},"rcptTo":[{"email":"not an address"}]}`)+`,
		"params":`+sub(idD, `,"envelope":{"mailFrom":{"email":"me@example.com","parameters":{"HOLDFOR":"60"}},"rcptTo":[{"email":"a@example.net"}]}`)+`,
		"sendAt":`+sub(idD, `,"sendAt":"2026-10-18T00:00:00Z"`)+`,
		"quota":`+sub(idD, `,"envelope":{"mailFrom":{"email":"me@example.com"},"rcptTo":[{"email":"a@example.net"},{"email":"b@example.net"},{"email":"c@example.net"},{"email":"d@example.net"}]}`)+`
	}}, "0"]]`)
	nc := result(t, out, 0, "EmailSubmission/set")["notCreated"].(map[string]any)
	want := map[string]string{
		"noIdent": "invalidProperties", "noEmail": "invalidProperties", "from": "forbiddenFrom", "msgid": "invalidEmail",
		"norcpt": "noRecipients", "mailFrom": "forbiddenMailFrom", "badRcpt": "invalidRecipients", "params": "invalidProperties",
		"sendAt": "invalidProperties", "quota": "overQuota",
	}
	for k, typ := range want {
		if e, ok := nc[k].(map[string]any); !ok || e["type"] != typ {
			t.Errorf("%s: %v, want %s", k, nc[k], typ)
		}
	}
	if len(m.subs) != 0 || len(m.sends) != 0 {
		t.Errorf("failed creations stored %v", m.subs)
	}
	// Hooks need the write scope; submission needs the submit scope.
	submitOnly := Account{InternalID: "primary", Scopes: []string{ScopeRead, ScopeSubmit}}
	out = runSubmit(t, s, submitOnly, `[["EmailSubmission/set",{"accountId":"$ACCT","create":{"s":`+sub(idD, "")+`},"onSuccessDestroyEmail":["#s"]},"0"]]`)
	if errType(t, out, 0) != "forbidden" || len(m.subs) != 0 {
		t.Error("hook without write scope")
	}
	readOnly := Account{InternalID: "primary", Scopes: []string{ScopeRead, ScopeWrite}}
	out = runSubmit(t, s, readOnly, `[["EmailSubmission/set",{"accountId":"$ACCT","create":{"s":`+sub(idD, "")+`}},"0"]]`)
	if errType(t, out, 0) != "forbidden" {
		t.Error("submission without submit scope")
	}
}

func TestSubmissionCancelAfterHandoffAndQueryChanges(t *testing.T) {
	s, m := sendingFixture()
	addDraft(m, idD, draft)
	ident := model.IdentityID("me@example.com")
	out := runSubmit(t, s, owner, `[
		["EmailSubmission/set",{"accountId":"$ACCT","create":{"s":{"identityId":"`+ident+`","emailId":"`+idD+`","envelope":{"mailFrom":{"email":"me@example.com"},"rcptTo":[{"email":"a@example.net"}]}}}},"0"],
		["EmailSubmission/query",{"accountId":"$ACCT","filter":{"undoStatus":"pending"},"sort":[{"property":"sentAt","isAscending":false}]},"1"]]`)
	id := result(t, out, 0, "EmailSubmission/set")["created"].(map[string]any)["s"].(map[string]any)["id"].(string)
	qs := result(t, out, 1, "EmailSubmission/query")["queryState"].(string)
	// A worker claims it and starts the handoff.
	ctx := context.Background()
	claim, _ := send.Claim(m.subs[id], "w", t0)
	st, _ := m.State(ctx)
	st, _ = m.Commit(ctx, st, *claim)
	h, _ := send.Handoff(m.subs[id], "w", 1, t0)
	_, _ = m.Commit(ctx, st, *h)
	out = runSubmit(t, s, owner, `[
		["EmailSubmission/set",{"accountId":"$ACCT","update":{"`+id+`":{"undoStatus":"canceled"}}},"0"],
		["EmailSubmission/queryChanges",{"accountId":"$ACCT","filter":{"undoStatus":"pending"},"sort":[{"property":"sentAt","isAscending":false}],"sinceQueryState":"`+qs+`"},"1"]]`)
	if e := result(t, out, 0, "EmailSubmission/set")["notUpdated"].(map[string]any)[id].(map[string]any); e["type"] != "cannotUnsend" {
		t.Errorf("cancel after handoff %v", e)
	}
	qc := result(t, out, 1, "EmailSubmission/queryChanges")
	if len(qc["removed"].([]any)) != 1 || len(qc["added"].([]any)) != 0 {
		t.Errorf("queryChanges %v", qc)
	}
}

// TestWebmailSendFlow follows the pinned webmail (root-fr/jmap-webmail
// 1.7.3): Email/set create, then the submission with its hook, in one
// request. The client fails the send when any response has a truthy
// notCreated or notUpdated, so success must report them as null.
func TestWebmailSendFlow(t *testing.T) {
	s, m := sendingFixture()
	ident := model.IdentityID("me@example.com")
	out := runSubmit(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","create":{"draft-1":{"from":[{"email":"me@example.com"}],"to":[{"email":"bob@example.net"}],
			"subject":"hi","keywords":{"$draft":true},"mailboxIds":{"drafts":true},
			"bodyValues":{"1":{"value":"hello"}},"textBody":[{"partId":"1","type":"text/plain"}]}}},"0"],
		["EmailSubmission/set",{"accountId":"$ACCT","create":{"1":{"emailId":"#draft-1","identityId":"`+ident+`"}},
			"onSuccessUpdateEmail":{"#1":{"mailboxIds/drafts":null,"mailboxIds/sent":true,"keywords/$draft":null,"keywords/$seen":true}}},"1"]]`)
	if len(out) != 3 {
		t.Fatalf("responses %v", out)
	}
	for i, name := range []string{"Email/set", "EmailSubmission/set", "Email/set"} {
		r := result(t, out, i, name)
		for _, k := range []string{"notCreated", "notUpdated", "notDestroyed"} {
			if v, ok := r[k]; !ok || v != nil {
				t.Errorf("%s %s = %v, want null", name, k, v)
			}
		}
	}
	id := result(t, out, 0, "Email/set")["created"].(map[string]any)["draft-1"].(map[string]any)["id"].(string)
	if e := m.emails[id]; !e.MailboxIDs["sent"] || e.MailboxIDs["drafts"] || e.Keywords["$draft"] || len(m.sends) != 1 {
		t.Errorf("sent copy %+v sends %v", e, m.sends)
	}
	// A failure is still reported by id.
	out = runSubmit(t, s, owner, `[["Email/set",{"accountId":"$ACCT","update":{"Mnone":{"keywords/$seen":true}}},"0"]]`)
	if nu, _ := result(t, out, 0, "Email/set")["notUpdated"].(map[string]any); nu["Mnone"] == nil {
		t.Errorf("notUpdated %v", nu)
	}
}
