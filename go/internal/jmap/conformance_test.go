package jmap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
)

// Regression tests for docs/evidence/jmap-rfc-review.md.

// F3: the response limit appears exactly when the server set or changed it.
func TestQueryLimitReported(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT"},"0"],
		["Email/query",{"accountId":"$ACCT","limit":1024},"1"],
		["Email/query",{"accountId":"$ACCT","limit":5000},"2"],
		["Mailbox/query",{"accountId":"$ACCT","limit":2},"3"]]`)
	for i, want := range map[int]any{0: float64(1024), 1: nil, 2: float64(1024), 3: nil} {
		name := "Email/query"
		if i == 3 {
			name = "Mailbox/query"
		}
		if got := result(t, out, i, name)["limit"]; got != want {
			t.Errorf("call %d: limit %v, want %v", i, got, want)
		}
	}
}

// A11: /queryChanges accepts maxChanges 0; only a change exceeds it.
func TestQueryChangesMaxChangesZero(t *testing.T) {
	s, _ := fixture()
	qs := result(t, run(t, s, owner, `[["Email/query",{"accountId":"$ACCT"},"0"]]`), 0, "Email/query")["queryState"].(string)
	out := run(t, s, owner, `[
		["Email/queryChanges",{"accountId":"$ACCT","sinceQueryState":"`+qs+`","maxChanges":0},"0"],
		["Email/queryChanges",{"accountId":"$ACCT","sinceQueryState":"`+qs+`","maxChanges":-1},"1"],
		["Email/set",{"accountId":"$ACCT","update":{"`+idA+`":{"keywords/$flagged":true}}},"2"],
		["Email/queryChanges",{"accountId":"$ACCT","sinceQueryState":"`+qs+`","maxChanges":0},"3"]]`)
	if r := result(t, out, 0, "Email/queryChanges"); len(r["removed"].([]any)) != 0 || len(r["added"].([]any)) != 0 {
		t.Errorf("no changes: %v", r)
	}
	if errType(t, out, 1) != "invalidArguments" || errType(t, out, 3) != "tooManyChanges" {
		t.Errorf("negative or exceeded maxChanges: %v", out)
	}
}

// A10: success and failure maps of /set and /import are null when empty.
func TestEmptySetResultsAreNull(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT"},"0"],
		["Mailbox/set",{"accountId":"$ACCT","destroy":["inbox"]},"1"],
		["Email/import",{"accountId":"$ACCT","emails":{}},"2"]]`)
	for i, name := range []string{"Email/set", "Mailbox/set"} {
		r := result(t, out, i, name)
		for _, k := range []string{"created", "updated", "destroyed"} {
			if v, ok := r[k]; !ok || v != nil {
				t.Errorf("%s %s = %v, want null", name, k, v)
			}
		}
	}
	if r := result(t, out, 2, "Email/import"); r["created"] != nil || r["notCreated"] != nil {
		t.Errorf("Email/import %v", r)
	}
}

// A7: maxSizeMailboxName counts UTF-8 octets.
func TestMailboxNameLimitInOctets(t *testing.T) {
	s, _ := fixture()
	long := strings.Repeat("界", 86) // 258 octets, 86 characters
	fits := strings.Repeat("界", 85) // 255 octets
	out := run(t, s, owner, `[["Mailbox/set",{"accountId":"$ACCT","create":{"long":{"name":"`+long+`"},"fits":{"name":"`+fits+`"}}},"0"]]`)
	r := result(t, out, 0, "Mailbox/set")
	if r["notCreated"].(map[string]any)["long"] == nil || r["created"].(map[string]any)["fits"] == nil {
		t.Errorf("name limits %v", r)
	}
}

// A4: a patch path escapes "/" inside a keyword as "~1".
func TestEscapedKeywordPatch(t *testing.T) {
	s, m := fixture()
	out := run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","update":{"`+idA+`":{"keywords/a~1b":true}}},"0"],
		["Email/set",{"accountId":"$ACCT","update":{"`+idB+`":{"keywords/a/b":true}}},"1"]]`)
	if r := result(t, out, 0, "Email/set"); r["notUpdated"] != nil || !m.emails[idA].Keywords["a/b"] {
		t.Errorf("escaped keyword: %v", r)
	}
	if nu := result(t, out, 1, "Email/set")["notUpdated"].(map[string]any); nu[idB].(map[string]any)["type"] != "invalidPatch" {
		t.Errorf("nested path: %v", nu)
	}
	out = run(t, s, owner, `[["Email/set",{"accountId":"$ACCT","update":{"`+idA+`":{"keywords/a~1b":null}}},"0"]]`)
	if r := result(t, out, 0, "Email/set"); r["notUpdated"] != nil || m.emails[idA].Keywords["a/b"] {
		t.Errorf("escaped keyword removal: %v", r)
	}
}

// A5: unchanged server-set values may be sent back in an update.
func TestUnchangedServerSetValuesInUpdate(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/get",{"accountId":"$ACCT","ids":["`+idA+`"],"properties":["blobId","threadId","size"]},"0"],
		["Mailbox/get",{"accountId":"$ACCT","ids":["inbox"]},"1"]]`)
	e := result(t, out, 0, "Email/get")["list"].([]any)[0].(map[string]any)
	mb := result(t, out, 1, "Mailbox/get")["list"].([]any)[0].(map[string]any)
	patch := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) }
	out = run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","update":{"`+idA+`":`+patch(map[string]any{"id": idA, "blobId": e["blobId"], "threadId": e["threadId"], "size": e["size"], "keywords/$flagged": true})+`}},"0"],
		["Email/set",{"accountId":"$ACCT","update":{"`+idA+`":{"size":1}}},"1"],
		["Mailbox/set",{"accountId":"$ACCT","update":{"inbox":`+patch(map[string]any{"id": "inbox", "totalEmails": mb["totalEmails"], "myRights": mb["myRights"], "sortOrder": 9})+`}},"2"],
		["Mailbox/set",{"accountId":"$ACCT","update":{"inbox":{"unreadEmails":99}}},"3"],
		["Mailbox/set",{"accountId":"$ACCT","create":{"x":{"name":"x","totalEmails":0}}},"4"]]`)
	if r := result(t, out, 0, "Email/set"); r["notUpdated"] != nil {
		t.Errorf("unchanged Email server-set values: %v", r)
	}
	if r := result(t, out, 2, "Mailbox/set"); r["notUpdated"] != nil {
		t.Errorf("unchanged Mailbox server-set values: %v", r)
	}
	for i, k := range map[int]string{1: "notUpdated", 3: "notUpdated", 4: "notCreated"} {
		name := "Email/set"
		if i > 1 {
			name = "Mailbox/set"
		}
		if result(t, out, i, name)[k] == nil {
			t.Errorf("call %d: a changed or created server-set value was accepted", i)
		}
	}
}

// A2: a Date or Message-ID given in any form or case is not generated
// again; an explicit sentAt still conflicts with another Date property.
func TestCreateWithHeaderFormDateAndMessageID(t *testing.T) {
	s, m := fixture()
	out := run(t, s, owner, `[["Email/set",{"accountId":"$ACCT","create":{
		"date":{"mailboxIds":{"drafts":true},"header:Date:asDate":"2026-10-17T06:00:00Z","subject":"hello"},
		"msgid":{"mailboxIds":{"drafts":true},"header:message-id:asMessageIds":["m1@example.com"]},
		"headers":{"mailboxIds":{"drafts":true},"headers":[{"name":"date","value":"Sat, 17 Oct 2026 06:00:00 +0000"}]},
		"both":{"mailboxIds":{"drafts":true},"sentAt":"2026-10-17T06:00:00Z","header:Date:asDate":"2026-10-17T06:00:00Z"}}},"0"]]`)
	r := result(t, out, 0, "Email/set")
	created := r["created"].(map[string]any)
	for _, k := range []string{"date", "msgid", "headers"} {
		c, ok := created[k].(map[string]any)
		if !ok {
			t.Errorf("%s not created: %v", k, r["notCreated"])
			continue
		}
		hdr := mailparse.Parse(m.raw["composed/"+c["id"].(string)]).Headers
		if len(hdr.All("Date")) != 1 || len(hdr.All("Message-ID")) != 1 {
			t.Errorf("%s: Date %v Message-ID %v", k, hdr.All("Date"), hdr.All("Message-ID"))
		}
	}
	if r["notCreated"].(map[string]any)["both"] == nil {
		t.Error("sentAt with header:Date:asDate must conflict")
	}
}

// A3: multipart parts keep their metadata and reject Content-Transfer-Encoding.
func TestCreateMultipartPartMetadata(t *testing.T) {
	s, m := fixture()
	out := run(t, s, owner, `[["Email/set",{"accountId":"$ACCT","create":{
		"ok":{"mailboxIds":{"drafts":true},"bodyValues":{"t":{"value":"x"}},"bodyStructure":{"type":"multipart/mixed",
			"disposition":"attachment","cid":"c1@example.com","header:X-Test":" yes","subParts":[{"partId":"t"}]}},
		"cte":{"mailboxIds":{"drafts":true},"bodyValues":{"t":{"value":"x"}},"bodyStructure":{"type":"multipart/mixed",
			"header:Content-Transfer-Encoding":"base64","subParts":[{"partId":"t"}]}},
		"blob":{"mailboxIds":{"drafts":true},"bodyValues":{"t":{"value":"x"}},"bodyStructure":{"type":"multipart/mixed",
			"partId":"t","subParts":[{"partId":"t"}]}}}},"0"]]`)
	r := result(t, out, 0, "Email/set")
	c, ok := r["created"].(map[string]any)["ok"].(map[string]any)
	if !ok {
		t.Fatalf("not created: %v", r["notCreated"])
	}
	body := mailparse.Parse(m.raw["composed/"+c["id"].(string)]).Body
	if body.Disposition != "attachment" || body.CID != "c1@example.com" || len(body.Headers.All("X-Test")) != 1 {
		t.Errorf("multipart metadata lost: %+v", body.Headers)
	}
	nc := r["notCreated"].(map[string]any)
	if nc["cte"] == nil || nc["blob"] == nil {
		t.Errorf("multipart content properties accepted: %v", r)
	}
}

// F5: forms RFC 8621 §4.1.2 forbids for a field are rejected.
func TestForbiddenHeaderForms(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Email/get",{"accountId":"$ACCT","ids":["`+idA+`"],"properties":["header:From:asDate"]},"0"],
		["Email/get",{"accountId":"$ACCT","ids":["`+idA+`"],"bodyProperties":["header:Subject:asURLs"],"properties":["bodyStructure"]},"1"],
		["Email/parse",{"accountId":"$ACCT","blobIds":["G`+idA[1:]+`"],"properties":["header:Received:asText"]},"2"],
		["Email/get",{"accountId":"$ACCT","ids":["`+idA+`"],"properties":["header:From:asGroupedAddresses","header:X-Custom:asDate","header:Received:asRaw","header:List-Id:asText"]},"3"],
		["Email/set",{"accountId":"$ACCT","create":{"x":{"mailboxIds":{"drafts":true},"header:Subject:asAddresses":[{"email":"a@example.com"}]}}},"4"]]`)
	for i := range 3 {
		if got := errType(t, out, i); got != "invalidArguments" {
			t.Errorf("call %d: %s, want invalidArguments", i, got)
		}
	}
	result(t, out, 3, "Email/get")
	if nc := result(t, out, 4, "Email/set")["notCreated"].(map[string]any); nc["x"].(map[string]any)["type"] != "invalidProperties" {
		t.Errorf("create with a forbidden form: %v", nc)
	}
}

// F4: from/to sort by display name (else email) and subject by base
// subject, with the i;unicode-casemap collation.
func TestEmailSortKeys(t *testing.T) {
	s, m := fixture()
	for id, e := range map[string]struct{ from, subject string }{
		"Mxaaaaaaaaaaaaaaaaaaaaaaa": {`"Zed" <a@example.com>`, "Re: Banana"},
		"Mxbbbbbbbbbbbbbbbbbbbbbbb": {`"amy" <z@example.com>`, "cherry"},
		"Mxccccccccccccccccccccccc": {"m@example.com", "Apple"},
	} {
		m.add(EmailMeta{ID: id, MailboxIDs: map[string]bool{"archive": true}, ReceivedAt: t0, From: []string{e.from}, To: []string{e.from}, Subject: e.subject}, "x")
	}
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"archive"},"sort":[{"property":"from"}]},"0"],
		["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"archive"},"sort":[{"property":"to","isAscending":false}]},"1"],
		["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"archive"},"sort":[{"property":"subject","collation":"i;unicode-casemap"}]},"2"]]`)
	want := map[int]string{
		0: "Mxbbbbbbbbbbbbbbbbbbbbbbb Mxccccccccccccccccccccccc Mxaaaaaaaaaaaaaaaaaaaaaaa",
		1: "Mxaaaaaaaaaaaaaaaaaaaaaaa Mxccccccccccccccccccccccc Mxbbbbbbbbbbbbbbbbbbbbbbb",
		2: "Mxccccccccccccccccccccccc Mxaaaaaaaaaaaaaaaaaaaaaaa Mxbbbbbbbbbbbbbbbbbbbbbbb",
	}
	for i, w := range want {
		var got []string
		for _, id := range result(t, out, i, "Email/query")["ids"].([]any) {
			got = append(got, id.(string))
		}
		if strings.Join(got, " ") != w {
			t.Errorf("sort %d: %v, want %s", i, got, w)
		}
	}
	if casemap("ǆ") != casemap("ǅ") || casemap("é") != casemap("É") {
		t.Error("casemap must titlecase and decompose")
	}
}

// F6: a created Mailbox reports its defaulted parentId and role.
func TestMailboxCreatedDefaults(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[["Mailbox/set",{"accountId":"$ACCT","create":{"x":{"name":"x"}}},"0"]]`)
	c := result(t, out, 0, "Mailbox/set")["created"].(map[string]any)["x"].(map[string]any)
	for _, k := range []string{"parentId", "role"} {
		if v, ok := c[k]; !ok || v != nil {
			t.Errorf("created %s = %v, want null", k, v)
		}
	}
}
