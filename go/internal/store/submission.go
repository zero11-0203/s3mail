package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Identities and submissions are small documents stored as one JSON
// attribute ("doc"); status and nextAttemptAt are also top-level for the
// recovery sweep's projection.

type identityDoc struct {
	Email         string          `json:"email"`
	Name          string          `json:"name"`
	ReplyTo       []model.Address `json:"replyTo,omitempty"`
	Bcc           []model.Address `json:"bcc,omitempty"`
	TextSignature string          `json:"textSignature,omitempty"`
	HTMLSignature string          `json:"htmlSignature,omitempty"`
}

type submissionDoc struct {
	IdentityID    string                           `json:"identityId"`
	EmailID       string                           `json:"emailId"`
	ThreadID      string                           `json:"threadId"`
	MailFrom      string                           `json:"mailFrom"`
	RcptTo        []string                         `json:"rcptTo"`
	SendAt        time.Time                        `json:"sendAt"`
	Blob          model.BlobRef                    `json:"blob"`
	Size          int64                            `json:"size"`
	Relay         string                           `json:"relay"`
	RelayID       string                           `json:"relayId,omitempty"`
	Status        string                           `json:"status"`
	Attempt       int                              `json:"attempt"`
	Owner         string                           `json:"owner,omitempty"`
	LeaseUntil    time.Time                        `json:"leaseUntil"`
	Handoff       bool                             `json:"handoff"`
	NextAttemptAt time.Time                        `json:"nextAttemptAt"`
	Recipients    map[string]model.RecipientStatus `json:"recipients"`
	Reservation   model.Reservation                `json:"reservation"`
	Destroyed     bool                             `json:"destroyed,omitempty"`
}

func docAttr(v any) types.AttributeValue {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain structs are encoded
	}
	return s(string(b))
}

func (a *Account) quotaItem(q model.QuotaDelta) types.TransactWriteItem {
	u := &types.Update{
		TableName:                 &a.Table,
		Key:                       a.key("QUOTA#" + q.Key),
		UpdateExpression:          aws.String("ADD units :u"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":u": n(q.Units)},
	}
	if q.Units > 0 {
		u.ConditionExpression = aws.String("attribute_not_exists(units) OR units <= :max")
		u.ExpressionAttributeValues[":max"] = n(q.Limit - q.Units)
	}
	return types.TransactWriteItem{Update: u}
}

// quotaFailed reports whether a quota reservation's condition canceled
// the transaction; the quota items follow STATE and the optional receipt
// or registration item.
func quotaFailed(tce *types.TransactionCanceledException, m model.Mutation) bool {
	start := 1
	if m.Receipt != nil || m.Search != nil {
		start = 2
	}
	for i := range m.Quota {
		if m.Quota[i].Units > 0 && reason(tce, start+i) == "ConditionalCheckFailed" {
			return true
		}
	}
	return false
}

// sendItems writes identity, submission and attempt records.
func (a *Account) sendItems(m model.Mutation, seq int64) []types.TransactWriteItem {
	var items []types.TransactWriteItem
	for _, c := range m.Identities {
		if c.After == nil {
			items = append(items, a.del("IDENTITY#"+c.Before.ID, exists))
			continue
		}
		x := c.After
		cond := exists
		if c.Before == nil {
			cond = absent
		}
		items = append(items, a.put("IDENTITY#"+x.ID, map[string]types.AttributeValue{"doc": docAttr(identityDoc{
			Email: x.Email, Name: x.Name, ReplyTo: x.ReplyTo, Bcc: x.Bcc, TextSignature: x.TextSignature, HTMLSignature: x.HTMLSignature,
		}), "updatedSeq": n(seq)}, cond))
	}
	for _, c := range m.Submissions {
		x := c.After
		cond := exists
		if c.Before == nil {
			cond = absent
		}
		items = append(items, a.put("SUBMISSION#"+x.ID, map[string]types.AttributeValue{
			"doc": docAttr(submissionDoc{
				IdentityID: x.IdentityID, EmailID: x.EmailID, ThreadID: x.ThreadID, MailFrom: x.MailFrom, RcptTo: x.RcptTo,
				SendAt: x.SendAt, Blob: x.Blob, Size: x.Size, Relay: x.Relay, RelayID: x.RelayID, Status: x.Status, Attempt: x.Attempt,
				Owner: x.Owner, LeaseUntil: x.LeaseUntil, Handoff: x.Handoff, NextAttemptAt: x.NextAttemptAt,
				Recipients: x.Recipients, Reservation: x.Reservation, Destroyed: x.Destroyed,
			}),
			"status":     s(x.Status),
			"updatedSeq": n(seq),
		}, cond))
		// Delivery reports find their submission by the relay's queue id.
		if x.RelayID != "" && (c.Before == nil || c.Before.RelayID != x.RelayID) {
			items = append(items, a.put(relayKey(x.Relay, x.RelayID), map[string]types.AttributeValue{"submissionId": s(x.ID)}, nil))
		}
	}
	for _, at := range m.Attempts {
		sk := fmt.Sprintf("ATTEMPT#%s#%04d#%02d", at.SubmissionID, at.Attempt, at.Seq)
		items = append(items, a.put(sk, map[string]types.AttributeValue{
			"phase": s(at.Phase), "owner": s(at.Owner), "at": s(sortTime(at.At)), "detail": s(at.Detail), "seq": n(seq),
		}, absent))
	}
	return items
}

func relayKey(relay, id string) string { return "RELAYID#" + relay + "#" + id }

// DeliveryMarkerRetention is how long a delivery report's deduplication
// marker is kept: well beyond the 30 days the relay may redeliver
// (docs/specs/mail-flow.md#webhook-idempotency). A report replayed after
// that cannot undo a final state, which is monotonic.
const DeliveryMarkerRetention = 90 * 24 * time.Hour

// deliveryItem writes the delivery report's deduplication marker; it
// follows the quota items so its cancellation can be classified. The
// table's TTL removes it after DeliveryMarkerRetention.
func (a *Account) deliveryItem(d *model.DeliveryMarker) types.TransactWriteItem {
	return a.put("EVENT#"+d.Relay+"#"+d.EventID, map[string]types.AttributeValue{
		"submissionId": s(d.SubmissionID), "at": s(sortTime(d.At)),
		"ttl": n(a.now().Add(DeliveryMarkerRetention).Unix()),
	}, absent)
}

func deliveryDuplicate(tce *types.TransactionCanceledException, m model.Mutation) bool {
	start := 1
	if m.Receipt != nil || m.Search != nil {
		start = 2
	}
	return m.Delivery != nil && reason(tce, start+len(m.Quota)) == "ConditionalCheckFailed"
}

// SubmissionByRelayID finds the submission the relay accepted under id.
func (a *Account) SubmissionByRelayID(ctx context.Context, relay, id string) (string, bool, error) {
	var sub string
	err := a.batchGet(ctx, []string{relayKey(relay, id)}, nil, nil, func(it map[string]types.AttributeValue) error {
		sub = str(it["submissionId"])
		return nil
	})
	return sub, sub != "", err
}

// ListIdentities reads every stored identity.
func (a *Account) ListIdentities(ctx context.Context) ([]model.Identity, error) {
	var out []model.Identity
	err := a.queryPrefix(ctx, "IDENTITY#", nil, nil, func(it map[string]types.AttributeValue) error {
		var d identityDoc
		if err := json.Unmarshal([]byte(str(it["doc"])), &d); err != nil {
			return fmt.Errorf("decode identity: %w", err)
		}
		out = append(out, model.Identity{
			ID: str(it["SK"])[len("IDENTITY#"):], Email: d.Email, Name: d.Name, ReplyTo: d.ReplyTo, Bcc: d.Bcc,
			TextSignature: d.TextSignature, HTMLSignature: d.HTMLSignature,
		})
		return nil
	})
	return out, err
}

func decodeSubmission(it map[string]types.AttributeValue) (model.Submission, error) {
	var d submissionDoc
	if err := json.Unmarshal([]byte(str(it["doc"])), &d); err != nil {
		return model.Submission{}, fmt.Errorf("decode submission: %w", err)
	}
	return model.Submission{
		ID: str(it["SK"])[len("SUBMISSION#"):], IdentityID: d.IdentityID, EmailID: d.EmailID, ThreadID: d.ThreadID,
		MailFrom: d.MailFrom, RcptTo: d.RcptTo, SendAt: d.SendAt, Blob: d.Blob, Size: d.Size, Relay: d.Relay, RelayID: d.RelayID,
		Status: d.Status, Attempt: d.Attempt, Owner: d.Owner, LeaseUntil: d.LeaseUntil, Handoff: d.Handoff,
		NextAttemptAt: d.NextAttemptAt, Recipients: d.Recipients, Reservation: d.Reservation, Destroyed: d.Destroyed,
	}, nil
}

// ListSubmissions reads every submission, including hidden ones.
func (a *Account) ListSubmissions(ctx context.Context) ([]model.Submission, error) {
	var out []model.Submission
	err := a.queryPrefix(ctx, "SUBMISSION#", nil, nil, func(it map[string]types.AttributeValue) error {
		x, err := decodeSubmission(it)
		out = append(out, x)
		return err
	})
	return out, err
}

// GetSubmission reads one submission strongly.
func (a *Account) GetSubmission(ctx context.Context, id string) (model.Submission, bool, error) {
	var x model.Submission
	found := false
	err := a.batchGet(ctx, []string{"SUBMISSION#" + id}, nil, nil, func(it map[string]types.AttributeValue) error {
		var err error
		x, err = decodeSubmission(it)
		found = true
		return err
	})
	return x, found, err
}

// Attempts reads a submission's appended attempt history, in order.
func (a *Account) Attempts(ctx context.Context, submissionID string) ([]model.Attempt, error) {
	var out []model.Attempt
	prefix := "ATTEMPT#" + submissionID + "#"
	err := a.queryPrefix(ctx, prefix, nil, nil, func(it map[string]types.AttributeValue) error {
		var at model.Attempt
		at.SubmissionID = submissionID
		rest := str(it["SK"])[len(prefix):]
		if len(rest) == 7 {
			at.Attempt, _ = strconv.Atoi(rest[:4])
			at.Seq, _ = strconv.Atoi(rest[5:])
		}
		at.Phase, at.Owner, at.Detail = str(it["phase"]), str(it["owner"]), str(it["detail"])
		at.At, _ = parseSortTime(str(it["at"]))
		out = append(out, at)
		return nil
	})
	return out, err
}

// QuotaUsed reads one quota counter.
func (a *Account) QuotaUsed(ctx context.Context, key string) (int64, error) {
	var used int64
	err := a.batchGet(ctx, []string{"QUOTA#" + key}, nil, nil, func(it map[string]types.AttributeValue) error {
		used, _ = num(it["units"])
		return nil
	})
	return used, err
}
