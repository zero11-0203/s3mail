// Package store implements the mail table commit protocol
// (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract)
// and reads of the permanent original bucket.
//
// Record layout under PK = ACCOUNT#<account>:
//
//	STATE                              seq, epoch, emailState, mailboxState, threadState
//	EMAIL#<emailId>                    Email metadata
//	LIST#<mailbox>#<receivedAt>#<id>   mailbox listing
//	MEMBER#<thread>#<receivedAt>#<id>  thread membership, oldest first
//	MAILBOXTHREAD#<mailbox>#<thread>   emails, unread: per-mailbox thread summary
//	COUNT#<mailbox>                    mailbox counters
//	MESSAGEID#<hash>#<emailId>         Message-ID / reference lookup
//	SUBJECT#<hash>#<emailId>           base-subject lookup
//	MAILBOX#<id>                       stored mailbox (system overrides and custom)
//	BLOB#<id>                          upload record
//	DELETED#<emailId>                  content-free deletion evidence
//	RECEIPT#<sesMessageId>             receipt replay marker
//	CHANGE#<seq>                       change record
//	OUTBOX#<seq>#<destination>         committed dispatch work
//	SEARCH                             registered search index generation
package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DB is the subset of the DynamoDB client the store uses.
type DB interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchGetItem(ctx context.Context, in *dynamodb.BatchGetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, opts ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Account is one account partition of the mail table.
type Account struct {
	DB        DB
	Table     string
	AccountID string
	// Backoff before retrying a sequence conflict; nil uses bounded jitter.
	Backoff func(attempt int) time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

func (a *Account) pk() string { return "ACCOUNT#" + a.AccountID }

func (a *Account) key(sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"PK": s(a.pk()), "SK": s(sk)}
}

func padded(seq int64) string { return fmt.Sprintf("%020d", seq) }

const sortTimeLayout = "2006-01-02T15:04:05.000000000Z"

// sortTime is fixed-width so keys sort chronologically.
func sortTime(t time.Time) string { return t.UTC().Format(sortTimeLayout) }

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func b(v bool) types.AttributeValue { return &types.AttributeValueMemberBOOL{Value: v} }

func l(vs ...string) types.AttributeValue {
	list := make([]types.AttributeValue, len(vs))
	for i, v := range vs {
		list[i] = s(v)
	}
	return &types.AttributeValueMemberL{Value: list}
}

func str(v types.AttributeValue) string {
	if x, ok := v.(*types.AttributeValueMemberS); ok {
		return x.Value
	}
	return ""
}

func num(v types.AttributeValue) (int64, bool) {
	x, ok := v.(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(x.Value, 10, 64)
	return n, err == nil
}

func boolean(v types.AttributeValue) bool {
	x, ok := v.(*types.AttributeValueMemberBOOL)
	return ok && x.Value
}

func strList(v types.AttributeValue) []string {
	x, ok := v.(*types.AttributeValueMemberL)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(x.Value))
	for _, e := range x.Value {
		out = append(out, str(e))
	}
	return out
}

func boolMapOf(v types.AttributeValue) map[string]bool {
	out := map[string]bool{}
	if x, ok := v.(*types.AttributeValueMemberM); ok {
		for k, e := range x.Value {
			if boolean(e) {
				out[k] = true
			}
		}
	}
	return out
}

func boolMapAttr(m map[string]bool) types.AttributeValue {
	out := map[string]types.AttributeValue{}
	for k, v := range m {
		if v {
			out[k] = b(true)
		}
	}
	return &types.AttributeValueMemberM{Value: out}
}

func strMapAttr(m map[string]string) types.AttributeValue {
	out := map[string]types.AttributeValue{}
	for k, v := range m {
		out[k] = s(v)
	}
	return &types.AttributeValueMemberM{Value: out}
}

func idsByType(v types.AttributeValue) map[string][]string {
	out := map[string][]string{}
	if x, ok := v.(*types.AttributeValueMemberM); ok {
		for t, ids := range x.Value {
			out[t] = strList(ids)
		}
	}
	return out
}

func idsByTypeAttr(m map[string][]string) types.AttributeValue {
	out := map[string]types.AttributeValue{}
	for t, ids := range m {
		if len(ids) > 0 {
			out[t] = l(ids...)
		}
	}
	return &types.AttributeValueMemberM{Value: out}
}

func parseSortTime(v string) (time.Time, error) { return time.Parse(sortTimeLayout, v) }

func (a *Account) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
