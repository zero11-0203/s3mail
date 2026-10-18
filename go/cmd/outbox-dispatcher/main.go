// Command outbox-dispatcher drains the transactional outbox
// (docs/specs/storage-sync.md#idempotency-and-durable-work). It is
// triggered by the table's stream, filtered to outbox inserts, and by a
// scheduled sweep that also checks the index against the change log.
//
// Environment: TABLE, ACCOUNT_ID, INDEX_QUEUE_URL and SUBMIT_QUEUE_URL;
// optionally EVENT_BUS, the EventBridge bus for content-free events such
// as EmailReceived. Without it events entries are acknowledged unpublished.
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/zero11-0203/s3mail/go/internal/dispatch"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/sendcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "INDEX_QUEUE_URL", "SUBMIT_QUEUE_URL")
	log := searchcmd.Logger("outbox-dispatcher")
	cfg := searchcmd.Config()
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	q := sqs.NewFromConfig(cfg)
	d := &dispatch.Dispatcher{
		Outbox: acct, Search: acct, Log: log,
		Waker:  &searchcmd.SQSWaker{SQS: q, QueueURL: env["INDEX_QUEUE_URL"]},
		Submit: &sendcmd.Queue{SQS: q, QueueURL: env["SUBMIT_QUEUE_URL"]},
	}
	if bus := os.Getenv("EVENT_BUS"); bus != "" {
		d.Events = &dispatch.EventBus{Client: eventbridge.NewFromConfig(cfg), Bus: bus, AccountID: env["ACCOUNT_ID"]}
	}
	lambda.Start(telemetry.Func(func(ctx context.Context, event json.RawMessage) error {
		// Stream batches carry only keys; the drain reads the outbox
		// itself, so records are not inspected. A failed drain fails the
		// batch and the stream retries it.
		var probe struct {
			Records []json.RawMessage `json:"Records"`
		}
		_ = json.Unmarshal(event, &probe)
		var err error
		if probe.Records != nil {
			err = d.Drain(ctx)
		} else {
			err = d.Sweep(ctx)
		}
		if err != nil {
			log.Error("dispatch", "error", err)
		}
		return err
	}))
}
