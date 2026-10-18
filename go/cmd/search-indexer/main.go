// Command search-indexer is the single index writer: each batch of queue
// wake-ups runs one replay and publication (docs/specs/search.md#index-pipeline).
//
// Environment: TABLE, ACCOUNT_ID and INDEX_BUCKET; optionally EMBED_MODEL
// and EMBED_KEY_SECRET for semantic vectors. The function's reserved
// concurrency is one and its ephemeral storage holds the working copy and
// snapshot under /tmp.
package main

import (
	"context"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/search"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "INDEX_BUCKET")
	base := searchcmd.Logger("search-indexer")
	cfg := searchcmd.Config()
	s3c := s3.NewFromConfig(cfg)
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	ix := search.Indexer{
		Source: acct, Registrar: acct,
		Originals: &store.Originals{S3: s3c, Log: base},
		Objects:   &store.IndexObjects{S3: s3c, Bucket: env["INDEX_BUCKET"]},
		Key:       searchcmd.IndexKey(env["ACCOUNT_ID"]),
		Dir:       os.TempDir(),
		Embedder:  searchcmd.Embedder(cfg),
	}
	lambda.Start(telemetry.Func(func(ctx context.Context, ev events.SQSEvent) error {
		inv := ix
		inv.Log = base
		if lc, ok := lambdacontext.FromContext(ctx); ok {
			inv.Log = base.With("requestId", lc.AwsRequestID)
		}
		inv.Log.Info("wake-ups", "messages", len(ev.Records))
		// The wake-ups only say "look"; a failure returns the whole batch
		// for redelivery and the next run replays from the published cursor.
		return inv.Run(ctx)
	}))
}
