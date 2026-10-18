// Package agenttrigger starts the mail agent's event-driven runs
// (docs/specs/agent.md#architecture): one triage run per EmailReceived
// event from the agent's queue, and the scheduled digest. It holds no mail
// access; the runtime reads mail through JMAP with the owner's tokens.
//
// Each event's session id derives from its event id, and the runtime keeps
// one triage row per email, so a duplicate or replayed message, including a
// DLQ redrive, is harmless. A run that does not succeed fails its message,
// which SQS retries and finally moves to the DLQ.
package agenttrigger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/zero11-0203/s3mail/go/internal/agentcli"
	"github.com/zero11-0203/s3mail/go/internal/dispatch"
)

// Ids as s3mail emits them: JMAP ids and <emailId>:received.
var (
	emailID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	eventID = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,300}$`)
)

// Invoker runs one payload on a runtime session.
type Invoker interface {
	Invoke(ctx context.Context, payload any, session string) (agentcli.Result, error)
}

// Trigger turns queue messages and schedules into runtime invocations.
type Trigger struct {
	Agent Invoker
	Log   *slog.Logger
	Now   func() time.Time
}

// envelope is the EventBridge event an SQS target delivers as the body.
type envelope struct {
	Source     string               `json:"source"`
	DetailType string               `json:"detail-type"`
	Detail     dispatch.EventDetail `json:"detail"`
}

// SessionID is the runtime session of one triage run: a hash of the event
// id, so the same event always lands on the same session.
func SessionID(event string) string {
	sum := sha256.Sum256([]byte(event))
	return "triage-" + hex.EncodeToString(sum[:])
}

// HandleSQS triages each message and reports the ones to retry.
func (t *Trigger) HandleSQS(ctx context.Context, batch events.SQSEvent) (events.SQSEventResponse, error) {
	var resp events.SQSEventResponse
	counts := map[string]int{}
	for _, m := range batch.Records {
		outcome := t.triage(ctx, m.Body)
		counts[outcome]++
		if outcome == "retry" {
			resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: m.MessageId})
		}
	}
	// Content-free: outcomes and counts only.
	t.Log.Info("triage batch", "messages", len(batch.Records), "stored", counts["stored"], "duplicate", counts["duplicate"], "skipped", counts["skipped"], "dropped", counts["dropped"], "retry", counts["retry"])
	return resp, nil
}

func (t *Trigger) triage(ctx context.Context, body string) string {
	var e envelope
	if err := json.Unmarshal([]byte(body), &e); err != nil || e.Source != dispatch.EventSource || e.DetailType != "EmailReceived" {
		// Not an event this queue's rule matches: retrying cannot help.
		t.Log.Warn("unexpected message dropped")
		return "dropped"
	}
	d := e.Detail
	if d.Version != 1 || !emailID.MatchString(d.EmailID) || !eventID.MatchString(d.EventID) {
		t.Log.Warn("malformed event dropped")
		return "dropped"
	}
	res, err := t.Agent.Invoke(ctx, map[string]any{
		"mode":  "triage",
		"event": map[string]string{"eventId": d.EventID, "emailId": d.EmailID},
	}, SessionID(d.EventID))
	if err != nil {
		t.Log.Error("triage invoke failed", "error", err)
		return "retry"
	}
	if res.Status != "ok" {
		// authorization_required, budget or JMAP failures: retried, then
		// held in the DLQ, which the agent dashboard shows, for a redrive.
		t.Log.Error("triage failed", "status", res.Status, "code", res.Error)
		return "retry"
	}
	switch res.Triage {
	case "stored", "duplicate":
		return res.Triage
	default:
		return "skipped" // the email was destroyed before triage
	}
}

// Schedule is the scheduled invocation's input.
type Schedule struct {
	Digest string `json:"digest"` // "mail": deliver the daily digest
}

// HandleSchedule runs the daily digest, delivered as an email (A2).
func (t *Trigger) HandleSchedule(ctx context.Context, s Schedule) error {
	if s.Digest != "mail" {
		return fmt.Errorf("unsupported schedule %q", s.Digest)
	}
	day := t.Now().UTC().Format("2006-01-02")
	res, err := t.Agent.Invoke(ctx, map[string]any{"mode": "digest", "deliver": "mail"}, SessionID("digest:"+day))
	if err != nil {
		return err
	}
	if err := agentcli.Check(res); err != nil {
		t.Log.Error("digest failed", "status", res.Status, "code", res.Error)
		return err
	}
	t.Log.Info("digest delivered")
	return nil
}
