package agenttrigger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/zero11-0203/s3mail/go/internal/agentcli"
	"github.com/zero11-0203/s3mail/go/internal/dispatch"
)

type call struct {
	payload map[string]any
	session string
}

type fakeAgent struct {
	calls   []call
	results []agentcli.Result
	err     error
}

func (f *fakeAgent) Invoke(_ context.Context, payload any, session string) (agentcli.Result, error) {
	raw, _ := json.Marshal(payload)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	f.calls = append(f.calls, call{p, session})
	if f.err != nil {
		return agentcli.Result{}, f.err
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r, nil
}

func body(t *testing.T, emailID, eventID string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"source":      dispatch.EventSource,
		"detail-type": "EmailReceived",
		"detail":      dispatch.EventDetail{Version: 1, AccountID: "a1", EventID: eventID, EmailID: emailID, MailboxIDs: []string{"inbox"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func trigger(f *fakeAgent) *Trigger {
	return &Trigger{Agent: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC) }}
}

func TestTriagesEachEventOnItsOwnSession(t *testing.T) {
	f := &fakeAgent{results: []agentcli.Result{
		{Status: "ok", Triage: "stored"},
		{Status: "ok", Triage: "duplicate"},
		{Status: "error", Error: "authorization_required"},
	}}
	batch := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "m1", Body: body(t, "Ma", "Ma:received")},
		{MessageId: "m2", Body: body(t, "Ma", "Ma:received")},
		{MessageId: "m3", Body: body(t, "Mb", "Mb:received")},
		{MessageId: "m4", Body: `{"source":"other","detail-type":"EmailReceived"}`},
		{MessageId: "m5", Body: body(t, "../x", "x:received")},
	}}
	resp, err := trigger(f).HandleSQS(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	// Only the failed run is retried; malformed messages are dropped.
	if len(resp.BatchItemFailures) != 1 || resp.BatchItemFailures[0].ItemIdentifier != "m3" {
		t.Fatalf("failures %+v", resp.BatchItemFailures)
	}
	if len(f.calls) != 3 || f.calls[0].session != f.calls[1].session || f.calls[0].session == f.calls[2].session {
		t.Fatalf("calls %+v", f.calls)
	}
	if !strings.HasPrefix(f.calls[0].session, "triage-") || len(f.calls[0].session) < 33 {
		t.Fatalf("session %q", f.calls[0].session)
	}
	event := f.calls[0].payload["event"].(map[string]any)
	if f.calls[0].payload["mode"] != "triage" || event["emailId"] != "Ma" || event["eventId"] != "Ma:received" {
		t.Fatalf("payload %v", f.calls[0].payload)
	}
}

func TestInvokeErrorsAreRetried(t *testing.T) {
	f := &fakeAgent{err: errors.New("throttled")}
	resp, _ := trigger(f).HandleSQS(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "m1", Body: body(t, "Ma", "Ma:received")}}})
	if len(resp.BatchItemFailures) != 1 {
		t.Fatalf("failures %+v", resp.BatchItemFailures)
	}
}

func TestScheduledDigestIsDeliveredAsMail(t *testing.T) {
	f := &fakeAgent{results: []agentcli.Result{{Status: "ok", EmailID: "D1"}, {Status: "error", Error: "scope_not_granted"}}}
	tr := trigger(f)
	if err := tr.HandleSchedule(context.Background(), Schedule{Digest: "mail"}); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].payload["mode"] != "digest" || f.calls[0].payload["deliver"] != "mail" || f.calls[0].session != SessionID("digest:2026-10-18") {
		t.Fatalf("call %+v", f.calls[0])
	}
	if err := tr.HandleSchedule(context.Background(), Schedule{Digest: "mail"}); err == nil {
		t.Fatal("a failed digest was not reported")
	}
	if err := tr.HandleSchedule(context.Background(), Schedule{}); err == nil {
		t.Fatal("an unknown schedule ran")
	}
}
