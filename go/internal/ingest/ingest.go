package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// ParserVersion identifies the projection rules; owner reprocessing with a
// newer parser does not duplicate the receipt.
const ParserVersion = 1

// ProcessingDeadline bounds the work for one delivery.
const ProcessingDeadline = 60 * time.Second

// Email is one inbound delivery as committed to the mail table.
type Email struct {
	ID       string
	ThreadID string
	Mailbox  string
	Reason   string // why it is not in the Inbox, or a diagnostic code

	SESMessageID string
	Bucket       string
	Key          string
	VersionID    string
	Size         int64

	ReceivedAt         time.Time
	EnvelopeSender     string
	EnvelopeRecipients []string // every RCPT TO SES accepted
	DeliveredTo        []string // the recipients that matched an alias
	Verdicts           Verdicts

	Headers       Headers
	ParserVersion int
}

// Outcome of committing a delivery.
type Outcome string

const (
	Committed Outcome = "committed"
	Duplicate Outcome = "duplicate" // the receipt marker already existed
)

// Originals reads delivered objects from the original bucket. It has no
// write or delete operations.
type Originals interface {
	// ResolveVersion returns the version that SES created for key; the
	// receipt notification does not carry it.
	ResolveVersion(ctx context.Context, bucket, key string) (string, error)
	// ReadPrefix returns up to n bytes from the start of that version and
	// the object's full size.
	ReadPrefix(ctx context.Context, bucket, key, version string, n int64) ([]byte, int64, error)
}

// Store commits deliveries under the receipt-marker contract
// (docs/specs/storage-sync.md#idempotency-and-durable-work).
type Store interface {
	// Seen reports whether the receipt marker exists, read strongly.
	Seen(ctx context.Context, sesMessageID string) (bool, error)
	CommitReceipt(ctx context.Context, e Email) (Outcome, error)
}

// Handler consumes the ingestion queue.
type Handler struct {
	AccountID string
	Source    Source
	Registry  Registry
	Originals Originals
	Store     Store
	Log       *slog.Logger
}

// Handle processes each SQS record independently and reports failures per
// item, so one bad delivery does not retry the batch.
func (h *Handler) Handle(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
	var resp events.SQSEventResponse
	for _, rec := range ev.Records {
		if err := h.process(ctx, rec); err != nil {
			resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: rec.MessageId})
		}
	}
	return resp, nil
}

func (h *Handler) process(ctx context.Context, rec events.SQSMessage) error {
	ctx, cancel := context.WithTimeout(ctx, ProcessingDeadline)
	defer cancel()
	log := h.Log.With("sqsMessageId", rec.MessageId, "receiveCount", rec.Attributes["ApproximateReceiveCount"])

	r, err := ParseNotification(rec.Body, h.Source)
	if err != nil {
		// Kept on the queue until it reaches the DLQ for review.
		log.Error("ingest", "outcome", "invalid", "error", err)
		return err
	}
	log = log.With("sesMessageId", r.SESMessageID)

	// Skip the S3 reads for a delivery that is already committed. The
	// commit still conditions on the marker, so this is only a shortcut.
	if seen, err := h.Store.Seen(ctx, r.SESMessageID); err != nil {
		log.Error("ingest", "outcome", "retry", "error", fmt.Errorf("read receipt marker: %w", err))
		return err
	} else if seen {
		log.Info("ingest", "outcome", string(Duplicate), "emailId", EmailID(h.AccountID, r.SESMessageID))
		return nil
	}

	e, err := h.build(ctx, r)
	if err != nil {
		log.Error("ingest", "outcome", "retry", "error", err)
		return err
	}
	out, err := h.Store.CommitReceipt(ctx, e)
	if err != nil {
		log.Error("ingest", "outcome", "retry", "emailId", e.ID, "error", err)
		return err
	}
	log.Info("ingest", "outcome", string(out), "emailId", e.ID, "mailbox", e.Mailbox,
		"reason", e.Reason, "incomplete", e.Headers.Incomplete, "size", e.Size)
	return nil
}

func (h *Handler) build(ctx context.Context, r Receipt) (Email, error) {
	id := EmailID(h.AccountID, r.SESMessageID)
	e := Email{
		ID:                 id,
		ThreadID:           "T" + id[1:],
		SESMessageID:       r.SESMessageID,
		Bucket:             r.Bucket,
		Key:                r.Key,
		ReceivedAt:         r.ReceivedAt,
		EnvelopeSender:     r.EnvelopeSender,
		EnvelopeRecipients: r.Recipients,
		DeliveredTo:        h.Registry.Route(r.Recipients),
		Verdicts:           r.Verdicts,
		ParserVersion:      ParserVersion,
	}
	e.Mailbox, e.Reason = Disposition(r.Verdicts, e.DeliveredTo)

	v, err := h.Originals.ResolveVersion(ctx, r.Bucket, r.Key)
	if err != nil {
		return Email{}, fmt.Errorf("resolve original version: %w", err)
	}
	e.VersionID = v
	prefix, size, err := h.Originals.ReadPrefix(ctx, r.Bucket, r.Key, v, MaxHeaderBytes+1)
	if err != nil {
		return Email{}, fmt.Errorf("read original: %w", err)
	}
	e.Size = size
	e.Headers, err = ParseHeaders(prefix, size > int64(len(prefix)))
	if errors.Is(err, ErrHeaderLimit) {
		// A structure limit is not retried: keep a minimal Quarantine
		// record with a diagnostic; the raw original stays in S3.
		e.Mailbox, e.Reason = Quarantine, "header-limit"
		e.Headers = Headers{Incomplete: []string{"header-limit"}}
	} else if err != nil {
		return Email{}, err
	}
	return e, nil
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// EmailID derives the stable, URL-safe JMAP id of an inbound delivery, so
// a redelivered notification resolves to the same Email.
func EmailID(accountID, sesMessageID string) string {
	sum := sha256.Sum256([]byte(accountID + "\x00" + sesMessageID))
	return "M" + strings.ToLower(idEncoding.EncodeToString(sum[:15]))
}
