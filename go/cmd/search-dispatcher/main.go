// Command search-dispatcher runs on a schedule and wakes the indexer when
// Emails were created or destroyed after the registered index generation
// (docs/specs/search.md#index-pipeline).
//
// Environment: TABLE, ACCOUNT_ID and QUEUE_URL.
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/zero11-0203/s3mail/go/internal/search"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "QUEUE_URL")
	log := searchcmd.Logger("search-dispatcher")
	cfg := searchcmd.Config()
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	w := &searchcmd.SQSWaker{SQS: sqs.NewFromConfig(cfg), QueueURL: env["QUEUE_URL"]}
	lambda.Start(func(ctx context.Context) error {
		woke, err := search.Dispatch(ctx, acct, w)
		if err != nil {
			log.Error("dispatch", "error", err)
			return err
		}
		log.Debug("dispatch", "woke", woke)
		return nil
	})
}
