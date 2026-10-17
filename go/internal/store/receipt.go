package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/zero11-0203/s3mail/go/internal/ingest"
	"github.com/zero11-0203/s3mail/go/internal/model"
)

const maxCommitAttempts = 5

// ErrContention means every attempt lost the account sequence race.
var ErrContention = errors.New("account sequence contention")

// Seen reads the receipt marker strongly.
func (a *Account) Seen(ctx context.Context, sesMessageID string) (bool, error) {
	out, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            &a.Table,
		Key:                  a.key("RECEIPT#" + sesMessageID),
		ConsistentRead:       aws.Bool(true),
		ProjectionExpression: aws.String("SK"),
	})
	if err != nil {
		return false, err
	}
	return out.Item != nil, nil
}

// CommitReceipt commits a delivery under the receipt-marker contract
// (docs/specs/storage-sync.md#idempotency-and-durable-work): thread it,
// then commit the Email, marker, counters and outbox together, retrying
// sequence conflicts from a fresh read. A failed marker condition is a
// duplicate only once a strong read finds the marker.
func (a *Account) CommitReceipt(ctx context.Context, e ingest.Email) (ingest.Outcome, error) {
	email := ReceiptEmail(e)
	for attempt := range maxCommitAttempts {
		if attempt > 0 {
			if err := sleep(ctx, a.backoff(attempt)); err != nil {
				return "", err
			}
		}
		st, err := a.State(ctx)
		if err != nil {
			return "", fmt.Errorf("read state: %w", err)
		}
		email.ThreadID, err = model.AssignThread(ctx, &email, a)
		if err != nil {
			return "", fmt.Errorf("thread lookup: %w", err)
		}
		_, err = a.Commit(ctx, st, model.Mutation{
			Emails: []model.EmailChange{{After: &email}},
			Receipt: &model.Receipt{
				SESMessageID: e.SESMessageID, EmailID: e.ID, Blob: email.Blob,
				ReceivedAt: e.ReceivedAt, ParserVersion: e.ParserVersion,
			},
			Events: []model.Event{{ID: e.ID + ":received", Type: "EmailReceived", EmailID: e.ID, MailboxIDs: []string{e.Mailbox}}},
		})
		switch {
		case err == nil:
			return ingest.Committed, nil
		case errors.Is(err, model.ErrDuplicateReceipt):
			seen, serr := a.Seen(ctx, e.SESMessageID)
			if serr != nil {
				return "", fmt.Errorf("classify receipt condition: %w", serr)
			}
			if seen {
				return ingest.Duplicate, nil
			}
			return "", errors.New("receipt condition failed but no marker exists")
		case errors.Is(err, model.ErrStateChanged):
			continue
		default:
			return "", err
		}
	}
	return "", ErrContention
}

func (a *Account) backoff(attempt int) time.Duration {
	if a.Backoff != nil {
		return a.Backoff(attempt)
	}
	base := time.Duration(attempt*attempt) * 25 * time.Millisecond
	return base + rand.N(base+1)
}

// ReceiptEmail converts an ingested delivery to the stored Email.
func ReceiptEmail(e ingest.Email) model.Email {
	h := e.Headers
	return model.Email{
		ID:         e.ID,
		ThreadID:   e.ThreadID,
		MailboxIDs: map[string]bool{e.Mailbox: true},
		Keywords:   map[string]bool{},
		ReceivedAt: e.ReceivedAt,
		Size:       e.Size,
		Blob:       model.BlobRef{Bucket: e.Bucket, Key: e.Key, VersionID: e.VersionID},
		Source:     model.SourceReceipt,
		Subject:    h.Subject,
		From:       h.From,
		To:         h.To,
		Cc:         h.Cc,
		SentAt:     h.Date,
		MessageID:  h.MessageID,
		InReplyTo:  h.InReplyTo,
		References: h.References,
		Envelope: &model.Envelope{
			SESMessageID:       e.SESMessageID,
			EnvelopeSender:     e.EnvelopeSender,
			EnvelopeRecipients: e.EnvelopeRecipients,
			DeliveredTo:        e.DeliveredTo,
			Verdicts: map[string]string{
				"spam": e.Verdicts.Spam, "virus": e.Verdicts.Virus, "spf": e.Verdicts.SPF,
				"dkim": e.Verdicts.DKIM, "dmarc": e.Verdicts.DMARC, "dmarcPolicy": e.Verdicts.DMARCPolicy,
			},
			Reason:        e.Reason,
			ReplyTo:       h.ReplyTo,
			Incomplete:    h.Incomplete,
			ParserVersion: e.ParserVersion,
		},
	}
}
