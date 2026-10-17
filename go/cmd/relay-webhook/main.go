// Command relay-webhook receives SMTP2GO delivery notifications on
// POST /webhooks/smtp2go: it checks the bearer secret and persists the
// body on the delivery queue (docs/specs/mail-flow.md#outbound-relay-interface).
// It has no table or mail-content access.
//
// Environment: SECRET_ID (the bearer secret, a plain string) and QUEUE_URL.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/webhook"
)

// secretTTL bounds how long a rotated secret takes to apply.
const secretTTL = 5 * time.Minute

func main() {
	env := searchcmd.Env("SECRET_ID", "QUEUE_URL")
	log := searchcmd.Logger("relay-webhook")
	cfg := searchcmd.Config()
	sm := secretsmanager.NewFromConfig(cfg)
	q := sqs.NewFromConfig(cfg)

	var mu sync.Mutex
	var cached []string
	var fetched time.Time
	secrets := func(ctx context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(fetched) < secretTTL {
			return cached, nil
		}
		var out []string
		for _, stage := range []string{"AWSCURRENT", "AWSPREVIOUS"} {
			v, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(env["SECRET_ID"]), VersionStage: aws.String(stage)})
			if err != nil {
				if stage == "AWSPREVIOUS" {
					continue // no rotation yet
				}
				return nil, err
			}
			out = append(out, aws.ToString(v.SecretString))
		}
		cached, fetched = out, time.Now()
		return out, nil
	}
	r := &webhook.Receiver{
		Secrets: secrets,
		Persist: func(ctx context.Context, body string) error {
			_, err := q.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(env["QUEUE_URL"]), MessageBody: &body})
			return err
		},
		Log: log,
	}
	lambda.Start(func(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		return r.Handle(ctx, req), nil
	})
}
