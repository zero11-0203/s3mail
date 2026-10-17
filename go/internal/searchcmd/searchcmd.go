// Package searchcmd holds the AWS wiring shared by the search dispatcher,
// indexer and search functions and the JMAP API's search client
// (docs/specs/search.md#index-pipeline). Each function runs under its own
// role (docs/operations/permissions.md).
package searchcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/zero11-0203/s3mail/go/internal/buildinfo"
	"github.com/zero11-0203/s3mail/go/internal/search"
)

// Env reads the required environment variables or exits.
func Env(keys ...string) map[string]string {
	env := map[string]string{}
	for _, k := range keys {
		if env[k] = os.Getenv(k); env[k] == "" {
			log.Fatalf("%s is required", k)
		}
	}
	return env
}

// Logger is the JSON logger at Lambda's configured level.
func Logger(component string) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("AWS_LAMBDA_LOG_LEVEL")))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})).
		With("component", component, "version", buildinfo.Version)
}

// Config loads the AWS configuration or exits.
func Config() aws.Config {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

// IndexKey is the account's index object in the index bucket.
func IndexKey(accountID string) string { return "index/" + accountID + ".sqlite" }

// SQSWaker sends wake-ups to the index queue.
type SQSWaker struct {
	SQS      *sqs.Client
	QueueURL string
}

// Wake implements search.Waker.
func (w *SQSWaker) Wake(ctx context.Context, seq int64) error {
	body := `{"seq":` + strconv.FormatInt(seq, 10) + `}`
	_, err := w.SQS.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &w.QueueURL, MessageBody: &body})
	return err
}

// LambdaSearcher invokes the search function synchronously.
type LambdaSearcher struct {
	Lambda   *lambda.Client
	Function string
}

// Search implements jmap.Searcher.
func (l *LambdaSearcher) Search(ctx context.Context, req search.Request) (search.Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return search.Response{}, err
	}
	out, err := l.Lambda.Invoke(ctx, &lambda.InvokeInput{FunctionName: &l.Function, Payload: payload})
	if err != nil {
		return search.Response{}, err
	}
	if out.FunctionError != nil {
		return search.Response{}, fmt.Errorf("search function: %s", *out.FunctionError)
	}
	var resp search.Response
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		return search.Response{}, fmt.Errorf("search response: %w", err)
	}
	return resp, nil
}
