// Command gc is the daily derived-data collector: it prunes change
// history older than the retention window and purges search index
// versions that may still hold destroyed Emails
// (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
// It cannot read or delete original mail.
//
// Environment: TABLE, ACCOUNT_ID and INDEX_BUCKET.
package main

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/gc"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "INDEX_BUCKET")
	log := searchcmd.Logger("gc")
	cfg := searchcmd.Config()
	db := dynamodb.NewFromConfig(cfg)
	c := &gc.Collector{
		Store:   &store.Account{DB: db, Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]},
		History: db,
		S3:      s3.NewFromConfig(cfg),
		Bucket:  env["INDEX_BUCKET"],
		Log:     log,
	}
	lambda.Start(telemetry.Func(func(ctx context.Context, _ json.RawMessage) error {
		if err := c.Run(ctx); err != nil {
			log.Error("gc", "error", err)
			return err
		}
		return nil
	}))
}
