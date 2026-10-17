package store

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// maxTransactItems is DynamoDB's TransactWriteItems limit.
const maxTransactItems = 100

// Commit writes one mutation in one transaction guarded by prev.Seq: the
// objects, their listing/membership/index records, counters, the change
// record, outbox work and the next STATE
// (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
// It returns model.ErrStateChanged when another commit won, and
// model.ErrDuplicateReceipt when the receipt marker already exists.
func (a *Account) Commit(ctx context.Context, prev model.State, m model.Mutation) (model.State, error) {
	summaries, threads := m.Keys()
	d, err := a.readDerived(ctx, summaries, threads)
	if err != nil {
		return model.State{}, fmt.Errorf("read derived state: %w", err)
	}
	ef := model.Plan(m, d)
	next := ef.Next(prev)
	items := a.build(prev, next, m, ef)
	if len(items) > maxTransactItems {
		return model.State{}, model.ErrTooLarge
	}
	_, err = a.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		switch {
		case reason(tce, 0) == "ConditionalCheckFailed" || anyReason(tce, "TransactionConflict"):
			return model.State{}, model.ErrStateChanged
		case m.Receipt != nil && reason(tce, 1) == "ConditionalCheckFailed":
			return model.State{}, model.ErrDuplicateReceipt
		case m.Search != nil && reason(tce, 1) == "ConditionalCheckFailed":
			return model.State{}, model.ErrSearchConflict
		}
		return model.State{}, fmt.Errorf("commit canceled: %w", err)
	}
	if err != nil {
		return model.State{}, err
	}
	return next, nil
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

var (
	absent = aws.String("attribute_not_exists(PK)")
	exists = aws.String("attribute_exists(PK)")
)

func (a *Account) put(sk string, attrs map[string]types.AttributeValue, cond *string) types.TransactWriteItem {
	item := a.key(sk)
	maps.Copy(item, attrs)
	return types.TransactWriteItem{Put: &types.Put{TableName: &a.Table, Item: item, ConditionExpression: cond}}
}

func (a *Account) del(sk string, cond *string) types.TransactWriteItem {
	return types.TransactWriteItem{Delete: &types.Delete{TableName: &a.Table, Key: a.key(sk), ConditionExpression: cond}}
}

// build returns the transaction items; STATE is item 0 and the receipt
// marker or search registration, when present, item 1, for cancellation
// classification.
func (a *Account) build(prev, next model.State, m model.Mutation, ef model.Effects) []types.TransactWriteItem {
	state := &types.Update{
		TableName:        &a.Table,
		Key:              a.key("STATE"),
		UpdateExpression: aws.String("SET seq = :next, emailState = :e, mailboxState = :m, threadState = :t, epoch = if_not_exists(epoch, :one)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":next": n(next.Seq), ":e": n(next.Email), ":m": n(next.Mailbox), ":t": n(next.Thread), ":one": n(1),
		},
	}
	if prev.Seq == 0 {
		state.ConditionExpression = aws.String("attribute_not_exists(seq)")
	} else {
		state.ConditionExpression = aws.String("seq = :prev")
		state.ExpressionAttributeValues[":prev"] = n(prev.Seq)
	}
	items := []types.TransactWriteItem{{Update: state}}

	if r := m.Receipt; r != nil {
		// The marker outlives the Email; it holds no sender, recipients or
		// content (docs/operations/recovery.md#retention-deletion-and-garbage-collection).
		items = append(items, a.put("RECEIPT#"+r.SESMessageID, map[string]types.AttributeValue{
			"emailId": s(r.EmailID), "bucket": s(r.Blob.Bucket), "key": s(r.Blob.Key), "versionId": s(r.Blob.VersionID),
			"receivedAt": s(sortTime(r.ReceivedAt)), "seq": n(next.Seq), "parserVersion": n(int64(r.ParserVersion)),
		}, absent))
	}

	if r := m.Search; r != nil {
		cond := aws.String("attribute_not_exists(generation)")
		var vals map[string]types.AttributeValue
		if r.Prev != "" {
			cond = aws.String("generation = :prevGen")
			vals = map[string]types.AttributeValue{":prevGen": s(r.Prev)}
		}
		x := r.Next
		item := a.key("SEARCH")
		maps.Copy(item, map[string]types.AttributeValue{
			"generation": s(x.Generation), "seq": n(x.Seq), "epoch": n(x.Epoch), "key": s(x.Key),
			"versionId": s(x.VersionID), "etag": s(x.ETag), "publishedAt": s(sortTime(x.PublishedAt)), "registeredSeq": n(next.Seq),
		})
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName: &a.Table, Item: item, ConditionExpression: cond, ExpressionAttributeValues: vals,
		}})
	}

	for _, c := range m.Emails {
		items = append(items, a.emailItems(c, next.Seq)...)
	}

	destroyedMailboxes := map[string]bool{}
	for _, c := range m.Mailboxes {
		if c.After == nil {
			destroyedMailboxes[c.Before.ID] = true
			items = append(items, a.del("MAILBOX#"+c.Before.ID, nil), a.del("COUNT#"+c.Before.ID, nil))
			continue
		}
		mb := c.After
		attrs := map[string]types.AttributeValue{"name": s(mb.Name), "sortOrder": n(mb.SortOrder), "isSubscribed": b(mb.IsSubscribed)}
		if mb.ParentID != "" {
			attrs["parentId"] = s(mb.ParentID)
		}
		if mb.Role != "" {
			attrs["role"] = s(mb.Role)
		}
		items = append(items, a.put("MAILBOX#"+mb.ID, attrs, nil))
	}

	for _, k := range slices.SortedFunc(maps.Keys(ef.MBThreads), func(x, y model.MBThreadKey) int {
		return strings.Compare(mbtKey(x), mbtKey(y))
	}) {
		v := ef.MBThreads[k]
		if v == (model.MBThread{}) {
			items = append(items, a.del(mbtKey(k), nil))
		} else {
			items = append(items, a.put(mbtKey(k), map[string]types.AttributeValue{"emails": n(v.Emails), "unread": n(v.Unread)}, nil))
		}
	}
	for _, mb := range slices.Sorted(maps.Keys(ef.Counts)) {
		if destroyedMailboxes[mb] {
			continue
		}
		c := ef.Counts[mb]
		items = append(items, types.TransactWriteItem{Update: &types.Update{
			TableName:        &a.Table,
			Key:              a.key("COUNT#" + mb),
			UpdateExpression: aws.String("ADD totalEmails :a, unreadEmails :b, totalThreads :c, unreadThreads :d"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":a": n(c.TotalEmails), ":b": n(c.UnreadEmails), ":c": n(c.TotalThreads), ":d": n(c.UnreadThreads),
			},
		}})
	}

	for _, u := range m.Uploads {
		items = append(items, a.put("BLOB#"+u.ID, map[string]types.AttributeValue{
			"type": s(u.Type), "size": n(u.Size), "bucket": s(u.Bucket), "key": s(u.Key),
			"created": s(sortTime(u.Created)), "expiresAt": s(sortTime(u.ExpiresAt)),
			// The table's TTL only cleans up; expiry is checked on use.
			"ttl": n(u.ExpiresAt.Add(24 * time.Hour).Unix()),
		}, absent))
	}

	change := map[string]types.AttributeValue{
		"at":        n(a.now().Unix()),
		"created":   idsByTypeAttr(ef.Change.Created),
		"updated":   idsByTypeAttr(ef.Change.Updated),
		"destroyed": idsByTypeAttr(ef.Change.Destroyed),
	}
	if len(ef.SearchDirty) > 0 {
		change["searchDirty"] = l(ef.SearchDirty...)
	}
	items = append(items, a.put("CHANGE#"+padded(next.Seq), change, absent))
	if len(ef.SearchDirty) > 0 {
		items = append(items, a.put("OUTBOX#"+padded(next.Seq)+"#index", map[string]types.AttributeValue{"emailIds": l(ef.SearchDirty...)}, absent))
	}
	if len(m.Events) > 0 {
		events := make([]types.AttributeValue, len(m.Events))
		for i, e := range m.Events {
			events[i] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
				"eventId": s(e.ID), "type": s(e.Type), "emailId": s(e.EmailID), "mailboxIds": l(e.MailboxIDs...),
			}}
		}
		items = append(items, a.put("OUTBOX#"+padded(next.Seq)+"#events", map[string]types.AttributeValue{
			"events": &types.AttributeValueMemberL{Value: events},
		}, absent))
	}
	return items
}

func listKey(mailbox string, e *model.Email) string {
	return "LIST#" + mailbox + "#" + sortTime(e.ReceivedAt) + "#" + e.ID
}

func memberKey(e *model.Email) string {
	return "MEMBER#" + e.ThreadID + "#" + sortTime(e.ReceivedAt) + "#" + e.ID
}

// emailItems writes one Email change with its listing, membership and
// threading index records.
func (a *Account) emailItems(c model.EmailChange, seq int64) []types.TransactWriteItem {
	var items []types.TransactWriteItem
	switch {
	case c.Before == nil:
		e := c.After
		items = append(items, a.put("EMAIL#"+e.ID, emailItem(e, seq), absent))
		for mb := range e.MailboxIDs {
			items = append(items, a.put(listKey(mb, e), map[string]types.AttributeValue{"emailId": s(e.ID)}, nil))
		}
		items = append(items, a.put(memberKey(e), map[string]types.AttributeValue{"emailId": s(e.ID), "receivedAt": s(sortTime(e.ReceivedAt))}, nil))
		ref := map[string]types.AttributeValue{"emailId": s(e.ID), "threadId": s(e.ThreadID), "receivedAt": s(sortTime(e.ReceivedAt))}
		for _, k := range e.MessageIDKeys() {
			items = append(items, a.put("MESSAGEID#"+k+"#"+e.ID, ref, nil))
		}
		if k := e.SubjectKey(); k != "" {
			subj := maps.Clone(ref)
			subj["addrs"] = l(e.Addresses()...)
			items = append(items, a.put("SUBJECT#"+k+"#"+e.ID, subj, nil))
		}
	case c.After == nil:
		e := c.Before
		items = append(items, a.del("EMAIL#"+e.ID, exists))
		for mb := range e.MailboxIDs {
			items = append(items, a.del(listKey(mb, e), nil))
		}
		items = append(items, a.del(memberKey(e), nil))
		for _, k := range e.MessageIDKeys() {
			items = append(items, a.del("MESSAGEID#"+k+"#"+e.ID, nil))
		}
		if k := e.SubjectKey(); k != "" {
			items = append(items, a.del("SUBJECT#"+k+"#"+e.ID, nil))
		}
		// Content-free deletion evidence: the exact original reference and
		// the deleting commit (docs/operations/recovery.md).
		marker := map[string]types.AttributeValue{
			"bucket": s(e.Blob.Bucket), "key": s(e.Blob.Key), "versionId": s(e.Blob.VersionID), "deletedSeq": n(seq),
		}
		if e.Envelope != nil && e.Envelope.SESMessageID != "" {
			marker["sesMessageId"] = s(e.Envelope.SESMessageID)
		}
		items = append(items, a.put("DELETED#"+e.ID, marker, nil))
	default:
		before, after := c.Before, c.After
		items = append(items, types.TransactWriteItem{Update: &types.Update{
			TableName:           &a.Table,
			Key:                 a.key("EMAIL#" + after.ID),
			UpdateExpression:    aws.String("SET keywords = :k, mailboxIds = :m, updatedSeq = :seq"),
			ConditionExpression: exists,
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":k": boolMapAttr(after.Keywords), ":m": boolMapAttr(after.MailboxIDs), ":seq": n(seq),
			},
		}})
		for _, mb := range slices.Sorted(maps.Keys(before.MailboxIDs)) {
			if !after.MailboxIDs[mb] {
				items = append(items, a.del(listKey(mb, before), nil))
			}
		}
		for _, mb := range slices.Sorted(maps.Keys(after.MailboxIDs)) {
			if !before.MailboxIDs[mb] {
				items = append(items, a.put(listKey(mb, after), map[string]types.AttributeValue{"emailId": s(after.ID)}, nil))
			}
		}
	}
	return items
}

func emailItem(e *model.Email, seq int64) map[string]types.AttributeValue {
	item := map[string]types.AttributeValue{
		"emailId":    s(e.ID),
		"threadId":   s(e.ThreadID),
		"mailboxIds": boolMapAttr(e.MailboxIDs),
		"keywords":   boolMapAttr(e.Keywords),
		"receivedAt": s(sortTime(e.ReceivedAt)),
		"size":       n(e.Size),
		"blob":       strMapAttr(map[string]string{"bucket": e.Blob.Bucket, "key": e.Blob.Key, "versionId": e.Blob.VersionID}),
		"source":     s(e.Source),
		"subject":    s(e.Subject),
		"from":       l(e.From...),
		"to":         l(e.To...),
		"cc":         l(e.Cc...),
		"messageId":  s(e.MessageID),
		"inReplyTo":  s(e.InReplyTo),
		"references": s(e.References),
		"createdSeq": n(seq),
	}
	if !e.SentAt.IsZero() {
		item["sentAt"] = s(e.SentAt.UTC().Format(time.RFC3339))
	}
	if v := e.Envelope; v != nil {
		item["sesMessageId"] = s(v.SESMessageID)
		item["envelopeSender"] = s(v.EnvelopeSender)
		item["envelopeRecipients"] = l(v.EnvelopeRecipients...)
		item["deliveredTo"] = l(v.DeliveredTo...)
		item["verdicts"] = strMapAttr(v.Verdicts)
		item["replyTo"] = l(v.ReplyTo...)
		item["incomplete"] = l(v.Incomplete...)
		item["parserVersion"] = n(int64(v.ParserVersion))
		if v.Reason != "" {
			item["dispositionReason"] = s(v.Reason)
		}
	}
	return item
}
