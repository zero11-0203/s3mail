package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zero11-0203/s3mail/go/internal/jmap"
)

// AccountDB is the subset of the DynamoDB client the JMAP store uses.
type AccountDB interface {
	DynamoDB
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchGetItem(ctx context.Context, in *dynamodb.BatchGetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error)
}

// Account implements jmap.Store over one account partition.
type Account struct {
	DB        AccountDB
	Table     string
	AccountID string
}

var _ jmap.Store = (*Account)(nil)

func (a *Account) mail() *Mail { return &Mail{Table: a.Table, AccountID: a.AccountID} }

// State reads STATE strongly; a fresh account is sequence 0, epoch 1.
func (a *Account) State(ctx context.Context) (jmap.State, error) {
	out, err := a.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &a.Table,
		Key:            a.mail().key("STATE"),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return jmap.State{}, err
	}
	st := jmap.State{Epoch: 1}
	if v, ok := num(out.Item["seq"]); ok {
		st.Seq = v
	}
	if v, ok := num(out.Item["epoch"]); ok {
		st.Epoch = v
	}
	return st, nil
}

// Every projected attribute is aliased: several (blob, size, from, to)
// are DynamoDB reserved words.
var emailAttributes = []string{"emailId", "threadId", "mailboxIds", "keywords", "receivedAt", "size", "blob", "subject", "from", "to", "cc", "sentAt"}

var emailProjection, emailNames = func() (string, map[string]string) {
	names := map[string]string{}
	refs := make([]string, len(emailAttributes))
	for i, a := range emailAttributes {
		refs[i] = "#" + a
		names["#"+a] = a
	}
	return strings.Join(refs, ", "), names
}()

// ListEmails reads every EMAIL record strongly, page by page.
func (a *Account) ListEmails(ctx context.Context) ([]jmap.EmailMeta, error) {
	var out []jmap.EmailMeta
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:                 &a.Table,
			ConsistentRead:            aws.Bool(true),
			KeyConditionExpression:    aws.String("PK = :pk AND begins_with(SK, :email)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s("ACCOUNT#" + a.AccountID), ":email": s("EMAIL#")},
			ProjectionExpression:      &emailProjection,
			ExpressionAttributeNames:  emailNames,
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			m, err := decodeEmail(it)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// GetEmails batch-reads EMAIL records strongly, retrying unprocessed keys.
func (a *Account) GetEmails(ctx context.Context, ids []string) (map[string]jmap.EmailMeta, error) {
	out := map[string]jmap.EmailMeta{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 100 {
		return nil, errors.New("at most 100 ids per batch")
	}
	keys := make([]map[string]types.AttributeValue, len(ids))
	for i, id := range ids {
		keys[i] = a.mail().key("EMAIL#" + id)
	}
	req := map[string]types.KeysAndAttributes{a.Table: {
		Keys:                     keys,
		ConsistentRead:           aws.Bool(true),
		ProjectionExpression:     &emailProjection,
		ExpressionAttributeNames: emailNames,
	}}
	for attempt := 0; len(req) > 0; attempt++ {
		if attempt > 0 {
			if attempt > 5 {
				return nil, errors.New("unprocessed keys after retries")
			}
			if err := sleep(ctx, time.Duration(attempt*attempt)*20*time.Millisecond); err != nil {
				return nil, err
			}
		}
		res, err := a.DB.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{RequestItems: req})
		if err != nil {
			return nil, err
		}
		for _, it := range res.Responses[a.Table] {
			m, err := decodeEmail(it)
			if err != nil {
				return nil, err
			}
			out[m.ID] = m
		}
		req = res.UnprocessedKeys
	}
	return out, nil
}

// Changes reads CHANGE records with after < seq <= upto.
func (a *Account) Changes(ctx context.Context, after, upto int64) ([]jmap.Change, error) {
	if after >= upto {
		return nil, nil
	}
	var out []jmap.Change
	var start map[string]types.AttributeValue
	for {
		page, err := a.DB.Query(ctx, &dynamodb.QueryInput{
			TableName:              &a.Table,
			ConsistentRead:         aws.Bool(true),
			KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":   s("ACCOUNT#" + a.AccountID),
				":from": s("CHANGE#" + padded(after+1)),
				":to":   s("CHANGE#" + padded(upto)),
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			sk := str(it["SK"])
			seq, err := strconv.ParseInt(sk[len("CHANGE#"):], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed change key %q", sk)
			}
			out = append(out, jmap.Change{
				Seq:       seq,
				Created:   idsByType(it["created"]),
				Updated:   idsByType(it["updated"]),
				Destroyed: idsByType(it["destroyed"]),
			})
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// UpdateEmail commits new keywords and mailbox membership with their
// listing entries, change record and index outbox work, guarded by the
// account sequence.
func (a *Account) UpdateEmail(ctx context.Context, prev jmap.State, before, after jmap.EmailMeta) error {
	m := a.mail()
	next := prev.Seq + 1
	items := []types.TransactWriteItem{
		{Update: &types.Update{
			TableName:                 &a.Table,
			Key:                       m.key("STATE"),
			UpdateExpression:          aws.String("SET seq = :next, emailState = :next"),
			ConditionExpression:       aws.String("seq = :prev"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":next": n(next), ":prev": n(prev.Seq)},
		}},
		{Update: &types.Update{
			TableName:           &a.Table,
			Key:                 m.key("EMAIL#" + after.ID),
			UpdateExpression:    aws.String("SET keywords = :k, mailboxIds = :m, updatedSeq = :next"),
			ConditionExpression: aws.String("attribute_exists(PK)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":k": boolMapAttr(after.Keywords), ":m": boolMapAttr(after.MailboxIDs), ":next": n(next),
			},
		}},
	}
	listKey := func(mailbox string) string {
		return fmt.Sprintf("LIST#%s#%s#%s", mailbox, sortTime(after.ReceivedAt), after.ID)
	}
	for id := range before.MailboxIDs {
		if !after.MailboxIDs[id] {
			items = append(items, types.TransactWriteItem{Delete: &types.Delete{TableName: &a.Table, Key: m.key(listKey(id))}})
		}
	}
	for id := range after.MailboxIDs {
		if !before.MailboxIDs[id] {
			item := m.key(listKey(id))
			item["emailId"] = s(after.ID)
			items = append(items, types.TransactWriteItem{Put: &types.Put{TableName: &a.Table, Item: item}})
		}
	}
	change := m.key("CHANGE#" + padded(next))
	change["updated"] = &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"Email": l(after.ID)}}
	change["searchDirty"] = l(after.ID)
	outbox := m.key("OUTBOX#" + padded(next) + "#index")
	outbox["emailIds"] = l(after.ID)
	items = append(items,
		types.TransactWriteItem{Put: &types.Put{TableName: &a.Table, Item: change, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
		types.TransactWriteItem{Put: &types.Put{TableName: &a.Table, Item: outbox, ConditionExpression: aws.String("attribute_not_exists(PK)")}},
	)
	_, err := a.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		switch {
		case reason(tce, 0) == "ConditionalCheckFailed" || anyReason(tce, "TransactionConflict"):
			return jmap.ErrStateChanged
		case reason(tce, 1) == "ConditionalCheckFailed":
			return fmt.Errorf("email %s no longer exists", after.ID)
		}
	}
	return err
}

func decodeEmail(it map[string]types.AttributeValue) (jmap.EmailMeta, error) {
	m := jmap.EmailMeta{
		ID:         str(it["emailId"]),
		ThreadID:   str(it["threadId"]),
		MailboxIDs: boolMapOf(it["mailboxIds"]),
		Keywords:   boolMapOf(it["keywords"]),
		Subject:    str(it["subject"]),
		From:       strList(it["from"]),
		To:         strList(it["to"]),
		Cc:         strList(it["cc"]),
	}
	if m.ID == "" {
		return m, errors.New("email record without emailId")
	}
	var err error
	if m.ReceivedAt, err = time.Parse("2006-01-02T15:04:05.000000000Z", str(it["receivedAt"])); err != nil {
		return m, fmt.Errorf("email %s receivedAt: %w", m.ID, err)
	}
	if v := str(it["sentAt"]); v != "" {
		m.SentAt, _ = time.Parse(time.RFC3339, v)
	}
	m.Size, _ = num(it["size"])
	if b, ok := it["blob"].(*types.AttributeValueMemberM); ok {
		m.Blob = jmap.BlobRef{Bucket: str(b.Value["bucket"]), Key: str(b.Value["key"]), VersionID: str(b.Value["versionId"])}
	}
	return m, nil
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
			if b, ok := e.(*types.AttributeValueMemberBOOL); ok && b.Value {
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
			out[k] = &types.AttributeValueMemberBOOL{Value: true}
		}
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

// MaxOriginalBytes bounds a full read: the SES S3 action stores at most
// 40 MB per message.
const MaxOriginalBytes = 41 << 20

// Fetch reads one exact original version in full.
func (o *Originals) Fetch(ctx context.Context, ref jmap.BlobRef) ([]byte, error) {
	if ref.VersionID == "" {
		return nil, fmt.Errorf("no version id for %s", ref.Key)
	}
	out, err := o.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &ref.Bucket, Key: &ref.Key, VersionId: &ref.VersionID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, MaxOriginalBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxOriginalBytes {
		return nil, fmt.Errorf("original %s exceeds %d bytes", ref.Key, MaxOriginalBytes)
	}
	return b, nil
}
