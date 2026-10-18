package dispatch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// EventSource is the source of every s3mail event on the bus.
const EventSource = "s3mail.mail"

// putEventsMax is EventBridge's entry limit per PutEvents call.
const putEventsMax = 10

// PutEventsAPI is the client operation EventBus needs.
type PutEventsAPI interface {
	PutEvents(ctx context.Context, in *eventbridge.PutEventsInput, opts ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// EventBus publishes events to one EventBridge bus. The detail carries ids
// only, never mail content: consumers read mail through JMAP.
type EventBus struct {
	Client    PutEventsAPI
	Bus       string
	AccountID string
}

// EventDetail is the JSON detail of a published event.
type EventDetail struct {
	Version    int      `json:"version"`
	AccountID  string   `json:"accountId"`
	EventID    string   `json:"eventId"`
	EmailID    string   `json:"emailId"`
	MailboxIDs []string `json:"mailboxIds"`
}

// Publish sends the events in batches; any rejected entry fails the call,
// so the outbox entry stays and the whole entry is published again.
func (b *EventBus) Publish(ctx context.Context, events []model.Event) error {
	for start := 0; start < len(events); start += putEventsMax {
		batch := events[start:min(start+putEventsMax, len(events))]
		entries := make([]types.PutEventsRequestEntry, len(batch))
		for i, e := range batch {
			detail, err := json.Marshal(EventDetail{Version: 1, AccountID: b.AccountID, EventID: e.ID, EmailID: e.EmailID, MailboxIDs: e.MailboxIDs})
			if err != nil {
				return err
			}
			entries[i] = types.PutEventsRequestEntry{
				EventBusName: aws.String(b.Bus),
				Source:       aws.String(EventSource),
				DetailType:   aws.String(e.Type),
				Detail:       aws.String(string(detail)),
			}
		}
		out, err := b.Client.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: entries})
		if err != nil {
			return err
		}
		if out.FailedEntryCount > 0 {
			return fmt.Errorf("event bus rejected %d of %d events", out.FailedEntryCount, len(entries))
		}
	}
	return nil
}
