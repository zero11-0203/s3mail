package jmap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
)

func upload(t *testing.T, s *Server, ctype, body string) map[string]any {
	t.Helper()
	req := authed("/upload/{accountId}", map[string]string{"accountId": acctID()}, nil)
	req.HTTPMethod = "POST"
	req.RequestContext.Authorizer["scope"] = ScopeRead + " " + ScopeWrite
	req.Headers = map[string]string{"Content-Type": ctype}
	req.Body = base64.StdEncoding.EncodeToString([]byte(body))
	req.IsBase64Encoded = true
	resp, err := s.HandleUpload(context.Background(), req, Owners{"sub-1": "primary"})
	if err != nil || resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload %d %s %v", resp.StatusCode, b, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

const replyMIME = "From: Bob <bob@example.net>\r\nTo: alice@example.com\r\nSubject: Re: hello\r\n" +
	"Message-ID: <reply@example.net>\r\nIn-Reply-To: <a@example.net>\r\nDate: Sat, 17 Oct 2026 16:00:00 +0900\r\n" +
	"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
	"--x\r\nContent-Type: message/rfc822\r\n\r\nSubject: inner\r\nContent-Type: multipart/mixed; boundary=y\r\n\r\n" +
	"--y\r\nContent-Type: text/plain\r\n\r\ninner text\r\n--y\r\nContent-Type: text/csv\r\n\r\na,b\r\n--y--\r\n--x--\r\n"

func TestUploadImportParseAndDownload(t *testing.T) {
	s, m := fixture()
	up := upload(t, s, "message/rfc822", replyMIME)
	blobID := up["blobId"].(string)
	if up["type"] != "message/rfc822" || up["size"].(float64) != float64(len(replyMIME)) || blobID[0] != 'U' {
		t.Fatalf("upload %v", up)
	}
	out := run(t, s, owner, `[
		["Email/parse",{"accountId":"$ACCT","blobIds":["`+blobID+`","`+blobID[:1]+"zzzzzzzzzzzzzzzzzzzzzzzz"+`"],"properties":["subject","from","attachments","threadId"]},"0"],
		["Email/import",{"accountId":"$ACCT","emails":{
			"i":{"blobId":"`+blobID+`","mailboxIds":{"inbox":true},"keywords":{"$Seen":true},"receivedAt":"2026-10-17T07:00:00Z"},
			"bad":{"blobId":"Uzzzzzzzzzzzzzzzzzzzzzzzz","mailboxIds":{"inbox":true}},
			"nobox":{"blobId":"`+blobID+`","mailboxIds":{"nowhere":true}}}},"1"],
		["Email/get",{"accountId":"$ACCT","ids":["#i"],"properties":["threadId","subject","receivedAt","keywords","blobId","attachments"]},"2"]]`)
	p := result(t, out, 0, "Email/parse")
	parsed := p["parsed"].(map[string]any)[blobID].(map[string]any)
	att := parsed["attachments"].([]any)[0].(map[string]any)
	if parsed["subject"] != "Re: hello" || parsed["threadId"] != nil || att["type"] != "message/rfc822" || len(p["notFound"].([]any)) != 1 {
		t.Fatalf("parse %v", p)
	}
	// The attached message's parts are reachable by path.
	inner := att["blobId"].(string)
	out2 := run(t, s, owner, `[["Email/parse",{"accountId":"$ACCT","blobIds":["`+inner+`"],"properties":["subject","attachments"]},"0"]]`)
	ip := result(t, out2, 0, "Email/parse")["parsed"].(map[string]any)[inner].(map[string]any)
	csv := ip["attachments"].([]any)[0].(map[string]any)["blobId"].(string)
	if ip["subject"] != "inner" || csv != inner+".2" {
		t.Errorf("inner parse %v", ip)
	}
	if b, ok, err := s.readBlob(context.Background(), csv); err != nil || !ok || string(b.data) != "a,b" {
		t.Errorf("nested part %q %v %v", b.data, ok, err)
	}

	imp := result(t, out, 1, "Email/import")
	created := imp["created"].(map[string]any)["i"].(map[string]any)
	nc := imp["notCreated"].(map[string]any)
	if nc["bad"].(map[string]any)["type"] != "blobNotFound" || nc["nobox"].(map[string]any)["type"] != "invalidProperties" {
		t.Errorf("notCreated %v", nc)
	}
	if created["threadId"] != "T"+idA[1:] {
		t.Errorf("the reply joins its parent's thread: %v", created)
	}
	g := result(t, out, 2, "Email/get")["list"].([]any)[0].(map[string]any)
	if g["subject"] != "Re: hello" || g["receivedAt"] != "2026-10-17T07:00:00Z" || g["keywords"].(map[string]any)["$seen"] != true ||
		g["blobId"] != "G"+created["id"].(string)[1:] {
		t.Errorf("imported %v", g)
	}
	if string(m.raw["imported/"+created["id"].(string)]) != replyMIME {
		t.Error("the import archives the exact MIME")
	}

	// Downloads serve uploads until they expire.
	dl := func() int {
		resp, _ := s.HandleDownload(context.Background(), authed("/download/{accountId}/{blobId}/{name}",
			map[string]string{"accountId": acctID(), "blobId": blobID, "name": "m.eml"}, nil), Owners{"sub-1": "primary"})
		return resp.StatusCode
	}
	if dl() != 200 {
		t.Error("upload download")
	}
	s.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if dl() != 404 {
		t.Error("an expired upload is not served")
	}
	out = run(t, s, owner, `[["Email/import",{"accountId":"$ACCT","emails":{"late":{"blobId":"`+blobID+`","mailboxIds":{"inbox":true}}}},"0"]]`)
	if result(t, out, 0, "Email/import")["notCreated"].(map[string]any)["late"].(map[string]any)["type"] != "blobNotFound" {
		t.Error("an expired upload cannot be imported")
	}
}

func TestUploadLimitsAndScope(t *testing.T) {
	s, _ := fixture()
	req := authed("/upload/{accountId}", map[string]string{"accountId": acctID()}, nil)
	req.HTTPMethod = "POST"
	resp, _ := s.HandleUpload(context.Background(), req, Owners{"sub-1": "primary"})
	if resp.StatusCode != 403 {
		t.Errorf("read-only token: %d", resp.StatusCode)
	}
	req.RequestContext.Authorizer["scope"] = ScopeWrite
	req.Body = strings.Repeat("x", MaxSizeUpload+1)
	if resp, _ := s.HandleUpload(context.Background(), req, Owners{"sub-1": "primary"}); resp.StatusCode != 413 {
		t.Errorf("oversize: %d", resp.StatusCode)
	}
	req.Body = "x"
	req.PathParameters["accountId"] = "Aother"
	if resp, _ := s.HandleUpload(context.Background(), req, Owners{"sub-1": "primary"}); resp.StatusCode != 404 {
		t.Errorf("foreign account: %d", resp.StatusCode)
	}
}

func TestEmailCreate(t *testing.T) {
	s, m := fixture()
	att := upload(t, s, "application/pdf", "%PDF-1 binary\x00\xff")["blobId"].(string)
	img := upload(t, s, "image/png", "\x89PNG")["blobId"].(string)
	out := run(t, s, owner, `[
		["Email/set",{"accountId":"$ACCT","create":{
			"d":{"mailboxIds":{"drafts":true},"keywords":{"$draft":true,"$seen":true},
				"from":[{"name":"Jörg Müller","email":"j@example.com"}],"to":[{"name":null,"email":"bob@example.net"}],
				"subject":"Grüße — a fairly long subject line that must be folded across lines","inReplyTo":["a@example.net"],
				"header:X-Probe:asText":"ok",
				"bodyValues":{"t":{"value":"Hallo\nzweite Zeile"},"h":{"value":"<p>Hallo <img src=\"cid:i1\"></p>"}},
				"textBody":[{"partId":"t","type":"text/plain"}],"htmlBody":[{"partId":"h","type":"text/html"}],
				"attachments":[{"blobId":"`+att+`","type":"application/pdf","name":"Bericht ä.pdf","disposition":"attachment"},
					{"blobId":"`+img+`","type":"image/png","cid":"i1","disposition":"inline"}]},
			"bad1":{"mailboxIds":{"drafts":true},"id":"x"},
			"bad2":{"mailboxIds":{"drafts":true},"header:Content-Type":"text/plain"},
			"bad3":{"mailboxIds":{"drafts":true},"bodyValues":{"t":{"value":"x"}},"textBody":[{"partId":"t","type":"text/plain"}],"bodyStructure":{"partId":"t"}},
			"bad4":{"mailboxIds":{"drafts":true},"bodyStructure":{"blobId":"Uzzzzzzzzzzzzzzzzzzzzzzzz"}},
			"bad5":{"mailboxIds":{"drafts":true},"bodyStructure":{"partId":"t","header:Content-Transfer-Encoding":"base64"},"bodyValues":{"t":{"value":"x"}}}}},"0"],
		["Email/get",{"accountId":"$ACCT","ids":["#d"],"properties":["from","subject","messageId","inReplyTo","textBody","htmlBody","attachments","bodyValues","threadId","header:X-Probe:asText"],"fetchAllBodyValues":true},"1"]]`)
	r := result(t, out, 0, "Email/set")
	nc := r["notCreated"].(map[string]any)
	for _, k := range []string{"bad1", "bad2", "bad3", "bad4", "bad5"} {
		if nc[k] == nil {
			t.Errorf("%s must fail: %v", k, r)
		}
	}
	if nc["bad4"].(map[string]any)["type"] != "blobNotFound" {
		t.Errorf("bad4 %v", nc["bad4"])
	}
	d := r["created"].(map[string]any)["d"].(map[string]any)
	if d["threadId"] != "T"+idA[1:] {
		t.Errorf("the draft reply joins the thread: %v", d)
	}
	raw := m.raw["composed/"+d["id"].(string)]
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Errorf("line too long: %d", len(line))
		}
	}
	g := result(t, out, 1, "Email/get")["list"].([]any)[0].(map[string]any)
	from := g["from"].([]any)[0].(map[string]any)
	if from["name"] != "Jörg Müller" || g["subject"] != "Grüße — a fairly long subject line that must be folded across lines" ||
		g["inReplyTo"].([]any)[0] != "a@example.net" || len(g["messageId"].([]any)) != 1 || g["header:X-Probe:asText"] != "ok" {
		t.Errorf("headers %v", g)
	}
	if !strings.HasSuffix(g["messageId"].([]any)[0].(string), "@example.com") {
		t.Errorf("generated Message-ID %v", g["messageId"])
	}
	tb := g["textBody"].([]any)[0].(map[string]any)
	bv := g["bodyValues"].(map[string]any)[tb["partId"].(string)].(map[string]any)
	if v := bv["value"].(string); strings.ReplaceAll(v, "\r\n", "\n") != "Hallo\nzweite Zeile" {
		t.Errorf("text %q", bv["value"])
	}
	atts := g["attachments"].([]any)
	if len(atts) != 2 {
		t.Fatalf("attachments %v", atts)
	}
	pdf := atts[0].(map[string]any)
	if pdf["type"] != "application/pdf" {
		pdf = atts[1].(map[string]any)
	}
	if pdf["name"] != "Bericht ä.pdf" || pdf["type"] != "application/pdf" {
		t.Errorf("pdf %v", pdf)
	}
	b, _, _ := s.readBlob(context.Background(), pdf["blobId"].(string))
	if string(b.data) != "%PDF-1 binary\x00\xff" {
		t.Errorf("attachment bytes %q", b.data)
	}
	if msg := mailparse.Parse(raw); msg.Body.Type != "multipart/mixed" || msg.Body.SubParts[0].Type != "multipart/alternative" ||
		msg.Body.SubParts[0].SubParts[1].Type != "multipart/related" {
		t.Errorf("structure %s", raw)
	}
}

func TestCopyMethods(t *testing.T) {
	s, _ := fixture()
	out := run(t, s, owner, `[
		["Blob/copy",{"fromAccountId":"$ACCT","accountId":"$ACCT","blobIds":["Gx"]},"0"],
		["Blob/copy",{"fromAccountId":"Aother","accountId":"$ACCT","blobIds":["Gx"]},"1"],
		["Email/copy",{"fromAccountId":"Aother","accountId":"$ACCT","create":{}},"2"]]`)
	if errType(t, out, 0) != "invalidArguments" || errType(t, out, 1) != "fromAccountNotFound" || errType(t, out, 2) != "fromAccountNotFound" {
		t.Errorf("copy %v", out)
	}
}
