package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

var testSource = Source{
	TopicArn:  "arn:aws:sns:us-east-1:111111111111:s3mail-inbound-receipts",
	Bucket:    "s3mail-originals-test",
	KeyPrefix: "inbound/",
}

const testSESID = "s2examplemessageid0000000000000000000001"

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/receipt-sns.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mutate rewrites the inner SES notification of the fixture.
func mutate(t *testing.T, f func(ses map[string]any)) string {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(fixture(t)), &env); err != nil {
		t.Fatal(err)
	}
	var ses map[string]any
	if err := json.Unmarshal([]byte(env["Message"].(string)), &ses); err != nil {
		t.Fatal(err)
	}
	f(ses)
	inner, _ := json.Marshal(ses)
	env["Message"] = string(inner)
	out, _ := json.Marshal(env)
	return string(out)
}

func action(ses map[string]any) map[string]any {
	return ses["receipt"].(map[string]any)["action"].(map[string]any)
}

func TestParseNotification(t *testing.T) {
	r, err := ParseNotification(fixture(t), testSource)
	if err != nil {
		t.Fatal(err)
	}
	if r.SESMessageID != testSESID || r.Key != "inbound/"+testSESID || r.Bucket != testSource.Bucket {
		t.Errorf("location = %+v", r)
	}
	if !slices.Equal(r.Recipients, []string{"alice+news@example.com", "bob@example.com"}) {
		t.Errorf("envelope recipients = %v", r.Recipients)
	}
	if r.EnvelopeSender != "probe@example.net" {
		t.Errorf("envelope sender = %q", r.EnvelopeSender)
	}
	want := Verdicts{Spam: "PASS", Virus: "PASS", SPF: "FAIL", DKIM: "GRAY", DMARC: "FAIL", DMARCPolicy: "REJECT"}
	if r.Verdicts != want {
		t.Errorf("verdicts = %+v, want %+v", r.Verdicts, want)
	}
	if r.ReceivedAt.IsZero() || r.ReceivedAt.Location().String() != "UTC" {
		t.Errorf("receivedAt = %v", r.ReceivedAt)
	}
}

func TestParseNotificationRejects(t *testing.T) {
	foreignTopic := strings.Replace(fixture(t), `"TopicArn": "arn:aws:sns:us-east-1:111111111111:s3mail-inbound-receipts"`, `"TopicArn": "arn:aws:sns:us-east-1:222222222222:other"`, 1)
	cases := map[string]string{
		"not JSON":       "{",
		"foreign topic":  foreignTopic,
		"bounce":         mutate(t, func(s map[string]any) { s["notificationType"] = "Bounce" }),
		"foreign bucket": mutate(t, func(s map[string]any) { action(s)["bucketName"] = "other" }),
		"not S3 action":  mutate(t, func(s map[string]any) { action(s)["type"] = "SNS" }),
		"key mismatch":   mutate(t, func(s map[string]any) { action(s)["objectKey"] = "inbound/../other" }),
		"bad message id": mutate(t, func(s map[string]any) { s["mail"].(map[string]any)["messageId"] = "../x" }),
		"no recipients":  mutate(t, func(s map[string]any) { s["receipt"].(map[string]any)["recipients"] = []string{} }),
	}
	for name, body := range cases {
		if _, err := ParseNotification(body, testSource); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestRoute(t *testing.T) {
	reg := Registry{Domain: "example.com", Aliases: []string{"alice", "canary"}}
	got := reg.Route([]string{
		"alice@example.com", "Alice+News@Example.com", "alice@example.com",
		"bob@example.com", "alice@example.net", "not-an-address",
	})
	want := []string{"alice@example.com", "Alice+News@Example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("Route = %v, want %v (exact delivered addresses, deduplicated)", got, want)
	}
}

func TestDisposition(t *testing.T) {
	clean := Verdicts{Spam: "PASS", Virus: "PASS", SPF: "PASS", DKIM: "PASS", DMARC: "PASS"}
	with := func(f func(*Verdicts)) Verdicts { v := clean; f(&v); return v }
	alias := []string{"alice@example.com"}
	cases := []struct {
		name    string
		v       Verdicts
		matched []string
		mailbox string
		reason  string
	}{
		{"clean", clean, alias, Inbox, ""},
		{"spf and dkim gray do not matter", with(func(v *Verdicts) { v.SPF, v.DKIM = "GRAY", "GRAY" }), alias, Inbox, ""},
		{"unmatched recipient", clean, nil, Quarantine, "unmatched-recipient"},
		{"virus", with(func(v *Verdicts) { v.Virus = "FAIL"; v.Spam = "FAIL" }), alias, Quarantine, "virus"},
		{"virus scan failed", with(func(v *Verdicts) { v.Virus = "PROCESSING_FAILED" }), alias, Quarantine, "verdict-unknown"},
		{"spam gray", with(func(v *Verdicts) { v.Spam = "GRAY" }), alias, Quarantine, "verdict-unknown"},
		{"dmarc reject", with(func(v *Verdicts) { v.DMARC, v.DMARCPolicy = "FAIL", "REJECT" }), alias, Quarantine, "dmarc"},
		{"dmarc quarantine", with(func(v *Verdicts) { v.DMARC, v.DMARCPolicy = "FAIL", "QUARANTINE" }), alias, Quarantine, "dmarc"},
		{"dmarc none", with(func(v *Verdicts) { v.DMARC, v.DMARCPolicy = "FAIL", "NONE" }), alias, Inbox, ""},
		{"spam", with(func(v *Verdicts) { v.Spam = "FAIL" }), alias, Junk, "spam"},
		{"canary only", clean, []string{"canary+run1@example.com"}, Quarantine, "canary"},
		{"canary with an alias", clean, []string{"canary@example.com", "alice@example.com"}, Inbox, ""},
	}
	for _, c := range cases {
		mb, reason := Disposition(c.v, c.matched)
		if mb != c.mailbox || reason != c.reason {
			t.Errorf("%s: got %s/%q, want %s/%q", c.name, mb, reason, c.mailbox, c.reason)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	msg := "From: =?UTF-8?B?w4Rsw6s=?= <ale@example.net>\r\n" +
		"To: alice@example.com, \"Bob\" <bob@example.com>\r\n" +
		"Subject: =?utf-8?q?caf=C3=A9?= menu\r\n" +
		"Date: Sat, 17 Oct 2026 15:40:12 +0900\r\n" +
		"Message-ID: <m1@example.net>\r\n" +
		"References: <a@example.net>\r\n <b@example.net>\r\n" +
		"\r\nbody\r\n"
	h, err := ParseHeaders([]byte(msg), false)
	if err != nil {
		t.Fatal(err)
	}
	if h.Subject != "café menu" {
		t.Errorf("subject = %q", h.Subject)
	}
	if len(h.From) != 1 || !strings.Contains(h.From[0], "ale@example.net") {
		t.Errorf("from = %v", h.From)
	}
	if len(h.To) != 2 {
		t.Errorf("to = %v", h.To)
	}
	if h.Date.Format("2006-01-02T15:04:05Z07:00") != "2026-10-17T06:40:12Z" {
		t.Errorf("date = %v", h.Date)
	}
	if h.MessageID != "<m1@example.net>" || h.References != "<a@example.net> <b@example.net>" {
		t.Errorf("ids = %q / %q", h.MessageID, h.References)
	}
	if len(h.Incomplete) != 0 {
		t.Errorf("incomplete = %v", h.Incomplete)
	}
}

func TestParseHeadersBounds(t *testing.T) {
	t.Run("legacy charset kept raw", func(t *testing.T) {
		h, _ := ParseHeaders([]byte("Subject: =?Shift_JIS?B?gmCCYA==?=\n\nx"), false)
		if h.Subject != "=?Shift_JIS?B?gmCCYA==?=" || !slices.Contains(h.Incomplete, "charset") {
			t.Errorf("subject %q incomplete %v", h.Subject, h.Incomplete)
		}
	})
	t.Run("header block over the limit", func(t *testing.T) {
		long := "X-Pad: " + strings.Repeat("a", MaxHeaderBytes) + "\r\n"
		if _, err := ParseHeaders([]byte(long[:MaxHeaderBytes+1]), true); !errors.Is(err, ErrHeaderLimit) {
			t.Errorf("err = %v, want ErrHeaderLimit", err)
		}
	})
	t.Run("headers only", func(t *testing.T) {
		h, err := ParseHeaders([]byte("Subject: hi\r\n"), false)
		if err != nil || h.Subject != "hi" {
			t.Errorf("subject %q err %v", h.Subject, err)
		}
	})
	t.Run("address count", func(t *testing.T) {
		var to []string
		for range MaxAddresses + 5 {
			to = append(to, "x@example.com")
		}
		h, _ := ParseHeaders([]byte("To: "+strings.Join(to, ", ")+"\n\n"), false)
		if len(h.To) != MaxAddresses || !slices.Contains(h.Incomplete, "address-count") {
			t.Errorf("to %d incomplete %v", len(h.To), h.Incomplete)
		}
	})
	t.Run("long subject", func(t *testing.T) {
		h, _ := ParseHeaders([]byte("Subject: "+strings.Repeat("é", maxSubjectBytes)+"\n\n"), false)
		if len(h.Subject) > maxSubjectBytes || !slices.Contains(h.Incomplete, "subject-length") {
			t.Errorf("subject %d bytes incomplete %v", len(h.Subject), h.Incomplete)
		}
	})
}

func TestEmailID(t *testing.T) {
	a, b := EmailID("primary", testSESID), EmailID("primary", testSESID)
	if a != b {
		t.Error("EmailID is not stable")
	}
	if a == EmailID("other", testSESID) {
		t.Error("EmailID ignores the account")
	}
	if !regexp.MustCompile(`^M[a-z2-7]{24}$`).MatchString(a) {
		t.Errorf("EmailID %q is not a URL-safe JMAP id", a)
	}
}

type fakeOriginals struct {
	data  []byte
	calls int
}

func (f *fakeOriginals) ResolveVersion(context.Context, string, string) (string, error) {
	f.calls++
	return "v1", nil
}

func (f *fakeOriginals) ReadPrefix(_ context.Context, _, _, _ string, n int64) ([]byte, int64, error) {
	f.calls++
	return f.data[:min(int64(len(f.data)), n)], int64(len(f.data)), nil
}

type fakeStore struct {
	seen      bool
	err       error
	committed []Email
}

func (f *fakeStore) Seen(context.Context, string) (bool, error) { return f.seen, nil }

func (f *fakeStore) CommitReceipt(_ context.Context, e Email) (Outcome, error) {
	if f.err != nil {
		return "", f.err
	}
	f.committed = append(f.committed, e)
	return Committed, nil
}

func newHandler(o Originals, s Store) *Handler {
	return &Handler{
		AccountID: "primary",
		Source:    testSource,
		Registry:  Registry{Domain: "example.com", Aliases: []string{"alice", "canary"}},
		Originals: o,
		Store:     s,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func event(bodies ...string) events.SQSEvent {
	var ev events.SQSEvent
	for i, b := range bodies {
		ev.Records = append(ev.Records, events.SQSMessage{MessageId: string(rune('a' + i)), Body: b})
	}
	return ev
}

func TestHandleCommits(t *testing.T) {
	o := &fakeOriginals{data: []byte("Subject: hello\r\n\r\nbody")}
	s := &fakeStore{}
	resp, err := newHandler(o, s).Handle(context.Background(), event(fixture(t)))
	if err != nil || len(resp.BatchItemFailures) != 0 {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if len(s.committed) != 1 {
		t.Fatalf("committed %d", len(s.committed))
	}
	e := s.committed[0]
	if e.ID != EmailID("primary", testSESID) || e.VersionID != "v1" || e.Size != int64(len(o.data)) {
		t.Errorf("email = %+v", e)
	}
	// The fixture's sender fails DMARC with p=reject.
	if e.Mailbox != Quarantine || e.Reason != "dmarc" {
		t.Errorf("disposition = %s/%s", e.Mailbox, e.Reason)
	}
	if !slices.Equal(e.DeliveredTo, []string{"alice+news@example.com"}) || len(e.EnvelopeRecipients) != 2 {
		t.Errorf("delivered %v envelope %v", e.DeliveredTo, e.EnvelopeRecipients)
	}
	if e.Headers.Subject != "hello" {
		t.Errorf("subject = %q", e.Headers.Subject)
	}
}

func TestHandleDuplicateSkipsReads(t *testing.T) {
	o := &fakeOriginals{}
	s := &fakeStore{seen: true}
	resp, _ := newHandler(o, s).Handle(context.Background(), event(fixture(t)))
	if len(resp.BatchItemFailures) != 0 || o.calls != 0 || len(s.committed) != 0 {
		t.Errorf("failures %v, S3 calls %d, commits %d", resp.BatchItemFailures, o.calls, len(s.committed))
	}
}

func TestHandleReportsFailuresPerItem(t *testing.T) {
	o := &fakeOriginals{data: []byte("Subject: x\r\n\r\n")}
	s := &fakeStore{}
	resp, _ := newHandler(o, s).Handle(context.Background(), event("{", fixture(t)))
	if len(resp.BatchItemFailures) != 1 || resp.BatchItemFailures[0].ItemIdentifier != "a" {
		t.Errorf("failures = %+v", resp.BatchItemFailures)
	}
	if len(s.committed) != 1 {
		t.Errorf("the valid record was not committed")
	}

	s.err = errors.New("throttled")
	resp, _ = newHandler(o, s).Handle(context.Background(), event(fixture(t)))
	if len(resp.BatchItemFailures) != 1 {
		t.Errorf("a store error must be retried, failures = %+v", resp.BatchItemFailures)
	}
}

func TestHandleHeaderLimitQuarantines(t *testing.T) {
	o := &fakeOriginals{data: []byte("X-Pad: " + strings.Repeat("a", MaxHeaderBytes+10))}
	s := &fakeStore{}
	body := mutate(t, func(ses map[string]any) {
		ses["receipt"].(map[string]any)["dmarcVerdict"] = map[string]string{"status": "PASS"}
	})
	resp, _ := newHandler(o, s).Handle(context.Background(), event(body))
	if len(resp.BatchItemFailures) != 0 || len(s.committed) != 1 {
		t.Fatalf("a structure limit must not be retried: %+v", resp)
	}
	if e := s.committed[0]; e.Mailbox != Quarantine || e.Reason != "header-limit" {
		t.Errorf("disposition = %s/%s", e.Mailbox, e.Reason)
	}
}
