// Command ingest consumes the SES receipt queue and commits each delivery
// to the mail table (docs/specs/mail-flow.md#inbound-email).
//
// Environment: ACCOUNT_ID (internal account), TABLE, BUCKET, KEY_PREFIX,
// TOPIC_ARN, DOMAIN and ALIASES (comma-separated local parts, including
// canary), all set by Terraform from the same alias declaration as the SES
// receipt rule.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/buildinfo"
	"github.com/zero11-0203/s3mail/go/internal/ingest"
	"github.com/zero11-0203/s3mail/go/internal/store"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

func main() {
	env := map[string]string{}
	for _, k := range []string{"ACCOUNT_ID", "TABLE", "BUCKET", "KEY_PREFIX", "TOPIC_ARN", "DOMAIN", "ALIASES"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			log.Fatalf("%s is required", k)
		}
	}
	var aliases []string
	for _, a := range strings.Split(env["ALIASES"], ",") {
		if a = strings.TrimSpace(strings.ToLower(a)); a != "" {
			aliases = append(aliases, a)
		}
	}

	// Lambda's JSON log format sets AWS_LAMBDA_LOG_LEVEL from Terraform.
	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("AWS_LAMBDA_LOG_LEVEL")))
	base := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("component", "ingest", "version", buildinfo.Version)

	telemetry.Init()
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	telemetry.AWS(&cfg)
	h := ingest.Handler{
		AccountID: env["ACCOUNT_ID"],
		Source:    ingest.Source{TopicArn: env["TOPIC_ARN"], Bucket: env["BUCKET"], KeyPrefix: env["KEY_PREFIX"]},
		Registry:  ingest.Registry{Domain: strings.ToLower(env["DOMAIN"]), Aliases: aliases},
		Originals: &store.Originals{S3: s3.NewFromConfig(cfg), Log: base},
		Store:     &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]},
	}
	lambda.Start(telemetry.Handler(func(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
		inv := h
		inv.Log = base
		if lc, ok := lambdacontext.FromContext(ctx); ok {
			inv.Log = base.With("requestId", lc.AwsRequestID)
		}
		return inv.Handle(ctx, ev)
	}))
}
