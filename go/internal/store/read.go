package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// State reads STATE strongly. A fresh account is sequence 0, epoch 1; a
// type state missing from an older record is the account sequence.
func (a *Account) State(ctx context.Context) (model.State, error) {
	out, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &a.Table,
		Key:            a.key("STATE"),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return model.State{}, err
	}
	st := model.State{Epoch: 1}
	if v, ok := num(out.Item["seq"]); ok {
		st.Seq = v
	}
	if v, ok := num(out.Item["epoch"]); ok {
		st.Epoch = v
	}
	st.Email, st.Mailbox, st.Thread, st.Identity, st.Submission = st.Seq, st.Seq, st.Seq, st.Seq, st.Seq
	if v, ok := num(out.Item["emailState"]); ok {
		st.Email = v
	}
	if v, ok := num(out.Item["mailboxState"]); ok {
		st.Mailbox = v
	}
	if v, ok := num(out.Item["threadState"]); ok {
		st.Thread = v
	}
	if v, ok := num(out.Item["identityState"]); ok {
		st.Identity = v
	}
	if v, ok := num(out.Item["submissionState"]); ok {
		st.Submission = v
	}
	return st, nil
}

// aliased builds a projection in which every attribute is aliased:
// several (blob, size, from, to) are DynamoDB reserved words.
func aliased(attrs ...string) (string, map[string]string) {
	names := map[string]string{}
	refs := make([]string, len(attrs))
	for i, at := range attrs {
		refs[i] = "#" + at
		names["#"+at] = at
	}
	return strings.Join(refs, ", "), names
}

var emailAttributes = []string{"emailId", "threadId", "mailboxIds", "keywords", "receivedAt", "size", "blob", "source",
	"subject", "from", "to", "cc", "sentAt", "messageId", "inReplyTo", "references", "sesMessageId"}

var emailProjection, emailNames = aliased(emailAttributes...)

// queryPrefix reads every item whose sort key starts with prefix, strongly,
// page by page.
func (a *Account) queryPrefix(ctx context.Context, prefix string, projection *string, names map[string]string, each func(map[string]types.AttributeValue) error) error {
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:                 &a.Table,
			ConsistentRead:            aws.Bool(true),
			KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(a.pk()), ":prefix": s(prefix)},
			ProjectionExpression:      projection,
			ExpressionAttributeNames:  names,
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			if err := each(it); err != nil {
				return err
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return nil
		}
		start = page.LastEvaluatedKey
	}
}

// ListEmails reads every EMAIL record.
func (a *Account) ListEmails(ctx context.Context) ([]model.Email, error) {
	var out []model.Email
	err := a.queryPrefix(ctx, "EMAIL#", &emailProjection, emailNames, func(it map[string]types.AttributeValue) error {
		m, err := decodeEmail(it)
		out = append(out, m)
		return err
	})
	return out, err
}

// batchGet reads up to 100 keys strongly, retrying unprocessed keys.
func (a *Account) batchGet(ctx context.Context, sks []string, projection *string, names map[string]string, each func(map[string]types.AttributeValue) error) error {
	for len(sks) > 0 {
		chunk := sks[:min(len(sks), 100)]
		sks = sks[len(chunk):]
		keys := make([]map[string]types.AttributeValue, len(chunk))
		for i, sk := range chunk {
			keys[i] = a.key(sk)
		}
		req := map[string]types.KeysAndAttributes{a.Table: {
			Keys: keys, ConsistentRead: aws.Bool(true), ProjectionExpression: projection, ExpressionAttributeNames: names,
		}}
		for attempt := 0; len(req) > 0; attempt++ {
			if attempt > 0 {
				if attempt > 5 {
					return errors.New("unprocessed keys after retries")
				}
				if err := sleep(ctx, time.Duration(attempt*attempt)*20*time.Millisecond); err != nil {
					return err
				}
			}
			res, err := a.DB.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: req})
			if err != nil {
				return err
			}
			for _, it := range res.Responses[a.Table] {
				if err := each(it); err != nil {
					return err
				}
			}
			req = res.UnprocessedKeys
		}
	}
	return nil
}

// GetEmails batch-reads EMAIL records strongly.
func (a *Account) GetEmails(ctx context.Context, ids []string) (map[string]model.Email, error) {
	out := map[string]model.Email{}
	sks := make([]string, 0, len(ids))
	for _, id := range ids {
		sks = append(sks, "EMAIL#"+id)
	}
	err := a.batchGet(ctx, sks, &emailProjection, emailNames, func(it map[string]types.AttributeValue) error {
		m, err := decodeEmail(it)
		out[m.ID] = m
		return err
	})
	return out, err
}

func decodeEmail(it map[string]types.AttributeValue) (model.Email, error) {
	m := model.Email{
		ID:         str(it["emailId"]),
		ThreadID:   str(it["threadId"]),
		MailboxIDs: boolMapOf(it["mailboxIds"]),
		Keywords:   boolMapOf(it["keywords"]),
		Source:     str(it["source"]),
		Subject:    str(it["subject"]),
		From:       strList(it["from"]),
		To:         strList(it["to"]),
		Cc:         strList(it["cc"]),
		MessageID:  str(it["messageId"]),
		InReplyTo:  str(it["inReplyTo"]),
		References: str(it["references"]),
	}
	if m.ID == "" {
		return m, errors.New("email record without emailId")
	}
	if m.Source == "" {
		m.Source = model.SourceReceipt
	}
	if v := str(it["sesMessageId"]); v != "" {
		m.Envelope = &model.Envelope{SESMessageID: v}
	}
	var err error
	if m.ReceivedAt, err = parseSortTime(str(it["receivedAt"])); err != nil {
		return m, fmt.Errorf("email %s receivedAt: %w", m.ID, err)
	}
	if v := str(it["sentAt"]); v != "" {
		m.SentAt, _ = time.Parse(time.RFC3339, v)
	}
	m.Size, _ = num(it["size"])
	if bl, ok := it["blob"].(*types.AttributeValueMemberM); ok {
		m.Blob = model.BlobRef{Bucket: str(bl.Value["bucket"]), Key: str(bl.Value["key"]), VersionID: str(bl.Value["versionId"])}
	}
	return m, nil
}

var mailboxProjection, mailboxNames = aliased("SK", "name", "parentId", "role", "sortOrder", "isSubscribed")

// ListMailboxes reads the stored MAILBOX records (system overrides and
// custom mailboxes); model.Mailboxes merges them with the defaults.
func (a *Account) ListMailboxes(ctx context.Context) ([]model.Mailbox, error) {
	var out []model.Mailbox
	err := a.queryPrefix(ctx, "MAILBOX#", &mailboxProjection, mailboxNames, func(it map[string]types.AttributeValue) error {
		m := model.Mailbox{
			ID:           strings.TrimPrefix(str(it["SK"]), "MAILBOX#"),
			Name:         str(it["name"]),
			ParentID:     str(it["parentId"]),
			Role:         str(it["role"]),
			IsSubscribed: boolean(it["isSubscribed"]),
		}
		m.SortOrder, _ = num(it["sortOrder"])
		out = append(out, m)
		return nil
	})
	return out, err
}

var countProjection, countNames = aliased("SK", "totalEmails", "unreadEmails", "totalThreads", "unreadThreads")

// MailboxCounts reads every COUNT record; a mailbox without one is zero.
func (a *Account) MailboxCounts(ctx context.Context) (map[string]model.Counts, error) {
	out := map[string]model.Counts{}
	err := a.queryPrefix(ctx, "COUNT#", &countProjection, countNames, func(it map[string]types.AttributeValue) error {
		var c model.Counts
		c.TotalEmails, _ = num(it["totalEmails"])
		c.UnreadEmails, _ = num(it["unreadEmails"])
		c.TotalThreads, _ = num(it["totalThreads"])
		c.UnreadThreads, _ = num(it["unreadThreads"])
		out[strings.TrimPrefix(str(it["SK"]), "COUNT#")] = c
		return nil
	})
	return out, err
}

var memberProjection, memberNames = aliased("emailId")

// ThreadMembers returns each thread's Email ids, oldest first; a thread
// without members is absent.
func (a *Account) ThreadMembers(ctx context.Context, threadIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	var mu sync.Mutex
	err := forEach(ctx, threadIDs, 8, func(ctx context.Context, t string) error {
		var ids []string
		err := a.queryPrefix(ctx, "MEMBER#"+t+"#", &memberProjection, memberNames, func(it map[string]types.AttributeValue) error {
			ids = append(ids, str(it["emailId"]))
			return nil
		})
		if err == nil && len(ids) > 0 {
			mu.Lock()
			out[t] = ids
			mu.Unlock()
		}
		return err
	})
	return out, err
}

// Changes reads CHANGE records with after < seq <= upto.
func (a *Account) Changes(ctx context.Context, after, upto int64) ([]model.Change, error) {
	if after >= upto {
		return nil, nil
	}
	var out []model.Change
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:              &a.Table,
			ConsistentRead:         aws.Bool(true),
			KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": s(a.pk()), ":from": s("CHANGE#" + padded(after+1)), ":to": s("CHANGE#" + padded(upto)),
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			sk := str(it["SK"])
			seq, err := strconv.ParseInt(strings.TrimPrefix(sk, "CHANGE#"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed change key %q", sk)
			}
			ch := model.Change{
				Seq: seq, Created: idsByType(it["created"]), Updated: idsByType(it["updated"]), Destroyed: idsByType(it["destroyed"]),
			}
			if at, ok := num(it["at"]); ok {
				ch.At = time.Unix(at, 0).UTC()
			}
			out = append(out, ch)
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

var refProjection, refNames = aliased("SK", "emailId", "threadId", "receivedAt", "addrs")

func decodeRef(it map[string]types.AttributeValue) model.ThreadRef {
	r := model.ThreadRef{EmailID: str(it["emailId"]), ThreadID: str(it["threadId"]), Addresses: strList(it["addrs"])}
	r.ReceivedAt, _ = parseSortTime(str(it["receivedAt"]))
	return r
}

// ByMessageID implements model.ThreadLookup over MESSAGEID records.
func (a *Account) ByMessageID(ctx context.Context, keys []string) (map[string][]model.ThreadRef, error) {
	out := map[string][]model.ThreadRef{}
	var mu sync.Mutex
	err := forEach(ctx, keys, 8, func(ctx context.Context, k string) error {
		var refs []model.ThreadRef
		err := a.queryPrefix(ctx, "MESSAGEID#"+k+"#", &refProjection, refNames, func(it map[string]types.AttributeValue) error {
			refs = append(refs, decodeRef(it))
			return nil
		})
		if err == nil && len(refs) > 0 {
			mu.Lock()
			out[k] = refs
			mu.Unlock()
		}
		return err
	})
	return out, err
}

// BySubject implements model.ThreadLookup over SUBJECT records.
func (a *Account) BySubject(ctx context.Context, key string) ([]model.ThreadRef, error) {
	if key == "" {
		return nil, nil
	}
	var refs []model.ThreadRef
	err := a.queryPrefix(ctx, "SUBJECT#"+key+"#", &refProjection, refNames, func(it map[string]types.AttributeValue) error {
		refs = append(refs, decodeRef(it))
		return nil
	})
	return refs, err
}

var uploadProjection, uploadNames = aliased("SK", "type", "size", "bucket", "key", "created", "expiresAt")

// GetUpload reads one upload record strongly; ok is false if it is absent.
func (a *Account) GetUpload(ctx context.Context, id string) (model.Upload, bool, error) {
	out, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &a.Table, Key: a.key("BLOB#" + id), ConsistentRead: aws.Bool(true),
		ProjectionExpression: &uploadProjection, ExpressionAttributeNames: uploadNames,
	})
	if err != nil || out.Item == nil {
		return model.Upload{}, false, err
	}
	u := model.Upload{ID: id, Type: str(out.Item["type"]), Bucket: str(out.Item["bucket"]), Key: str(out.Item["key"])}
	u.Size, _ = num(out.Item["size"])
	u.Created, _ = parseSortTime(str(out.Item["created"]))
	u.ExpiresAt, _ = parseSortTime(str(out.Item["expiresAt"]))
	return u, true, nil
}

// derived is the strongly read state a commit plan needs.
type derived struct {
	mbt  map[model.MBThreadKey]model.MBThread
	size map[string]int64
}

func (d derived) MBThread(k model.MBThreadKey) model.MBThread { return d.mbt[k] }
func (d derived) ThreadSize(t string) int64                   { return d.size[t] }

var mbtProjection, mbtNames = aliased("SK", "emails", "unread")

func mbtKey(k model.MBThreadKey) string { return "MAILBOXTHREAD#" + k.Mailbox + "#" + k.Thread }

func (a *Account) readDerived(ctx context.Context, summaries []model.MBThreadKey, threads []string) (derived, error) {
	d := derived{mbt: map[model.MBThreadKey]model.MBThread{}, size: map[string]int64{}}
	byKey := map[string]model.MBThreadKey{}
	sks := make([]string, 0, len(summaries))
	for _, k := range summaries {
		byKey[mbtKey(k)] = k
		sks = append(sks, mbtKey(k))
	}
	err := a.batchGet(ctx, sks, &mbtProjection, mbtNames, func(it map[string]types.AttributeValue) error {
		var v model.MBThread
		v.Emails, _ = num(it["emails"])
		v.Unread, _ = num(it["unread"])
		d.mbt[byKey[str(it["SK"])]] = v
		return nil
	})
	if err != nil {
		return d, err
	}
	members, err := a.ThreadMembers(ctx, threads)
	if err != nil {
		return d, err
	}
	for _, t := range threads {
		d.size[t] = int64(len(members[t]))
	}
	return d, nil
}

// forEach runs fn over items with bounded concurrency and returns the first
// error.
func forEach(ctx context.Context, items []string, limit int, fn func(context.Context, string) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for _, it := range slices.Compact(slices.Sorted(slices.Values(items))) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, it); err != nil {
				mu.Lock()
				if first == nil {
					first = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
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

// SearchIndex reads the registered search generation strongly.
func (a *Account) SearchIndex(ctx context.Context) (model.SearchIndex, bool, error) {
	out, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: &a.Table, Key: a.key("SEARCH"), ConsistentRead: aws.Bool(true)})
	if err != nil || out.Item == nil {
		return model.SearchIndex{}, false, err
	}
	it := out.Item
	x := model.SearchIndex{Generation: str(it["generation"]), Key: str(it["key"]), VersionID: str(it["versionId"]), ETag: str(it["etag"])}
	x.Seq, _ = num(it["seq"])
	x.Epoch, _ = num(it["epoch"])
	if t, err := parseSortTime(str(it["publishedAt"])); err == nil {
		x.PublishedAt = t
	}
	return x, true, nil
}
