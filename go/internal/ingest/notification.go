// Package ingest turns SES S3-action receipt notifications into durable
// mailbox records (docs/specs/mail-flow.md#inbound-email).
package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Receipt is the part of an SES receipt notification ingestion relies on.
// Envelope fields come from the SMTP transaction, never from MIME headers.
type Receipt struct {
	SESMessageID   string
	EnvelopeSender string
	Recipients     []string // SMTP RCPT TO addresses, exactly as delivered
	ReceivedAt     time.Time
	Verdicts       Verdicts
	Bucket         string
	Key            string
}

// Verdicts are SES statuses: PASS, FAIL, GRAY or PROCESSING_FAILED.
type Verdicts struct {
	Spam        string
	Virus       string
	SPF         string
	DKIM        string
	DMARC       string
	DMARCPolicy string // NONE, QUARANTINE or REJECT, when DMARC failed
}

// ErrInvalid marks a notification that can never be processed; retrying it
// only moves it to the dead-letter queue, where it is kept for review.
var ErrInvalid = errors.New("invalid receipt notification")

// sesMessageID matches SES message IDs, which are also the S3 key suffix.
var sesMessageID = regexp.MustCompile(`^[a-z0-9]{16,128}$`)

type snsEnvelope struct {
	Type     string `json:"Type"`
	TopicArn string `json:"TopicArn"`
	Message  string `json:"Message"`
}

type verdict struct {
	Status string `json:"status"`
}

type sesNotification struct {
	NotificationType string `json:"notificationType"`
	Mail             struct {
		MessageID string `json:"messageId"`
		Source    string `json:"source"`
	} `json:"mail"`
	Receipt struct {
		Timestamp    time.Time `json:"timestamp"`
		Recipients   []string  `json:"recipients"`
		SpamVerdict  verdict   `json:"spamVerdict"`
		VirusVerdict verdict   `json:"virusVerdict"`
		SPFVerdict   verdict   `json:"spfVerdict"`
		DKIMVerdict  verdict   `json:"dkimVerdict"`
		DMARCVerdict verdict   `json:"dmarcVerdict"`
		DMARCPolicy  string    `json:"dmarcPolicy"`
		Action       struct {
			Type            string `json:"type"`
			TopicArn        string `json:"topicArn"`
			BucketName      string `json:"bucketName"`
			ObjectKeyPrefix string `json:"objectKeyPrefix"`
			ObjectKey       string `json:"objectKey"`
		} `json:"action"`
	} `json:"receipt"`
}

// Source is where notifications are expected to come from.
type Source struct {
	TopicArn  string
	Bucket    string
	KeyPrefix string
}

// ParseNotification decodes an SQS body holding the SNS envelope of an SES
// S3-action notification and checks it against the configured source.
func ParseNotification(body string, src Source) (Receipt, error) {
	var env snsEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return Receipt{}, fmt.Errorf("%w: SNS envelope: %v", ErrInvalid, err)
	}
	if env.Type != "Notification" || env.TopicArn != src.TopicArn {
		return Receipt{}, fmt.Errorf("%w: SNS type %q from an unexpected topic", ErrInvalid, env.Type)
	}
	var n sesNotification
	if err := json.Unmarshal([]byte(env.Message), &n); err != nil {
		return Receipt{}, fmt.Errorf("%w: SES notification: %v", ErrInvalid, err)
	}
	a := n.Receipt.Action
	switch {
	case n.NotificationType != "Received":
		return Receipt{}, fmt.Errorf("%w: notificationType %q", ErrInvalid, n.NotificationType)
	case a.Type != "S3" || a.BucketName != src.Bucket || a.ObjectKeyPrefix != src.KeyPrefix:
		return Receipt{}, fmt.Errorf("%w: action is not the configured S3 delivery", ErrInvalid)
	case !sesMessageID.MatchString(n.Mail.MessageID):
		return Receipt{}, fmt.Errorf("%w: malformed SES message id", ErrInvalid)
	case a.ObjectKey != src.KeyPrefix+n.Mail.MessageID:
		return Receipt{}, fmt.Errorf("%w: object key does not match the message id", ErrInvalid)
	case len(n.Receipt.Recipients) == 0:
		return Receipt{}, fmt.Errorf("%w: no envelope recipients", ErrInvalid)
	case n.Receipt.Timestamp.IsZero():
		return Receipt{}, fmt.Errorf("%w: no receipt timestamp", ErrInvalid)
	}
	return Receipt{
		SESMessageID:   n.Mail.MessageID,
		EnvelopeSender: n.Mail.Source,
		Recipients:     n.Receipt.Recipients,
		ReceivedAt:     n.Receipt.Timestamp.UTC(),
		Verdicts: Verdicts{
			Spam:        n.Receipt.SpamVerdict.Status,
			Virus:       n.Receipt.VirusVerdict.Status,
			SPF:         n.Receipt.SPFVerdict.Status,
			DKIM:        n.Receipt.DKIMVerdict.Status,
			DMARC:       n.Receipt.DMARCVerdict.Status,
			DMARCPolicy: strings.ToUpper(n.Receipt.DMARCPolicy),
		},
		Bucket: a.BucketName,
		Key:    a.ObjectKey,
	}, nil
}
