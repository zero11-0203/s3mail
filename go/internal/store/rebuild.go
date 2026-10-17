package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// derivedPrefixes are the records RebuildDerived recomputes from EMAIL#.
var derivedPrefixes = []string{"LIST#", "MEMBER#", "MAILBOXTHREAD#", "COUNT#", "MESSAGEID#", "SUBJECT#"}

// RebuildDerived recomputes every listing, membership, threading-index,
// summary and counter record from the stored Emails, in one transaction
// guarded by the account sequence, and records a counter-only change so
// clients refetch counts. Emails keep their ids and threads. It is for
// small accounts and schema migrations: it fails with model.ErrTooLarge
// rather than write a partial rebuild.
func (a *Account) RebuildDerived(ctx context.Context) (model.State, error) {
	for attempt := range maxCommitAttempts {
		if attempt > 0 {
			if err := sleep(ctx, a.backoff(attempt)); err != nil {
				return model.State{}, err
			}
		}
		st, err := a.rebuildOnce(ctx)
		if errors.Is(err, model.ErrStateChanged) {
			continue
		}
		return st, err
	}
	return model.State{}, ErrContention
}

func (a *Account) rebuildOnce(ctx context.Context) (model.State, error) {
	prev, err := a.State(ctx)
	if err != nil {
		return model.State{}, err
	}
	emails, err := a.ListEmails(ctx)
	if err != nil {
		return model.State{}, err
	}
	existing := map[string]bool{}
	keyOnly := aws.String("SK")
	for _, p := range derivedPrefixes {
		if err := a.queryPrefix(ctx, p, keyOnly, nil, func(it map[string]types.AttributeValue) error {
			existing[str(it["SK"])] = true
			return nil
		}); err != nil {
			return model.State{}, err
		}
	}

	want := map[string]map[string]types.AttributeValue{}
	mbt := map[model.MBThreadKey]model.MBThread{}
	for i := range emails {
		e := &emails[i]
		for _, it := range a.emailItems(model.EmailChange{After: e}, 0) {
			if sk := str(it.Put.Item["SK"]); !strings.HasPrefix(sk, "EMAIL#") {
				want[sk] = it.Put.Item
			}
		}
		for mb := range e.MailboxIDs {
			k := model.MBThreadKey{Mailbox: mb, Thread: e.ThreadID}
			t := mbt[k]
			t.Emails++
			if e.Unread() {
				t.Unread++
			}
			mbt[k] = t
		}
	}
	counts := map[string]model.Counts{}
	for k, t := range mbt {
		want[mbtKey(k)] = mergeKey(a.key(mbtKey(k)), map[string]types.AttributeValue{"emails": n(t.Emails), "unread": n(t.Unread)})
		c := counts[k.Mailbox]
		c.TotalEmails += t.Emails
		c.UnreadEmails += t.Unread
		c.TotalThreads++
		if t.Unread > 0 {
			c.UnreadThreads++
		}
		counts[k.Mailbox] = c
	}
	for mb, c := range counts {
		want["COUNT#"+mb] = mergeKey(a.key("COUNT#"+mb), map[string]types.AttributeValue{
			"totalEmails": n(c.TotalEmails), "unreadEmails": n(c.UnreadEmails),
			"totalThreads": n(c.TotalThreads), "unreadThreads": n(c.UnreadThreads),
		})
	}

	next := prev
	next.Seq = prev.Seq + 1
	next.Mailbox = next.Seq
	state := &types.Update{
		TableName:           &a.Table,
		Key:                 a.key("STATE"),
		UpdateExpression:    aws.String("SET seq = :next, mailboxState = :next"),
		ConditionExpression: aws.String("seq = :prev"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":next": n(next.Seq), ":prev": n(prev.Seq),
		},
	}
	items := []types.TransactWriteItem{{Update: state}}
	for _, sk := range slices.Sorted(maps.Keys(want)) {
		items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: &a.Table, Item: want[sk]}})
	}
	for _, sk := range slices.Sorted(maps.Keys(existing)) {
		if want[sk] == nil {
			items = append(items, a.del(sk, nil))
		}
	}
	mailboxes := slices.Sorted(maps.Keys(counts))
	for sk := range existing {
		if mb, ok := strings.CutPrefix(sk, "COUNT#"); ok && counts[mb] == (model.Counts{}) {
			mailboxes = append(mailboxes, mb)
		}
	}
	items = append(items, a.put("CHANGE#"+padded(next.Seq), map[string]types.AttributeValue{
		"at":        n(a.now().Unix()),
		"created":   idsByTypeAttr(nil),
		"updated":   idsByTypeAttr(map[string][]string{"MailboxCounts": mailboxes}),
		"destroyed": idsByTypeAttr(nil),
	}, absent))
	if len(items) > maxTransactItems {
		return model.State{}, fmt.Errorf("rebuild needs %d items: %w", len(items), model.ErrTooLarge)
	}
	_, err = a.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) && (reason(tce, 0) == "ConditionalCheckFailed" || anyReason(tce, "TransactionConflict")) {
		return model.State{}, model.ErrStateChanged
	}
	if err != nil {
		return model.State{}, err
	}
	return next, nil
}

func mergeKey(key, attrs map[string]types.AttributeValue) map[string]types.AttributeValue {
	maps.Copy(key, attrs)
	return key
}
