// Command submission-worker sends queued submissions through the active
// relay, at most once per durable submission
// (docs/specs/mail-flow.md#outbound-email).
//
// Environment: TABLE, ACCOUNT_ID, QUEUE_URL (its own queue, for retries),
// RELAY, RELAY_PARAMETER, SECRET_ID, SMTP_HOST, SMTP_PORT and HELO_NAME.
package main

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/send"
	"github.com/zero11-0203/s3mail/go/internal/sendcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "QUEUE_URL", "RELAY", "RELAY_PARAMETER", "SECRET_ID", "SMTP_HOST", "SMTP_PORT", "HELO_NAME")
	log := searchcmd.Logger("submission-worker")
	port, err := strconv.Atoi(env["SMTP_PORT"])
	if err != nil || (port != 465 && port != 587) {
		log.Error("SMTP_PORT is 465 or 587")
		panic("bad SMTP_PORT")
	}
	cfg := searchcmd.Config()
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	w := &send.Worker{
		Store:     acct,
		Originals: &store.Originals{S3: s3.NewFromConfig(cfg), Log: log},
		Config: &sendcmd.Config{
			SSM: ssm.NewFromConfig(cfg), Parameter: env["RELAY_PARAMETER"], RelayName: env["RELAY"], Log: log,
			Secrets: secretsmanager.NewFromConfig(cfg), SecretID: env["SECRET_ID"],
			Host: env["SMTP_HOST"], Port: port, HeloName: env["HELO_NAME"],
		},
		Queue: &sendcmd.Queue{SQS: sqs.NewFromConfig(cfg), QueueURL: env["QUEUE_URL"]},
		Log:   log,
	}
	lambda.Start(func(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
		var resp events.SQSEventResponse
		for _, r := range ev.Records {
			var m sendcmd.Message
			if err := json.Unmarshal([]byte(r.Body), &m); err != nil || m.SubmissionID == "" {
				log.Error("malformed wake-up", "messageId", r.MessageId)
				continue
			}
			if err := w.Process(ctx, m.SubmissionID); err != nil {
				log.Error("process", "submissionId", m.SubmissionID, "error", err)
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: r.MessageId})
			}
		}
		return resp, nil
	})
}
