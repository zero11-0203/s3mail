package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

type putEvents struct {
	calls  [][]string
	failed int32
}

func (p *putEvents) PutEvents(_ context.Context, in *eventbridge.PutEventsInput, _ ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error) {
	var details []string
	for _, e := range in.Entries {
		if *e.EventBusName != "s3mail-events" || *e.Source != EventSource || *e.DetailType != "EmailReceived" {
			return nil, fmt.Errorf("entry %+v", e)
		}
		details = append(details, *e.Detail)
	}
	p.calls = append(p.calls, details)
	return &eventbridge.PutEventsOutput{FailedEntryCount: p.failed}, nil
}

func TestEventBusPublish(t *testing.T) {
	var events []model.Event
	for i := range 12 {
		id := fmt.Sprintf("M%d", i)
		events = append(events, model.Event{ID: id + ":received", Type: "EmailReceived", EmailID: id, MailboxIDs: []string{"inbox"}})
	}
	api := &putEvents{}
	b := &EventBus{Client: api, Bus: "s3mail-events", AccountID: "a1"}
	if err := b.Publish(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 2 || len(api.calls[0]) != 10 || len(api.calls[1]) != 2 {
		t.Fatalf("batches %v", api.calls)
	}
	var d EventDetail
	if err := json.Unmarshal([]byte(api.calls[1][1]), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || d.AccountID != "a1" || d.EventID != "M11:received" || d.EmailID != "M11" || len(d.MailboxIDs) != 1 {
		t.Errorf("detail %+v", d)
	}

	api.failed = 1
	if err := b.Publish(context.Background(), events[:1]); err == nil {
		t.Error("a rejected entry was not reported")
	}
}
