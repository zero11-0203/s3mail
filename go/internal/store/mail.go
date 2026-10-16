// Package store implements the mail table commit protocol
// (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/ingest"
)

// DynamoDB is the subset of the client the store uses.
type DynamoDB interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Mail is one account's partition of the mail table.
type Mail struct {
	DB        DynamoDB
	Table     string
	AccountID string
	// Backoff before retrying a sequence conflict; nil uses bounded jitter.
	Backoff func(attempt int) time.Duration
}

const maxCommitAttempts = 5

// Positions of the receipt transaction's items, used to read cancellation
// reasons.
const (
	itemState = iota
	itemReceipt
	itemEmail
	itemList
	itemChange
	itemOutboxIndex
	itemOutboxEvent
)

// ErrContention means every attempt lost the account sequence race.
var ErrContention = errors.New("account sequence contention")

func (m *Mail) pk() string { return "ACCOUNT#" + m.AccountID }

func (m *Mail) key(sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"PK": s(m.pk()), "SK": s(sk)}
}

// Seen reads the receipt marker strongly.
func (m *Mail) Seen(ctx context.Context, sesMessageID string) (bool, error) {
	out, err := m.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            &m.Table,
		Key:                  m.key("RECEIPT#" + sesMessageID),
		ConsistentRead:       aws.Bool(true),
		ProjectionExpression: aws.String("SK"),
	})
	if err != nil {
		return false, err
	}
	return out.Item != nil, nil
}

func (m *Mail) sequence(ctx context.Context) (int64, error) {
	out, err := m.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            &m.Table,
		Key:                  m.key("STATE"),
		ConsistentRead:       aws.Bool(true),
		ProjectionExpression: aws.String("seq"),
	})
	if err != nil {
		return 0, err
	}
	v, ok := out.Item["seq"].(*types.AttributeValueMemberN)
	if !ok {
		return 0, nil
	}
	return strconv.ParseInt(v.Value, 10, 64)
}

// CommitReceipt writes the receipt marker, Email, listing entry, change
// record, outbox work and next account sequence in one transaction,
// conditioned on the marker's absence and the sequence just read.
func (m *Mail) CommitReceipt(ctx context.Context, e ingest.Email) (ingest.Outcome, error) {
	for attempt := range maxCommitAttempts {
		seq, err := m.sequence(ctx)
		if err != nil {
			return "", fmt.Errorf("read STATE: %w", err)
		}
		_, err = m.DB.TransactWriteItems(ctx, m.receiptTransaction(e, seq))
		if err == nil {
			return ingest.Committed, nil
		}
		var tce *types.TransactionCanceledException
		if !errors.As(err, &tce) {
			return "", err
		}
		switch {
		case reason(tce, itemReceipt) == "ConditionalCheckFailed":
			// Never treat a failed condition as proof of a commit: confirm
			// the marker with a strong read.
			seen, err := m.Seen(ctx, e.SESMessageID)
			if err != nil {
				return "", fmt.Errorf("confirm receipt marker: %w", err)
			}
			if seen {
				return ingest.Duplicate, nil
			}
			return "", errors.New("receipt condition failed but no marker exists")
		case reason(tce, itemEmail) == "ConditionalCheckFailed":
			return "", fmt.Errorf("email %s exists without a receipt marker", e.ID)
		case reason(tce, itemState) == "ConditionalCheckFailed" || anyReason(tce, "TransactionConflict"):
			if err := sleep(ctx, m.backoff(attempt)); err != nil {
				return "", err
			}
			continue
		}
		return "", err
	}
	return "", ErrContention
}

func (m *Mail) backoff(attempt int) time.Duration {
	if m.Backoff != nil {
		return m.Backoff(attempt)
	}
	base := 20 * time.Millisecond << attempt
	return base/2 + rand.N(base)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func reason(tce *types.TransactionCanceledException, i int) string {
	if i < len(tce.CancellationReasons) {
		return aws.ToString(tce.CancellationReasons[i].Code)
	}
	return ""
}

func anyReason(tce *types.TransactionCanceledException, code string) bool {
	for _, r := range tce.CancellationReasons {
		if aws.ToString(r.Code) == code {
			return true
		}
	}
	return false
}

func padded(seq int64) string { return fmt.Sprintf("%020d", seq) }

// sortTime is fixed-width so listing keys sort chronologically.
func sortTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (m *Mail) receiptTransaction(e ingest.Email, prev int64) *dynamodb.TransactWriteItemsInput {
	next := prev + 1
	state := &types.Update{
		TableName:        &m.Table,
		Key:              m.key("STATE"),
		UpdateExpression: aws.String("SET seq = :next, emailState = :next, epoch = if_not_exists(epoch, :one)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":next": n(next),
			":one":  n(1),
		},
	}
	if prev == 0 {
		state.ConditionExpression = aws.String("attribute_not_exists(seq)")
	} else {
		state.ConditionExpression = aws.String("seq = :prev")
		state.ExpressionAttributeValues[":prev"] = n(prev)
	}

	absent := aws.String("attribute_not_exists(PK)")
	put := func(sk string, attrs map[string]types.AttributeValue, cond *string) types.TransactWriteItem {
		item := m.key(sk)
		for k, v := range attrs {
			item[k] = v
		}
		return types.TransactWriteItem{Put: &types.Put{TableName: &m.Table, Item: item, ConditionExpression: cond}}
	}

	// The marker outlives the Email; it holds no sender, recipients or
	// content (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
	receipt := map[string]types.AttributeValue{
		"emailId":       s(e.ID),
		"bucket":        s(e.Bucket),
		"key":           s(e.Key),
		"versionId":     s(e.VersionID),
		"receivedAt":    s(sortTime(e.ReceivedAt)),
		"seq":           n(next),
		"parserVersion": n(int64(e.ParserVersion)),
	}
	event := map[string]types.AttributeValue{
		"eventId":    s(e.ID + ":received"),
		"type":       s("EmailReceived"),
		"emailId":    s(e.ID),
		"mailboxIds": l(e.Mailbox),
	}
	return &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			itemState:   {Update: state},
			itemReceipt: put("RECEIPT#"+e.SESMessageID, receipt, absent),
			itemEmail:   put("EMAIL#"+e.ID, emailItem(e, next), absent),
			itemList: put(fmt.Sprintf("LIST#%s#%s#%s", e.Mailbox, sortTime(e.ReceivedAt), e.ID),
				map[string]types.AttributeValue{"emailId": s(e.ID)}, absent),
			itemChange: put("CHANGE#"+padded(next), map[string]types.AttributeValue{
				"created":     &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"Email": l(e.ID)}},
				"searchDirty": l(e.ID),
			}, absent),
			itemOutboxIndex: put("OUTBOX#"+padded(next)+"#index", map[string]types.AttributeValue{"emailIds": l(e.ID)}, absent),
			itemOutboxEvent: put("OUTBOX#"+padded(next)+"#events", event, absent),
		},
	}
}

func emailItem(e ingest.Email, seq int64) map[string]types.AttributeValue {
	h := e.Headers
	item := map[string]types.AttributeValue{
		"emailId":            s(e.ID),
		"threadId":           s(e.ThreadID),
		"mailboxIds":         &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{e.Mailbox: &types.AttributeValueMemberBOOL{Value: true}}},
		"keywords":           &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{}},
		"receivedAt":         s(sortTime(e.ReceivedAt)),
		"size":               n(e.Size),
		"blob":               &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"bucket": s(e.Bucket), "key": s(e.Key), "versionId": s(e.VersionID)}},
		"sesMessageId":       s(e.SESMessageID),
		"envelopeSender":     s(e.EnvelopeSender),
		"envelopeRecipients": l(e.EnvelopeRecipients...),
		"deliveredTo":        l(e.DeliveredTo...),
		"verdicts": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"spam": s(e.Verdicts.Spam), "virus": s(e.Verdicts.Virus), "spf": s(e.Verdicts.SPF),
			"dkim": s(e.Verdicts.DKIM), "dmarc": s(e.Verdicts.DMARC), "dmarcPolicy": s(e.Verdicts.DMARCPolicy),
		}},
		"subject":       s(h.Subject),
		"from":          l(h.From...),
		"to":            l(h.To...),
		"cc":            l(h.Cc...),
		"replyTo":       l(h.ReplyTo...),
		"messageId":     s(h.MessageID),
		"inReplyTo":     s(h.InReplyTo),
		"references":    s(h.References),
		"incomplete":    l(h.Incomplete...),
		"parserVersion": n(int64(e.ParserVersion)),
		"createdSeq":    n(seq),
	}
	if e.Reason != "" {
		item["dispositionReason"] = s(e.Reason)
	}
	if !h.Date.IsZero() {
		item["sentAt"] = s(h.Date.Format(time.RFC3339))
	}
	return item
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func l(vs ...string) types.AttributeValue {
	list := make([]types.AttributeValue, len(vs))
	for i, v := range vs {
		list[i] = s(v)
	}
	return &types.AttributeValueMemberL{Value: list}
}
