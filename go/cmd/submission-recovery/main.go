// Command submission-recovery is the one-minute work-recovery sweep: it
// settles expired attempt leases and re-enqueues due submissions whose
// wake-up was lost (docs/specs/mail-flow.md#outbound-email). It never
// talks to the relay and cannot read the relay secret.
//
// Environment: TABLE, ACCOUNT_ID, QUEUE_URL, RELAY and RELAY_PARAMETER.
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/send"
	"github.com/zero11-0203/s3mail/go/internal/sendcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "QUEUE_URL", "RELAY", "RELAY_PARAMETER")
	log := searchcmd.Logger("submission-recovery")
	cfg := searchcmd.Config()
	r := &send.Recovery{
		Store:  &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]},
		Config: &sendcmd.Config{SSM: ssm.NewFromConfig(cfg), Parameter: env["RELAY_PARAMETER"], RelayName: env["RELAY"], Log: log},
		Queue:  &sendcmd.Queue{SQS: sqs.NewFromConfig(cfg), QueueURL: env["QUEUE_URL"]},
		Log:    log,
	}
	lambda.Start(func(ctx context.Context) error {
		if err := r.Run(ctx); err != nil {
			log.Error("recovery", "error", err)
			return err
		}
		return nil
	})
}
