package send

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/relay"
)

// Store is the account access the worker and recovery need.
type Store interface {
	State(ctx context.Context) (model.State, error)
	Commit(ctx context.Context, prev model.State, m model.Mutation) (model.State, error)
	GetSubmission(ctx context.Context, id string) (model.Submission, bool, error)
	ListSubmissions(ctx context.Context) ([]model.Submission, error)
}

// Originals reads the frozen send material by exact version.
type Originals interface {
	Fetch(ctx context.Context, ref model.BlobRef) ([]byte, error)
}

// Config answers whether sending is paused and builds the active relay;
// both are read per invocation so pausing needs no deployment.
type Config interface {
	Paused(ctx context.Context) (bool, error)
	Relay(ctx context.Context) (relay.Relay, error)
}

// Queue schedules a submission for the worker after delay.
type Queue interface {
	Enqueue(ctx context.Context, submissionID string, delay time.Duration) error
}

// ErrBusy means the account kept changing during a commit.
var ErrBusy = errors.New("account busy")

// commit retries a guarded mutation from fresh reads; fn returning a nil
// mutation commits nothing.
func commit(ctx context.Context, st Store, id string, fn func(model.Submission) (*model.Mutation, error)) (model.Submission, bool, error) {
	for range 8 {
		state, err := st.State(ctx)
		if err != nil {
			return model.Submission{}, false, err
		}
		sub, ok, err := st.GetSubmission(ctx, id)
		if err != nil || !ok {
			return sub, false, err
		}
		m, err := fn(sub)
		if err != nil || m == nil {
			return sub, false, err
		}
		_, err = st.Commit(ctx, state, *m)
		if errors.Is(err, model.ErrStateChanged) {
			continue
		}
		if err != nil {
			return sub, false, err
		}
		return *m.Submissions[0].After, true, nil
	}
	return model.Submission{}, false, ErrBusy
}

// Worker sends queued submissions, at most once per durable submission.
type Worker struct {
	Store     Store
	Originals Originals
	Config    Config
	Queue     Queue
	Log       *slog.Logger
	Now       func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func newOwner() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Process handles one wake-up for a submission. A duplicate delivery, a
// live lease held elsewhere or a terminal submission is a no-op. An error
// means the wake-up should be retried; the send state is never left
// inconsistent.
func (w *Worker) Process(ctx context.Context, id string) error {
	log := w.Log.With("submissionId", id)
	if paused, err := w.Config.Paused(ctx); err != nil || paused {
		// Recovery re-enqueues due work once sending resumes.
		log.Info("sending paused", "error", err)
		return nil
	}
	rl, err := w.Config.Relay(ctx)
	if err != nil {
		log.Error("relay unavailable", "error", err)
		return nil
	}
	owner := newOwner()
	sub, claimed, err := commit(ctx, w.Store, id, func(s model.Submission) (*model.Mutation, error) {
		m, ok := Claim(s, owner, w.now())
		if !ok {
			return nil, nil
		}
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		log.Info("nothing to send", "status", sub.Status)
		return nil
	}
	n := sub.Attempt
	log = log.With("attempt", n)

	raw, err := w.Originals.Fetch(ctx, sub.Blob)
	var res relay.Result
	if err != nil {
		res = relay.Result{Outcome: relay.NotAccepted, Stage: "read", Reply: "frozen message unavailable"}
		log.Error("read frozen message", "error", err)
	} else {
		res = rl.Submit(ctx, relay.Message{
			Envelope: relay.Envelope{MailFrom: sub.MailFrom, RcptTo: sub.RcptTo},
			Body:     bytes.NewReader(relay.StripBcc(raw)),
			Handoff: func(ctx context.Context) error {
				_, _, err := commit(ctx, w.Store, id, func(s model.Submission) (*model.Mutation, error) {
					return Handoff(s, owner, n, w.now())
				})
				return err
			},
		})
	}
	if res.Stage == "handoff" {
		// The lease or ownership was lost before anything was sent;
		// whoever holds the submission now decides.
		log.Warn("handoff refused", "reason", res.Reply)
		return nil
	}
	var requeue time.Duration = -1
	_, _, err = commit(ctx, w.Store, id, func(s model.Submission) (*model.Mutation, error) {
		m, d := Finish(s, owner, n, res, w.now())
		requeue = d
		return m, nil
	})
	if err != nil {
		// The relay result is lost only if this commit keeps failing; the
		// expired lease then makes the attempt unknown (after handoff) or
		// retryable (before), never silently resent.
		return fmt.Errorf("record result %s: %w", res.Outcome, err)
	}
	log.Info("attempt finished", "outcome", res.Outcome.String(), "stage", res.Stage)
	if res.Outcome == relay.Unknown {
		log.Error("acceptance unknown", "outcome", "acceptance_unknown")
	}
	if requeue >= 0 && w.Queue != nil {
		if err := w.Queue.Enqueue(ctx, id, requeue); err != nil {
			// Recovery enqueues due work anyway.
			log.Warn("requeue", "error", err)
		}
	}
	return nil
}
