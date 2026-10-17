// Package sendcmd holds the AWS wiring shared by the submission worker,
// the work-recovery sweep and the JMAP API's sending configuration
// (docs/specs/mail-flow.md#outbound-email). Each function runs under its
// own role (docs/operations/permissions.md).
//
// The relay parameter (SSM) names the active relay; any other value,
// normally "paused", pauses sending without a deployment. The relay
// secret (Secrets Manager) is JSON {"username": ..., "password": ...},
// set out of band.
package sendcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/zero11-0203/s3mail/go/internal/relay"
	"github.com/zero11-0203/s3mail/go/internal/send"
)

// PolicyFromEnv reads RELAY, QUOTA_DAILY, QUOTA_MONTHLY,
// QUOTA_PER_RECIPIENT and QUOTA_TIMEZONE.
func PolicyFromEnv() (send.Policy, error) {
	p := send.Policy{Relay: os.Getenv("RELAY"), PerRecipient: os.Getenv("QUOTA_PER_RECIPIENT") != "false"}
	if p.Relay == "" {
		return p, errors.New("RELAY is required")
	}
	for _, c := range []struct {
		key string
		out *int64
	}{{"QUOTA_DAILY", &p.Daily}, {"QUOTA_MONTHLY", &p.Monthly}} {
		if v := os.Getenv(c.key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return p, fmt.Errorf("%s is a non-negative integer", c.key)
			}
			*c.out = n
		}
	}
	if tz := os.Getenv("QUOTA_TIMEZONE"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return p, fmt.Errorf("QUOTA_TIMEZONE: %w", err)
		}
		p.Location = loc
	}
	return p, nil
}

// Queue sends wake-ups to the submission queue.
type Queue struct {
	SQS      *sqs.Client
	QueueURL string
}

// Message is the submission queue body.
type Message struct {
	SubmissionID string `json:"submissionId"`
}

// maxDelay is SQS's delay limit; a later due time is re-checked by the
// worker and re-enqueued by recovery.
const maxDelay = 15 * time.Minute

// Enqueue implements send.Queue.
func (q *Queue) Enqueue(ctx context.Context, id string, delay time.Duration) error {
	b, _ := json.Marshal(Message{SubmissionID: id})
	body := string(b)
	secs := int32(min(max(delay, 0), maxDelay) / time.Second)
	_, err := q.SQS.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: &q.QueueURL, MessageBody: &body, DelaySeconds: secs})
	return err
}

// Config reads the relay parameter per call and builds the SMTP adapter
// from a cached secret. The recovery sweep uses only Paused and leaves
// the secret fields empty.
type Config struct {
	SSM       *ssm.Client
	Parameter string
	// RelayName is the configured relay's name; the parameter must equal it
	// for sending to run.
	RelayName string
	// Log records the parameter value whenever it pauses sending.
	Log *slog.Logger

	Secrets  *secretsmanager.Client
	SecretID string
	Host     string
	Port     int
	HeloName string

	mu      sync.Mutex
	creds   credentials
	fetched time.Time
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// secretTTL bounds how long a rotated secret takes to apply.
const secretTTL = 5 * time.Minute

// Paused implements send.Config.
func (c *Config) Paused(ctx context.Context) (bool, error) {
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: &c.Parameter})
	if err != nil {
		return true, err
	}
	var value string
	var version int64
	if out.Parameter != nil {
		value, version = aws.ToString(out.Parameter.Value), out.Parameter.Version
	}
	if value != c.RelayName && c.Log != nil {
		c.Log.Info("relay parameter pauses sending", "value", value, "version", version, "relay", c.RelayName)
	}
	return value != c.RelayName, nil
}

// Relay implements send.Config.
func (c *Config) Relay(ctx context.Context) (relay.Relay, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.creds.Username == "" || time.Since(c.fetched) > secretTTL {
		out, err := c.Secrets.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &c.SecretID})
		if err != nil {
			return nil, fmt.Errorf("read relay secret: %w", err)
		}
		var cr credentials
		if out.SecretString == nil || json.Unmarshal([]byte(*out.SecretString), &cr) != nil || cr.Username == "" || cr.Password == "" {
			return nil, errors.New("relay secret is not {\"username\",\"password\"} JSON")
		}
		c.creds, c.fetched = cr, time.Now()
	}
	return &relay.SMTP{
		Host: c.Host, Port: c.Port, ImplicitTLS: c.Port == 465,
		Username: c.creds.Username, Password: c.creds.Password, HeloName: c.HeloName,
		Timeout: 60 * time.Second,
	}, nil
}
