package send

import (
	"context"
	"errors"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/relay"
)

// RelayIndex finds the submission a relay accepted under its queue id.
type RelayIndex interface {
	SubmissionByRelayID(ctx context.Context, relay, id string) (string, bool, error)
}

// ErrUncorrelated means no submission is known under the event's relay
// id yet; the event stays queued and is retried.
var ErrUncorrelated = errors.New("no submission for the relay id")

// ApplyEvent applies one relay delivery report with a guarded commit. A
// report already applied, or one that would not move the recipient's
// state forward, is a successful no-op.
func ApplyEvent(ctx context.Context, st Store, idx RelayIndex, ev relay.DeliveryEvent) (bool, error) {
	id, ok, err := idx.SubmissionByRelayID(ctx, ev.Relay, ev.RelayID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrUncorrelated
	}
	_, applied, err := commit(ctx, st, id, func(s model.Submission) (*model.Mutation, error) {
		return ApplyDelivery(s, ev), nil
	})
	if errors.Is(err, model.ErrDuplicateEvent) {
		return false, nil
	}
	return applied, err
}
