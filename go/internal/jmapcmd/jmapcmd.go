// Package jmapcmd is the shared Lambda entry point of the JMAP API,
// download and push functions. Each runs under its own role
// (docs/operations/permissions.md).
//
// Environment: TABLE, ACCOUNT_ID (internal account), OWNER_SUB (the
// Cognito subject mapped to it) and PUBLIC_URL (the public API root that
// session URLs are built from).
package jmapcmd

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
	"github.com/zero11-0203/s3mail/go/internal/jmap"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

// Handler is one of the jmap.Server HTTP handlers.
type Handler func(s *jmap.Server, ctx context.Context, req events.APIGatewayProxyRequest, owners jmap.Owners) (*events.APIGatewayProxyStreamingResponse, error)

// Start configures the server from the environment and serves h.
func Start(component string, h Handler) {
	env := map[string]string{}
	for _, k := range []string{"TABLE", "ACCOUNT_ID", "OWNER_SUB", "PUBLIC_URL"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			log.Fatalf("%s is required", k)
		}
	}
	if !strings.HasPrefix(env["PUBLIC_URL"], "https://") {
		log.Fatal("PUBLIC_URL must be an https URL")
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("AWS_LAMBDA_LOG_LEVEL")))
	base := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("component", component, "version", buildinfo.Version)

	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	originals := &store.Originals{S3: s3.NewFromConfig(cfg), Log: base}
	owners := jmap.Owners{env["OWNER_SUB"]: env["ACCOUNT_ID"]}
	srv := jmap.NewServer(strings.TrimSuffix(env["PUBLIC_URL"], "/"), acct, originals, base)

	lambda.Start(func(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
		// Lambda runs one invocation per environment at a time, so the
		// per-request logger can live on the shared server.
		srv.Log = base
		if lc, ok := lambdacontext.FromContext(ctx); ok {
			srv.Log = base.With("requestId", lc.AwsRequestID)
		}
		return h(srv, ctx, req, owners)
	})
}
