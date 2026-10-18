// Command agent-trigger starts the mail agent's event-driven runs
// (docs/specs/agent.md#architecture): triage for each EmailReceived message
// on the agent queue, and the scheduled daily digest. It may only invoke the
// runtime's live endpoint and consume its queue.
//
// Environment: RUNTIME_ARN and QUALIFIER (live).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"

	"github.com/zero11-0203/s3mail/go/internal/agentcli"
	"github.com/zero11-0203/s3mail/go/internal/agenttrigger"
	"github.com/zero11-0203/s3mail/go/internal/searchcmd"
	"github.com/zero11-0203/s3mail/go/internal/telemetry"
)

func main() {
	env := searchcmd.Env("RUNTIME_ARN", "QUALIFIER")
	log := searchcmd.Logger("agent-trigger")
	cfg := searchcmd.Config()
	t := &agenttrigger.Trigger{
		Agent: &agentcli.Client{API: bedrockagentcore.NewFromConfig(cfg), RuntimeARN: env["RUNTIME_ARN"], Qualifier: env["QUALIFIER"]},
		Log:   log,
		Now:   time.Now,
	}
	lambda.Start(telemetry.Handler(func(ctx context.Context, raw json.RawMessage) (any, error) {
		var probe struct {
			Records []json.RawMessage `json:"Records"`
			Digest  string            `json:"digest"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if probe.Records != nil {
			var batch events.SQSEvent
			if err := json.Unmarshal(raw, &batch); err != nil {
				return nil, err
			}
			return t.HandleSQS(ctx, batch)
		}
		if probe.Digest != "" {
			return nil, t.HandleSchedule(ctx, agenttrigger.Schedule{Digest: probe.Digest})
		}
		return nil, errors.New("unsupported event")
	}))
}
