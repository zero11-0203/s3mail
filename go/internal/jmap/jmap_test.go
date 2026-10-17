package jmap

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// memStore is an in-memory account that commits through model.Plan with
// the same sequence guard as the DynamoDB store.
type memStore struct {
	mu        sync.Mutex
	st        State
	emails    map[string]EmailMeta
	mailboxes map[string]Mailbox
	uploads   map[string]model.Upload
	changes   []Change
	raw       map[string][]byte
}

func (m *memStore) State(context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st, nil
}

func (m *memStore) ListEmails(context.Context) ([]EmailMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EmailMeta
	for _, e := range m.emails {
		out = append(out, e.Clone())
	}
	return out, nil
}

func (m *memStore) GetEmails(_ context.Context, ids []string) (map[string]EmailMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]EmailMeta{}
	for _, id := range ids {
		if e, ok := m.emails[id]; ok {
			out[id] = e.Clone()
		}
	}
	return out, nil
}

func (m *memStore) ListMailboxes(context.Context) ([]Mailbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Collect(maps.Values(m.mailboxes)), nil
}

func (m *memStore) MailboxCounts(context.Context) (map[string]model.Counts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]model.Counts{}
	threads := map[model.MBThreadKey]model.MBThread{}
	for _, e := range m.emails {
		for mb := range e.MailboxIDs {
			c := out[mb]
			c.TotalEmails++
			k := model.MBThreadKey{Mailbox: mb, Thread: e.ThreadID}
			t := threads[k]
			t.Emails++
			if e.Unread() {
				c.UnreadEmails++
				t.Unread++
			}
			out[mb], threads[k] = c, t
		}
	}
	for k, t := range threads {
		c := out[k.Mailbox]
		c.TotalThreads++
		if t.Unread > 0 {
			c.UnreadThreads++
		}
		out[k.Mailbox] = c
	}
	return out, nil
}

func (m *memStore) ThreadMembers(_ context.Context, ids []string) (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	var all []EmailMeta
	for _, e := range m.emails {
		if slices.Contains(ids, e.ThreadID) {
			all = append(all, e)
		}
	}
	slices.SortFunc(all, func(a, b EmailMeta) int {
		if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, e := range all {
		out[e.ThreadID] = append(out[e.ThreadID], e.ID)
	}
	return out, nil
}

func (m *memStore) Changes(_ context.Context, after, upto int64) ([]Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Change
	for _, c := range m.changes {
		if c.Seq > after && c.Seq <= upto {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memStore) GetUpload(_ context.Context, id string) (model.Upload, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.uploads[id]
	return u, ok, nil
}

func (m *memStore) ByMessageID(_ context.Context, keys []string) (map[string][]model.ThreadRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]model.ThreadRef{}
	for _, e := range m.emails {
		for _, k := range e.MessageIDKeys() {
			if slices.Contains(keys, k) {
				out[k] = append(out[k], model.ThreadRef{EmailID: e.ID, ThreadID: e.ThreadID, ReceivedAt: e.ReceivedAt, Addresses: e.Addresses()})
			}
		}
	}
	return out, nil
}

func (m *memStore) BySubject(_ context.Context, key string) ([]model.ThreadRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.ThreadRef
	for _, e := range m.emails {
		if e.SubjectKey() == key {
			out = append(out, model.ThreadRef{EmailID: e.ID, ThreadID: e.ThreadID, ReceivedAt: e.ReceivedAt, Addresses: e.Addresses()})
		}
	}
	return out, nil
}

// memDerived answers Plan's reads from the current emails.
type memDerived struct{ emails map[string]EmailMeta }

func (d memDerived) MBThread(k model.MBThreadKey) model.MBThread {
	var t model.MBThread
	for _, e := range d.emails {
		if e.ThreadID == k.Thread && e.MailboxIDs[k.Mailbox] {
			t.Emails++
			if e.Unread() {
				t.Unread++
			}
		}
	}
	return t
}

func (d memDerived) ThreadSize(id string) int64 {
	var n int64
	for _, e := range d.emails {
		if e.ThreadID == id {
			n++
		}
	}
	return n
}

func (m *memStore) Commit(_ context.Context, prev State, mu model.Mutation) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev != m.st {
		return State{}, ErrStateChanged
	}
	ef := model.Plan(mu, memDerived{m.emails})
	for _, c := range mu.Emails {
		if c.After != nil {
			m.emails[c.After.ID] = c.After.Clone()
		} else {
			delete(m.emails, c.Before.ID)
		}
	}
	for _, c := range mu.Mailboxes {
		if c.After != nil {
			m.mailboxes[c.After.ID] = *c.After
		} else {
			delete(m.mailboxes, c.Before.ID)
		}
	}
	for _, u := range mu.Uploads {
		m.uploads[u.ID] = u
	}
	m.st = ef.Next(m.st)
	ch := ef.Change
	ch.Seq = m.st.Seq
	m.changes = append(m.changes, ch)
	return m.st, nil
}

// add ingests an Email the way the receipt commit does.
func (m *memStore) add(e EmailMeta, raw string) {
	if e.ThreadID == "" {
		e.ThreadID = "T" + e.ID[1:]
	}
	e.Blob = BlobRef{Bucket: "b", Key: "inbound/" + e.ID, VersionID: "v1"}
	e.Size = int64(len(raw))
	if e.Keywords == nil {
		e.Keywords = map[string]bool{}
	}
	m.mu.Lock()
	m.raw[e.ID] = []byte(raw)
	st := m.st
	m.mu.Unlock()
	if _, err := m.Commit(context.Background(), st, model.Mutation{Emails: []model.EmailChange{{After: &e}}}); err != nil {
		panic(err)
	}
}

func (m *memStore) Fetch(_ context.Context, ref BlobRef) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.raw[strings.TrimPrefix(ref.Key, "inbound/")], nil
}

const (
	idA = "Maaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "Mbbbbbbbbbbbbbbbbbbbbbbbb"
	idC = "Mcccccccccccccccccccccccc"
)

var t0 = time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)

const rawA = "From: =?utf-8?q?J=C3=B6rg?= <j@example.net>\r\nTo: alice@example.com\r\nSubject: =?ISO-2022-JP?B?GyRCRnxLXBsoQg==?=\r\n" +
	"Date: Sat, 17 Oct 2026 15:00:00 +0900\r\nMessage-ID: <a@example.net>\r\n" +
	"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello there, this is A\r\n" +
	"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0x\r\n--b--\r\n"

func fixture() (*Server, *memStore) {
	m := &memStore{st: State{Epoch: 1}, emails: map[string]EmailMeta{}, mailboxes: map[string]Mailbox{}, uploads: map[string]model.Upload{}, raw: map[string][]byte{}}
	m.add(EmailMeta{ID: idA, MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: t0, Subject: "日本", From: []string{"j@example.net"}}, rawA)
	m.add(EmailMeta{ID: idB, MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: t0.Add(time.Hour), Subject: "second", From: []string{"bob@example.net"},
		Keywords: map[string]bool{"$seen": true}}, "Subject: second\r\n\r\nB body\r\n")
	m.add(EmailMeta{ID: idC, MailboxIDs: map[string]bool{"quarantine": true}, ReceivedAt: t0.Add(2 * time.Hour), Subject: "q"}, "Subject: q\r\n\r\nC\r\n")
	s := NewServer("https://api.example.com/v1", m, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.PushPoll = 10 * time.Millisecond
	return s, m
}

var owner = Account{InternalID: "primary", Username: "owner", Scopes: []string{ScopeRead, ScopeWrite, ScopeSubmit}}

func acctID() string { return PublicAccountID("primary", 1) }

// run executes one request and returns the decoded method responses.
func run(t *testing.T, s *Server, acct Account, calls string) []any {
	t.Helper()
	calls = strings.ReplaceAll(calls, "$ACCT", acctID())
	var req Request
	if err := json.Unmarshal([]byte(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":`+calls+`}`), &req); err != nil {
		t.Fatal(err)
	}
	resp := s.Execute(context.Background(), acct, &req)
	b, _ := json.Marshal(resp.MethodResponses)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
}

func result(t *testing.T, out []any, i int, name string) map[string]any {
	t.Helper()
	inv := out[i].([]any)
	if inv[0] != name {
		t.Fatalf("response %d is %v %v, want %s", i, inv[0], inv[1], name)
	}
	return inv[1].(map[string]any)
}

func errType(t *testing.T, out []any, i int) string {
	t.Helper()
	inv := out[i].([]any)
	if inv[0] != "error" {
		t.Fatalf("response %d is %v, want an error", i, inv[0])
	}
	return inv[1].(map[string]any)["type"].(string)
}

func TestSession(t *testing.T) {
	s, _ := fixture()
	sess, err := s.Session(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if sess.PrimaryAccounts[CapMail] != acctID() || sess.APIURL != "https://api.example.com/v1/jmap" {
		t.Errorf("session %+v", sess)
	}
	if _, ok := sess.Capabilities[CapCore].(map[string]any)["maxSizeUpload"]; !ok {
		t.Error("core capability lacks limits")
	}
	if !strings.Contains(sess.DownloadURL, "{blobId}") || !strings.Contains(sess.EventSourceURL, "{closeafter}") {
		t.Errorf("templates %q %q", sess.DownloadURL, sess.EventSourceURL)
	}
}

func TestRequestErrors(t *testing.T) {
	s, _ := fixture()
	owners := Owners{"sub-1": "primary"}
	call := func(body string) (int, string) {
		req := events.APIGatewayProxyRequest{HTTPMethod: "POST", Resource: "/jmap", Body: body,
			Headers: map[string]string{"content-type": "application/json"}}
		req.RequestContext.Authorizer = map[string]any{"sub": "sub-1", "scope": ScopeRead}
		resp, _ := s.HandleAPI(context.Background(), req, owners)
		b, _ := io.ReadAll(resp.Body)
		var p Problem
		_ = json.Unmarshal(b, &p)
		return resp.StatusCode, strings.TrimPrefix(p.Type, "urn:ietf:params:jmap:error:") + p.Limit
	}
	many := strings.Repeat(`["Core/echo",{},"c"],`, MaxCallsInRequest+1)
	cases := map[string]struct {
		body string
		want string
	}{
		"not json":           {`{`, "notJSON"},
		"not request":        {`{"using":"x"}`, "notRequest"},
		"no methodCalls":     {`{"using":[]}`, "notRequest"},
		"unknown capability": {`{"using":["urn:example:nope"],"methodCalls":[]}`, "unknownCapability"},
		"too many calls":     {`{"using":[],"methodCalls":[` + strings.TrimSuffix(many, ",") + `]}`, "limitmaxCallsInRequest"},
		"too large":          {`{"using":[],"methodCalls":[],"x":"` + strings.Repeat("a", MaxSizeRequest) + `"}`, "limitmaxSizeRequest"},
	}
	for name, c := range cases {
		if status, got := call(c.body); status != 400 || got != c.want {
			t.Errorf("%s: %d %q, want 400 %q", name, status, got, c.want)
		}
	}

	req := events.APIGatewayProxyRequest{HTTPMethod: "POST", Resource: "/jmap", Body: `{"using":[],"methodCalls":[]}`}
	req.RequestContext.Authorizer = map[string]any{"sub": "someone-else", "scope": ScopeRead}
	if resp, _ := s.HandleAPI(context.Background(), req, owners); resp.StatusCode != 403 {
		t.Errorf("unmapped subject got %d", resp.StatusCode)
	}
}

func TestMethodErrors(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[["Nope/get",{},"0"],
		["Mailbox/get",{"accountId":"Awrong"},"1"],
		["Mailbox/get",{"accountId":"$ACCT","bogus":1},"2"],
		["Email/get",{"accountId":"$ACCT","ids":[],"properties":["nope"]},"3"],
		["Core/echo",{"hello":true},"4"]]`)
	for i, want := range []string{"unknownMethod", "accountNotFound", "invalidArguments", "invalidArguments"} {
		if got := errType(t, out, i); got != want {
			t.Errorf("call %d: %s, want %s", i, got, want)
		}
	}
	if r := result(t, out, 4, "Core/echo"); r["hello"] != true {
		t.Errorf("echo %v", r)
	}

	readOnly := Account{InternalID: "primary", Scopes: []string{ScopeRead}}
	out = run(t, s, readOnly, `[["Email/set",{"accountId":"$ACCT","update":{}},"0"]]`)
	if got := errType(t, out, 0); got != "forbidden" {
		t.Errorf("write without mail.write: %s", got)
	}

	var req Request
	_ = json.Unmarshal([]byte(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Email/get",{"accountId":"x"},"0"]]}`), &req)
	b, _ := json.Marshal(s.Execute(context.Background(), owner, &req).MethodResponses)
	if !strings.Contains(string(b), "unknownMethod") {
		t.Errorf("a method outside the declared capabilities must be unknownMethod: %s", b)
	}
}

func TestQueryThenGetByReference(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"inbox"},"sort":[{"property":"receivedAt","isAscending":false}],"calculateTotal":true},"q"],
		["Email/get",{"accountId":"$ACCT","#ids":{"resultOf":"q","name":"Email/query","path":"/ids"},"properties":["subject","from","mailboxIds"]},"g"],
		["Email/get",{"accountId":"$ACCT","#ids":{"resultOf":"q","name":"Email/changes","path":"/ids"}},"bad"]]`)
	q := result(t, out, 0, "Email/query")
	ids := q["ids"].([]any)
	if len(ids) != 2 || ids[0] != idB || ids[1] != idA || q["total"].(float64) != 2 || q["canCalculateChanges"] != true {
		t.Fatalf("query %v", q)
	}
	g := result(t, out, 1, "Email/get")
	list := g["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("get %v", g)
	}
	a := list[1].(map[string]any)
	if a["subject"] != "日本" {
		t.Errorf("subject %v", a["subject"])
	}
	from := a["from"].([]any)[0].(map[string]any)
	if from["name"] != "Jörg" || from["email"] != "j@example.net" {
		t.Errorf("from %v", from)
	}
	if errType(t, out, 2) != "invalidResultReference" {
		t.Error("a reference to the wrong method name must fail")
	}
}

func TestEmailQueryOptions(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT","filter":{"operator":"AND","conditions":[{"notKeyword":"$seen"},{"inMailboxOtherThan":["trash"]}]}},"0"],
		["Email/query",{"accountId":"$ACCT","sort":[{"property":"receivedAt"}],"position":1,"limit":1},"1"],
		["Email/query",{"accountId":"$ACCT","sort":[{"property":"receivedAt"}],"anchor":"`+idB+`","anchorOffset":1},"2"],
		["Email/query",{"accountId":"$ACCT","filter":{"text":"x"}},"3"],
		["Email/query",{"accountId":"$ACCT","sort":[{"property":"nope"}]},"4"],
		["Email/query",{"accountId":"$ACCT","anchor":"Mzzzzzzzzzzzzzzzzzzzzzzzz"},"5"],
		["Email/queryChanges",{"accountId":"$ACCT","sinceQueryState":"q1-3"},"6"]]`)
	if ids := result(t, out, 0, "Email/query")["ids"].([]any); len(ids) != 2 {
		t.Errorf("unseen outside trash: %v", ids)
	}
	if ids := result(t, out, 1, "Email/query")["ids"].([]any); len(ids) != 1 || ids[0] != idB {
		t.Errorf("position/limit: %v", ids)
	}
	if ids := result(t, out, 2, "Email/query")["ids"].([]any); len(ids) != 1 || ids[0] != idC {
		t.Errorf("anchor: %v", ids)
	}
	for i, want := range map[int]string{3: "unsupportedFilter", 4: "unsupportedSort", 5: "anchorNotFound", 6: "cannotCalculateChanges"} {
		if got := errType(t, out, i); got != want {
			t.Errorf("call %d: %s, want %s", i, got, want)
		}
	}
}

func TestEmailGetBody(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[["Email/get",{"accountId":"$ACCT","ids":["`+idA+`","Mzzzzzzzzzzzzzzzzzzzzzzzz"],
		"properties":["blobId","bodyStructure","textBody","attachments","bodyValues","hasAttachment","preview","headers","header:Subject:asText","header:X-Missing:all","sentAt","messageId"],
		"fetchTextBodyValues":true,"maxBodyValueBytes":5}, "0"]]`)
	g := result(t, out, 0, "Email/get")
	if nf := g["notFound"].([]any); len(nf) != 1 {
		t.Errorf("notFound %v", nf)
	}
	e := g["list"].([]any)[0].(map[string]any)
	if e["blobId"] != rawBlobID(idA) || e["hasAttachment"] != true || e["preview"] != "hello there, this is A" {
		t.Errorf("email %v", e)
	}
	if e["header:Subject:asText"] != "日本" || len(e["header:X-Missing:all"].([]any)) != 0 {
		t.Errorf("header forms %v %v", e["header:Subject:asText"], e["header:X-Missing:all"])
	}
	if e["sentAt"] != "2026-10-17T15:00:00+09:00" || e["messageId"].([]any)[0] != "a@example.net" {
		t.Errorf("sentAt %v messageId %v", e["sentAt"], e["messageId"])
	}
	bs := e["bodyStructure"].(map[string]any)
	subs := bs["subParts"].([]any)
	if bs["type"] != "multipart/mixed" || bs["blobId"] != nil || len(subs) != 2 {
		t.Fatalf("bodyStructure %v", bs)
	}
	att := e["attachments"].([]any)[0].(map[string]any)
	if att["blobId"] != partBlobID(idA, "2") || att["name"] != "a.pdf" || att["size"].(float64) != 6 {
		t.Errorf("attachment %v", att)
	}
	bv := e["bodyValues"].(map[string]any)["1"].(map[string]any)
	if bv["value"] != "hello" || bv["isTruncated"] != true {
		t.Errorf("bodyValues %v", bv)
	}
	if len(e["headers"].([]any)) != 6 {
		t.Errorf("headers %v", e["headers"])
	}
}

func TestEmailSetAndChanges(t *testing.T) {
	s, m := fixture()
	out := run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","ifInState":"1-3","update":{
			"`+idA+`":{"keywords/$seen":true,"mailboxIds/inbox":null,"mailboxIds/archive":true},
			"`+idB+`":{"mailboxIds":{}},
			"`+idC+`":{"subject":"x"},
			"Mzzzzzzzzzzzzzzzzzzzzzzzz":{"keywords/$flagged":true}},
			"destroy":["Mzzzzzzzzzzzzzzzzzzzzzzzz"],"create":{"k1":{}}},"0"],
		["Email/set",{"accountId":"$ACCT","ifInState":"1-3","update":{}},"1"],
		["Email/changes",{"accountId":"$ACCT","sinceState":"1-3"},"2"],
		["Email/changes",{"accountId":"$ACCT","sinceState":"1-0","maxChanges":2},"3"],
		["Mailbox/changes",{"accountId":"$ACCT","sinceState":"1-3"},"4"],
		["Mailbox/get",{"accountId":"$ACCT","ids":["inbox","archive"],"properties":["unreadEmails","totalEmails"]},"5"],
		["Thread/changes",{"accountId":"$ACCT","sinceState":"1-0"},"6"],
		["Email/changes",{"accountId":"$ACCT","sinceState":"2-0"},"7"]]`)
	r := result(t, out, 0, "Email/set")
	if _, ok := r["updated"].(map[string]any)[idA]; !ok || r["newState"] != "1-4" || r["oldState"] != "1-3" {
		t.Fatalf("set %v", r)
	}
	nu := r["notUpdated"].(map[string]any)
	for id, want := range map[string]string{idB: "invalidProperties", idC: "invalidProperties", "Mzzzzzzzzzzzzzzzzzzzzzzzz": "notFound"} {
		if got := nu[id].(map[string]any)["type"]; got != want {
			t.Errorf("notUpdated %s = %v, want %s", id, got, want)
		}
	}
	if r["notDestroyed"].(map[string]any)["Mzzzzzzzzzzzzzzzzzzzzzzzz"] == nil || r["notCreated"].(map[string]any)["k1"] == nil {
		t.Error("destroy and create must be refused per object")
	}
	if a := m.emails[idA]; !a.Keywords["$seen"] || a.MailboxIDs["inbox"] || !a.MailboxIDs["archive"] {
		t.Errorf("stored %+v", a)
	}
	if errType(t, out, 1) != "stateMismatch" {
		t.Error("stale ifInState must fail")
	}
	ch := result(t, out, 2, "Email/changes")
	if u := ch["updated"].([]any); len(u) != 1 || u[0] != idA || ch["newState"] != "1-4" {
		t.Errorf("changes %v", ch)
	}
	page := result(t, out, 3, "Email/changes")
	if c := page["created"].([]any); len(c) != 2 || page["hasMoreChanges"] != true || page["newState"] != "1-2" {
		t.Errorf("paged changes %v", page)
	}
	mc := result(t, out, 4, "Mailbox/changes")
	if len(mc["updated"].([]any)) != 2 || mc["updatedProperties"] == nil || mc["newState"] != "1-4" {
		t.Errorf("mailbox changes %v", mc)
	}
	for _, mb := range result(t, out, 5, "Mailbox/get")["list"].([]any) {
		o := mb.(map[string]any)
		if o["id"] == "inbox" && (o["totalEmails"].(float64) != 1 || o["unreadEmails"].(float64) != 0) {
			t.Errorf("inbox counts %v", o)
		}
		if o["id"] == "archive" && o["totalEmails"].(float64) != 1 {
			t.Errorf("archive counts %v", o)
		}
	}
	if c := result(t, out, 6, "Thread/changes")["created"].([]any); len(c) != 3 || c[0] != "T"+idA[1:] {
		t.Errorf("thread changes %v", c)
	}
	if errType(t, out, 7) != "cannotCalculateChanges" {
		t.Error("a foreign-epoch state must fail")
	}
}

func TestChangesCoalesce(t *testing.T) {
	s, m := fixture()
	m.mu.Lock()
	m.st.Seq, m.st.Email = 5, 5
	m.changes = append(m.changes,
		Change{Seq: 4, Updated: map[string][]string{"Email": {idC}}},
		Change{Seq: 5, Destroyed: map[string][]string{"Email": {idC, idB}}})
	m.mu.Unlock()
	ch := result(t, run(t, s, owner, `[["Email/changes",{"accountId":"$ACCT","sinceState":"1-2"},"0"]]`), 0, "Email/changes")
	// idC was created after state 2 and destroyed: not reported. idB
	// existed at state 2: destroyed.
	if c, d := ch["created"].([]any), ch["destroyed"].([]any); len(c) != 0 || len(d) != 1 || d[0] != idB || len(ch["updated"].([]any)) != 0 {
		t.Errorf("coalesced %v", ch)
	}
}

func authed(path string, params map[string]string, query map[string]string) events.APIGatewayProxyRequest {
	req := events.APIGatewayProxyRequest{HTTPMethod: "GET", Resource: path, PathParameters: params, QueryStringParameters: query}
	req.RequestContext.Authorizer = map[string]any{"sub": "sub-1", "scope": ScopeRead, "exp": float64(time.Now().Add(time.Minute).Unix())}
	return req
}

func TestDownload(t *testing.T) {
	s, _ := fixture()
	owners := Owners{"sub-1": "primary"}
	get := func(acct, blob string) (int, map[string]string, string) {
		resp, _ := s.HandleDownload(context.Background(), authed("/download/{accountId}/{blobId}/{name}",
			map[string]string{"accountId": acct, "blobId": blob, "name": "a <b>.pdf"}, map[string]string{"type": "application/pdf"}), owners)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Headers, string(b)
	}
	status, h, body := get(acctID(), partBlobID(idA, "2"))
	if status != 200 || body != "%PDF-1" {
		t.Fatalf("part download %d %q", status, body)
	}
	if !strings.HasPrefix(h["Content-Disposition"], "attachment;") || h["X-Content-Type-Options"] != "nosniff" || !strings.Contains(h["Content-Security-Policy"], "sandbox") {
		t.Errorf("headers %v", h)
	}
	if status, _, body := get(acctID(), rawBlobID(idA)); status != 200 || body != rawA {
		t.Errorf("raw download %d", status)
	}
	for name, blob := range map[string]string{"unknown email": "Gzzzzzzzzzzzzzzzzzzzzzzzz", "bad id": "../x", "missing part": partBlobID(idA, "9")} {
		if status, _, _ := get(acctID(), blob); status != 404 {
			t.Errorf("%s: %d", name, status)
		}
	}
	if status, _, _ := get("Aother", rawBlobID(idA)); status != 404 {
		t.Errorf("foreign account: %d", status)
	}
}

func TestEventSource(t *testing.T) {
	s, m := fixture()
	// The stream stops 15 s before the deadline, leaving 5 s here.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, _ := s.HandleEventSource(ctx, authed("/eventsource", nil, map[string]string{"types": "*", "closeafter": "state", "ping": "0"}), Owners{"sub-1": "primary"})
	if resp.StatusCode != 200 || resp.Headers["Content-Type"] != "text/event-stream" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Headers)
	}
	sc := bufio.NewScanner(resp.Body)
	var states []string
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			states = append(states, line)
			if len(states) == 1 {
				m.add(EmailMeta{ID: "Mdddddddddddddddddddddddd", MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: t0}, "x")
			}
		}
	}
	if len(states) != 2 || !strings.Contains(states[0], `"Email":"1-3"`) || !strings.Contains(states[1], `"Email":"1-4"`) {
		t.Errorf("events %v", states)
	}
}

func TestMailboxQuerySetAndThreads(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Mailbox/query",{"accountId":"$ACCT","filter":{"role":"inbox"}},"0"],
		["Mailbox/query",{"accountId":"$ACCT","filter":{"hasAnyRole":false},"sort":[{"property":"sortOrder"}]},"1"],
		["Mailbox/query",{"accountId":"$ACCT","filter":{"isSubscribed":true,"bogus":1}},"2"],
		["Mailbox/set",{"accountId":"$ACCT","create":{"k":{"name":"x"}},"destroy":["inbox"]},"3"],
		["Thread/get",{"accountId":"$ACCT","ids":["`+"T"+idA[1:]+`","Tzzzzzzzzzzzzzzzzzzzzzzzz","bogus"]},"4"],
		["Thread/get",{"accountId":"$ACCT","ids":null},"5"],
		["Email/get",{"accountId":"$ACCT","ids":null,"properties":["threadId"]},"6"]]`)
	if ids := result(t, out, 0, "Mailbox/query")["ids"].([]any); len(ids) != 1 || ids[0] != "inbox" {
		t.Errorf("role filter %v", ids)
	}
	if ids := result(t, out, 1, "Mailbox/query")["ids"].([]any); len(ids) != 1 || ids[0] != "quarantine" {
		t.Errorf("hasAnyRole filter %v", ids)
	}
	if errType(t, out, 2) != "unsupportedFilter" {
		t.Error("an unsupported Mailbox filter must fail")
	}
	r := result(t, out, 3, "Mailbox/set")
	if r["created"].(map[string]any)["k"] == nil || r["notDestroyed"].(map[string]any)["inbox"].(map[string]any)["type"] != "forbidden" {
		t.Errorf("Mailbox/set: %v", r)
	}
	th := result(t, out, 4, "Thread/get")
	if l := th["list"].([]any); len(l) != 1 || l[0].(map[string]any)["emailIds"].([]any)[0] != idA || len(th["notFound"].([]any)) != 2 {
		t.Errorf("Thread/get %v", th)
	}
	if errType(t, out, 5) != "requestTooLarge" {
		t.Error("Thread/get without ids must not list every thread")
	}
	if l := result(t, out, 6, "Email/get")["list"].([]any); len(l) != 3 {
		t.Errorf("Email/get with null ids returns every Email up to maxObjectsInGet: %d", len(l))
	}
}
