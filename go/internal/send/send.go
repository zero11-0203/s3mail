// Package send is the shared submission code
// (docs/specs/mail-flow.md#outbound-email): quota reservation, the attempt
// state machine, the submission worker and the work-recovery sweep. Every
// transition is a guarded commit; the relay adapter only transmits.
package send

import (
	"fmt"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/relay"
)

// Policy is the active relay's configuration: its name and accounting
// (docs/specs/mail-flow.md#outbound-relay-interface).
type Policy struct {
	Relay string
	// Daily and Monthly caps in Units; zero disables a cap.
	Daily, Monthly int64
	// PerRecipient counts each envelope recipient as one unit; otherwise
	// each message is one unit.
	PerRecipient bool
	// Location is the provider's reset timezone (UTC when nil).
	Location *time.Location
}

func (p Policy) loc() *time.Location {
	if p.Location == nil {
		return time.UTC
	}
	return p.Location
}

// Reserve returns the reservation for a new submission and the quota
// deltas that make it, in the period of now.
func (p Policy) Reserve(now time.Time, recipients int) (model.Reservation, []model.QuotaDelta) {
	units := int64(1)
	if p.PerRecipient {
		units = int64(recipients)
	}
	t := now.In(p.loc())
	r := model.Reservation{Units: units}
	var deltas []model.QuotaDelta
	for _, c := range []struct {
		key   string
		limit int64
	}{
		{fmt.Sprintf("%s#day#%s", p.Relay, t.Format("2006-01-02")), p.Daily},
		{fmt.Sprintf("%s#month#%s", p.Relay, t.Format("2006-01")), p.Monthly},
	} {
		if c.limit <= 0 {
			continue
		}
		r.Keys = append(r.Keys, c.key)
		deltas = append(deltas, model.QuotaDelta{Key: c.key, Units: units, Limit: c.limit})
	}
	return r, deltas
}

// Release returns the deltas that give a reservation back; it is used
// only after proven non-acceptance or cancellation.
func Release(r model.Reservation) []model.QuotaDelta {
	var out []model.QuotaDelta
	for _, k := range r.Keys {
		out = append(out, model.QuotaDelta{Key: k, Units: -r.Units})
	}
	return out
}

// Attempt record numbers within one attempt.
const (
	recClaimed = iota + 1
	recHandoff
	recResult
	recRecovered
	recCanceled
	recResolved
)

// Lease and retry timing.
const (
	Lease       = 2 * time.Minute
	MaxAttempts = 5
)

// retryDelay is the wait before attempt n+1 after a proven non-acceptance.
func retryDelay(n int) time.Duration {
	switch n {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	}
	return time.Hour
}

func attempt(sub *model.Submission, rec int, phase, owner string, now time.Time, detail string) model.Attempt {
	return model.Attempt{SubmissionID: sub.ID, Attempt: sub.Attempt, Seq: rec, Phase: phase, Owner: owner, At: now, Detail: detail}
}

// Claim takes a queued, due submission for owner under a lease.
func Claim(sub model.Submission, owner string, now time.Time) (*model.Mutation, bool) {
	if sub.Status != model.SubmissionQueued || sub.NextAttemptAt.After(now) {
		return nil, false
	}
	before := sub
	after := sub.Clone()
	after.Status, after.Attempt, after.Owner = model.SubmissionActive, sub.Attempt+1, owner
	after.LeaseUntil, after.Handoff = now.Add(Lease), false
	return &model.Mutation{
		Submissions: []model.SubmissionChange{{Before: &before, After: &after}},
		Attempts:    []model.Attempt{attempt(&after, recClaimed, "claimed", owner, now, "")},
	}, true
}

// Handoff records handoff_started; it is the last step before anything
// could transmit DATA and requires the owner's live lease.
func Handoff(sub model.Submission, owner string, attemptN int, now time.Time) (*model.Mutation, error) {
	switch {
	case sub.Status != model.SubmissionActive:
		return nil, fmt.Errorf("submission is %s", sub.Status)
	case sub.Owner != owner || sub.Attempt != attemptN:
		return nil, fmt.Errorf("attempt is owned elsewhere")
	case !now.Before(sub.LeaseUntil):
		return nil, fmt.Errorf("lease expired")
	case sub.Handoff:
		return nil, nil
	}
	before := sub
	after := sub.Clone()
	after.Handoff = true
	return &model.Mutation{
		Submissions: []model.SubmissionChange{{Before: &before, After: &after}},
		Attempts:    []model.Attempt{attempt(&after, recHandoff, "handoff_started", owner, now, "")},
	}, nil
}

// Finish records an attempt's result. Only the attempt's owner completes
// it; after a lease expiry turned it into unknown, the owner's evidence
// still resolves it. requeue is the delay before the next attempt, or -1.
func Finish(sub model.Submission, owner string, attemptN int, res relay.Result, now time.Time) (m *model.Mutation, requeue time.Duration) {
	if sub.Owner != owner || sub.Attempt != attemptN {
		return nil, -1
	}
	if sub.Status != model.SubmissionActive && (sub.Status != model.SubmissionUnknown || res.Outcome == relay.Unknown) {
		return nil, -1
	}
	before := sub
	after := sub.Clone()
	after.Owner = owner
	requeue = -1
	var release []model.QuotaDelta
	detail := res.Stage + ": " + res.Reply
	switch res.Outcome {
	case relay.Accepted:
		// Relay acceptance is not destination delivery: "queued" until a
		// delivery report says otherwise.
		after.Status = model.SubmissionAccepted
		after.SetRecipients("queued", res.Reply)
	case relay.Unknown:
		after.Status = model.SubmissionUnknown
		after.SetRecipients("unknown", res.Reply)
	case relay.Rejected:
		after.Status = model.SubmissionRejected
		after.SetRecipients("no", res.Reply)
		release = Release(sub.Reservation)
	default:
		if sub.Attempt >= MaxAttempts {
			after.Status = model.SubmissionRejected
			after.SetRecipients("no", res.Reply)
			release = Release(sub.Reservation)
			detail += " (attempts exhausted)"
			break
		}
		// Proven not accepted: retry later with the same reservation.
		requeue = retryDelay(sub.Attempt)
		after.Status, after.Handoff, after.NextAttemptAt = model.SubmissionQueued, false, now.Add(requeue)
		after.SetRecipients("queued", res.Reply)
	}
	for rcpt, r := range res.Recipients {
		if v, ok := after.Recipients[rcpt]; ok {
			v.SMTPReply = r
			after.Recipients[rcpt] = v
		}
	}
	return &model.Mutation{
		Submissions: []model.SubmissionChange{{Before: &before, After: &after}},
		Quota:       release,
		Attempts:    []model.Attempt{attempt(&after, recResult, res.Outcome.String(), owner, now, detail)},
	}, requeue
}

// Recover handles an expired lease: before handoff the attempt is safely
// retryable; after handoff its acceptance is unknown and it is never
// resent automatically.
func Recover(sub model.Submission, now time.Time) *model.Mutation {
	if sub.Status != model.SubmissionActive || now.Before(sub.LeaseUntil) {
		return nil
	}
	before := sub
	after := sub.Clone()
	var phase string
	if sub.Handoff {
		after.Status = model.SubmissionUnknown
		after.SetRecipients("unknown", "lease expired after handoff; acceptance unknown")
		phase = "lease-expired-unknown"
	} else {
		after.Status, after.NextAttemptAt = model.SubmissionQueued, now
		phase = "lease-expired-retry"
	}
	return &model.Mutation{
		Submissions: []model.SubmissionChange{{Before: &before, After: &after}},
		Attempts:    []model.Attempt{attempt(&after, recRecovered, phase, sub.Owner, now, "")},
	}
}

// Cancel cancels a submission before handoff and releases its quota.
func Cancel(sub model.Submission, now time.Time) (*model.Mutation, bool) {
	if sub.UndoStatus() != "pending" {
		return nil, false
	}
	before := sub
	after := sub.Clone()
	after.Status = model.SubmissionCanceled
	after.SetRecipients("no", "canceled before sending")
	return &model.Mutation{
		Submissions: []model.SubmissionChange{{Before: &before, After: &after}},
		Quota:       Release(sub.Reservation),
		Attempts:    []model.Attempt{attempt(&after, recCanceled, "canceled", "", now, "")},
	}, true
}

// Resolve is the owner's reconciliation of an unknown submission after
// checking the relay: "accepted", "rejected" (not accepted, releasing the
// quota) or "retry" (explicitly authorizing a new attempt).
func Resolve(sub model.Submission, outcome string, now time.Time) (*model.Mutation, error) {
	if sub.Status != model.SubmissionUnknown {
		return nil, fmt.Errorf("submission is %s, not unknown", sub.Status)
	}
	before := sub
	after := sub.Clone()
	m := &model.Mutation{Submissions: []model.SubmissionChange{{Before: &before, After: &after}}}
	switch outcome {
	case "accepted":
		after.Status = model.SubmissionAccepted
		after.SetRecipients("queued", "relay acceptance confirmed by the owner")
	case "rejected":
		after.Status = model.SubmissionRejected
		after.SetRecipients("no", "non-acceptance confirmed by the owner")
		m.Quota = Release(sub.Reservation)
	case "retry":
		after.Status, after.Handoff, after.NextAttemptAt = model.SubmissionQueued, false, now
		after.SetRecipients("queued", "new attempt authorized by the owner")
		m.Send = []string{sub.ID}
	default:
		return nil, fmt.Errorf("unknown resolution %q", outcome)
	}
	m.Attempts = []model.Attempt{attempt(&after, recResolved, "owner-"+outcome, "owner", now, "")}
	return m, nil
}
