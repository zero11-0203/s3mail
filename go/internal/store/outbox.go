package store

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// OutboxEntry is committed dispatch work (OUTBOX#<seq>#<destination>).
type OutboxEntry struct {
	Key         string // the sort key, used to acknowledge it
	Destination string // index, submit or events
	IDs         []string
	Trace       string // X-Ray header of the committing request, or ""
}

// Outbox reads every pending entry in commit order.
func (a *Account) Outbox(ctx context.Context) ([]OutboxEntry, error) {
	var out []OutboxEntry
	err := a.queryPrefix(ctx, "OUTBOX#", nil, nil, func(it map[string]types.AttributeValue) error {
		sk := str(it["SK"])
		e := OutboxEntry{Key: sk, Destination: sk[strings.LastIndex(sk, "#")+1:], Trace: str(it["trace"])}
		switch e.Destination {
		case "index":
			e.IDs = strList(it["emailIds"])
		case "submit":
			e.IDs = strList(it["submissionIds"])
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// Outbox entries are internal bookkeeping: once committed they are never
// changed, and removing one changes nothing JMAP-visible, so the
// acknowledgement is a plain delete rather than a guarded commit
// (docs/specs/storage-sync.md#idempotency-and-durable-work).

// Deleter is the client operation AckOutbox needs; only the dispatcher's
// role is granted it for this purpose.
type Deleter interface {
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// AckOutbox removes an entry after its destination accepted the work.
func (a *Account) AckOutbox(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "OUTBOX#") {
		return errors.New("not an outbox key")
	}
	db, ok := a.DB.(Deleter)
	if !ok {
		return errors.New("store client cannot delete")
	}
	_, err := db.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &a.Table, Key: a.key(key)})
	return err
}
