// Command search answers the JMAP API's text conditions and snippets from
// the registered index generation (docs/specs/search.md#index-pipeline).
// It is invoked directly with a search.Request; warm instances keep their
// downloaded generation.
//
// Environment: TABLE, ACCOUNT_ID and INDEX_BUCKET.
package main

import (
	"context"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/search"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	env := searchcmd.Env("TABLE", "ACCOUNT_ID", "INDEX_BUCKET")
	log := searchcmd.Logger("search")
	cfg := searchcmd.Config()
	r := &search.Reader{
		Source:  &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]},
		Objects: &store.IndexObjects{S3: s3.NewFromConfig(cfg), Bucket: env["INDEX_BUCKET"]},
		Dir:     os.TempDir(),
		Log:     log,
	}
	lambda.Start(func(ctx context.Context, req search.Request) (search.Response, error) {
		resp, err := r.Serve(ctx, req)
		if err != nil {
			log.Error("search", "op", req.Op, "error", err)
		}
		return resp, err
	})
}
