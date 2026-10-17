// Command delivery-worker applies queued SMTP2GO notifications to
// submission delivery status (docs/specs/mail-flow.md#webhook-idempotency).
// Each report is deduplicated by a durable marker and only moves a
// recipient's state forward. A report whose submission is not known yet
// is retried by the queue and finally dead-lettered.
//
// Environment: TABLE and ACCOUNT_ID.
package main

import (
	"context"
	"errors"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/zero11-0203/s3mail/go/internal/relay/smtp2go"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/send"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID")
	log := searchcmd.Logger("delivery-worker")
	cfg := searchcmd.Config()
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	lambda.Start(func(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
		var resp events.SQSEventResponse
		for _, r := range ev.Records {
			reports, err := smtp2go.Parse([]byte(r.Body))
			if err != nil {
				// Persisted but unusable: kept in the DLQ for inspection.
				log.Error("unparseable notification", "messageId", r.MessageId, "error", err)
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: r.MessageId})
				continue
			}
			failed := false
			for _, d := range reports {
				applied, err := send.ApplyEvent(ctx, acct, acct, d)
				switch {
				case errors.Is(err, send.ErrUncorrelated):
					log.Warn("uncorrelated delivery event", "relayId", d.RelayID, "delivered", d.Delivered)
					failed = true
				case err != nil:
					log.Error("apply delivery event", "relayId", d.RelayID, "error", err)
					failed = true
				default:
					log.Info("delivery event", "relayId", d.RelayID, "delivered", d.Delivered, "applied", applied)
				}
			}
			if failed {
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: r.MessageId})
			}
		}
		return resp, nil
	})
}
