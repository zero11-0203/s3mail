// Package jmapcmd is the shared Lambda entry point of the JMAP API,
// download and push functions. Each runs under its own role
// (docs/operations/permissions.md).
//
// Environment: TABLE, ACCOUNT_ID (internal account), OWNER_SUB (the
// Cognito subject mapped to it), PUBLIC_URL (the public API root that
// session URLs are built from), ORIGINALS_BUCKET (durable MIME for new
// Emails) and BLOBS_BUCKET (uploads). Each role's policy decides which of
// the buckets the function can actually use. SEARCH_FUNCTION, when set,
// enables text conditions and SearchSnippet/get through the search function.
// SEND_ALIASES (comma-separated local parts, never the canary), with
// DOMAIN and the sendcmd policy variables, enables Identity and
// EmailSubmission. CORS_ORIGIN, when set, is the one browser origin
// allowed to read responses (the Phase 4 webmail).
package jmapcmd

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/buildinfo"
	"github.com/zero11-0203/s3mail/go/internal/jmap"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/sendcmd"
	"github.com/zero11-0203/s3mail/go/internal/store"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

// Handler is one of the jmap.Server HTTP handlers.
type Handler func(s *jmap.Server, ctx context.Context, req events.APIGatewayProxyRequest, owners jmap.Owners) (*events.APIGatewayProxyStreamingResponse, error)

// Start configures the server from the environment and serves h.
func Start(component string, h Handler) {
	env := map[string]string{}
	for _, k := range []string{"TABLE", "ACCOUNT_ID", "OWNER_SUB", "PUBLIC_URL", "ORIGINALS_BUCKET", "BLOBS_BUCKET"} {
		if env[k] = os.Getenv(k); env[k] == "" {
			log.Fatalf("%s is required", k)
		}
	}
	if !strings.HasPrefix(env["PUBLIC_URL"], "https://") {
		log.Fatal("PUBLIC_URL must be an https URL")
	}
	cors := os.Getenv("CORS_ORIGIN")
	if cors != "" && (!strings.HasPrefix(cors, "https://") || strings.Count(cors, "/") != 2) {
		log.Fatal("CORS_ORIGIN must be an https origin without a path")
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("AWS_LAMBDA_LOG_LEVEL")))
	base := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("component", component, "version", buildinfo.Version)

	telemetry.Init()
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	// The push stream polls the account state every three seconds; one
	// span per connection is enough there (docs/operations/monitoring.md#tracing-x-ray).
	if component != "jmap-push" {
		telemetry.AWS(&cfg)
	}
	acct := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: env["TABLE"], AccountID: env["ACCOUNT_ID"]}
	s3c := s3.NewFromConfig(cfg)
	originals := &store.Originals{S3: s3c, Log: base}
	owners := jmap.Owners{env["OWNER_SUB"]: env["ACCOUNT_ID"]}
	srv := jmap.NewServer(strings.TrimSuffix(env["PUBLIC_URL"], "/"), acct, originals, base)
	srv.Archive = &store.Archive{S3: s3c, Bucket: env["ORIGINALS_BUCKET"]}
	srv.Uploads = &store.Uploads{S3: s3c, Bucket: env["BLOBS_BUCKET"]}
	if fn := os.Getenv("SEARCH_FUNCTION"); fn != "" {
		srv.Searcher = &searchcmd.LambdaSearcher{Lambda: awslambda.NewFromConfig(cfg), Function: fn}
		// The search function embeds queries only with its own EMBED_MODEL.
		srv.Semantic = os.Getenv("SEMANTIC") == "true"
	}
	if aliases := os.Getenv("SEND_ALIASES"); aliases != "" {
		policy, err := sendcmd.PolicyFromEnv()
		if err != nil {
			log.Fatal(err)
		}
		sd := &jmap.Sending{Domain: strings.ToLower(os.Getenv("DOMAIN")), Policy: policy}
		for _, a := range strings.Split(aliases, ",") {
			if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
				sd.Aliases = append(sd.Aliases, a)
			}
		}
		if sd.Domain == "" {
			log.Fatal("DOMAIN is required with SEND_ALIASES")
		}
		srv.Sending = sd
	}

	lambda.Start(telemetry.Streaming(func(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
		// Lambda runs one invocation per environment at a time, so the
		// per-request logger can live on the shared server.
		srv.Log = base
		if lc, ok := lambdacontext.FromContext(ctx); ok {
			srv.Log = base.With("requestId", lc.AwsRequestID)
		}
		resp, err := h(srv, ctx, req, owners)
		allowOrigin(cors, req, resp)
		return resp, err
	}, func(r *events.APIGatewayProxyStreamingResponse) *io.Reader { return &r.Body }))
}
