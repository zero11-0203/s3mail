package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/zero11-0203/s3mail/go/internal/ingest"
)

// fakeDB answers STATE and RECEIPT reads from fields and fails each
// transaction with the next scripted error.
type fakeDB struct {
	seq        int64
	marker     bool
	txErrs     []error
	txs        []*dynamodb.TransactWriteItemsInput
	stateReads int
}

func (f *fakeDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if !aws.ToBool(in.ConsistentRead) {
		return nil, errors.New("reads must be strongly consistent")
	}
	sk := in.Key["SK"].(*types.AttributeValueMemberS).Value
	switch {
	case sk == "STATE":
		f.stateReads++
		if f.seq == 0 {
			return &dynamodb.GetItemOutput{}, nil
		}
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{"seq": n(f.seq)}}, nil
	case strings.HasPrefix(sk, "RECEIPT#") && f.marker:
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{"SK": s(sk)}}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDB) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.txs = append(f.txs, in)
	if len(f.txErrs) > 0 {
		err := f.txErrs[0]
		f.txErrs = f.txErrs[1:]
		return nil, err
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

// canceled builds a cancellation with code at item position i.
func canceled(i int, code string) error {
	reasons := make([]types.CancellationReason, itemOutboxEvent+1)
	for j := range reasons {
		reasons[j].Code = aws.String("None")
	}
	reasons[i].Code = aws.String(code)
	return &types.TransactionCanceledException{Message: aws.String("canceled"), CancellationReasons: reasons}
}

var testEmail = ingest.Email{
	ID: "Mabc", ThreadID: "Tabc", Mailbox: ingest.Inbox,
	SESMessageID: "s2examplemessageid0000000000000000000001",
	Bucket:       "s3mail-originals-test", Key: "inbound/s2examplemessageid0000000000000000000001", VersionID: "v1",
	Size: 1929, ReceivedAt: time.Date(2026, 10, 17, 6, 40, 12, 345e6, time.UTC),
	EnvelopeSender: "probe@example.net", EnvelopeRecipients: []string{"alice@example.com"},
	DeliveredTo: []string{"alice@example.com"}, ParserVersion: 1,
	Headers: ingest.Headers{Subject: "hello"},
}

func newMail(db DynamoDB) *Mail {
	return &Mail{DB: db, Table: "mail", AccountID: "primary", Backoff: func(int) time.Duration { return 0 }}
}

func sk(item types.TransactWriteItem) string {
	if item.Put != nil {
		return item.Put.Item["SK"].(*types.AttributeValueMemberS).Value
	}
	return item.Update.Key["SK"].(*types.AttributeValueMemberS).Value
}

func TestCommitReceiptTransaction(t *testing.T) {
	db := &fakeDB{}
	out, err := newMail(db).CommitReceipt(context.Background(), testEmail)
	if err != nil || out != ingest.Committed {
		t.Fatalf("out %v err %v", out, err)
	}
	items := db.txs[0].TransactItems
	want := []string{
		"STATE",
		"RECEIPT#s2examplemessageid0000000000000000000001",
		"EMAIL#Mabc",
		"LIST#inbox#2026-10-17T06:40:12.345000000Z#Mabc",
		"CHANGE#00000000000000000001",
		"OUTBOX#00000000000000000001#index",
		"OUTBOX#00000000000000000001#events",
	}
	for i, w := range want {
		if got := sk(items[i]); got != w {
			t.Errorf("item %d SK = %q, want %q", i, got, w)
		}
		pk := items[i]
		var key map[string]types.AttributeValue
		if pk.Put != nil {
			key = pk.Put.Item
			if aws.ToString(pk.Put.ConditionExpression) != "attribute_not_exists(PK)" {
				t.Errorf("%s is not conditioned on absence", w)
			}
		} else {
			key = pk.Update.Key
		}
		if key["PK"].(*types.AttributeValueMemberS).Value != "ACCOUNT#primary" {
			t.Errorf("%s is outside the account partition", w)
		}
	}
	if c := aws.ToString(items[itemState].Update.ConditionExpression); c != "attribute_not_exists(seq)" {
		t.Errorf("first commit STATE condition = %q", c)
	}
	receipt := items[itemReceipt].Put.Item
	for _, forbidden := range []string{"envelopeSender", "envelopeRecipients", "deliveredTo", "subject", "from", "to"} {
		if _, ok := receipt[forbidden]; ok {
			t.Errorf("receipt marker holds %s", forbidden)
		}
	}
	if receipt["versionId"].(*types.AttributeValueMemberS).Value != "v1" {
		t.Error("receipt marker lacks the exact original version")
	}
	email := items[itemEmail].Put.Item
	if _, ok := email["envelopeRecipients"]; !ok {
		t.Error("email lacks the envelope recipients")
	}
}

func TestCommitReceiptGuardsTheSequence(t *testing.T) {
	db := &fakeDB{seq: 41}
	if _, err := newMail(db).CommitReceipt(context.Background(), testEmail); err != nil {
		t.Fatal(err)
	}
	u := db.txs[0].TransactItems[itemState].Update
	if aws.ToString(u.ConditionExpression) != "seq = :prev" || u.ExpressionAttributeValues[":prev"].(*types.AttributeValueMemberN).Value != "41" ||
		u.ExpressionAttributeValues[":next"].(*types.AttributeValueMemberN).Value != "42" {
		t.Errorf("STATE update = %+v", u)
	}
}

func TestCommitReceiptRetriesSequenceConflicts(t *testing.T) {
	db := &fakeDB{seq: 3, txErrs: []error{canceled(itemState, "ConditionalCheckFailed"), canceled(itemList, "TransactionConflict")}}
	out, err := newMail(db).CommitReceipt(context.Background(), testEmail)
	if err != nil || out != ingest.Committed || len(db.txs) != 3 || db.stateReads != 3 {
		t.Errorf("out %v err %v txs %d state reads %d (each retry starts from a fresh read)", out, err, len(db.txs), db.stateReads)
	}
}

func TestCommitReceiptContention(t *testing.T) {
	var errs []error
	for range maxCommitAttempts {
		errs = append(errs, canceled(itemState, "ConditionalCheckFailed"))
	}
	_, err := newMail(&fakeDB{txErrs: errs}).CommitReceipt(context.Background(), testEmail)
	if !errors.Is(err, ErrContention) {
		t.Errorf("err = %v, want ErrContention", err)
	}
}

func TestCommitReceiptClassifiesTheMarker(t *testing.T) {
	dup := &fakeDB{marker: true, txErrs: []error{canceled(itemReceipt, "ConditionalCheckFailed")}}
	if out, err := newMail(dup).CommitReceipt(context.Background(), testEmail); err != nil || out != ingest.Duplicate {
		t.Errorf("confirmed marker: out %v err %v, want duplicate", out, err)
	}

	// A failed receipt condition without a readable marker is not a commit.
	ghost := &fakeDB{txErrs: []error{canceled(itemReceipt, "ConditionalCheckFailed")}}
	if out, err := newMail(ghost).CommitReceipt(context.Background(), testEmail); err == nil {
		t.Errorf("unconfirmed marker: out %v, want an error", out)
	}

	orphan := &fakeDB{txErrs: []error{canceled(itemEmail, "ConditionalCheckFailed")}}
	if _, err := newMail(orphan).CommitReceipt(context.Background(), testEmail); err == nil {
		t.Error("an existing Email without a marker must not be overwritten or counted as a duplicate")
	}

	validation := &fakeDB{txErrs: []error{canceled(itemEmail, "ValidationError")}}
	if _, err := newMail(validation).CommitReceipt(context.Background(), testEmail); err == nil || len(validation.txs) != 1 {
		t.Errorf("validation failures are not retried in place: err %v txs %d", err, len(validation.txs))
	}
}

type fakeS3 struct {
	versions []s3types.ObjectVersion
	body     string
	got      *s3.GetObjectInput
}

func (f *fakeS3) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	return &s3.ListObjectVersionsOutput{Versions: f.versions}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.got = in
	return &s3.GetObjectOutput{
		Body:         io.NopCloser(strings.NewReader(f.body[:4])),
		ContentRange: aws.String("bytes 0-3/1929"),
	}, nil
}

func version(key, id string, at time.Time) s3types.ObjectVersion {
	return s3types.ObjectVersion{Key: aws.String(key), VersionId: aws.String(id), LastModified: aws.Time(at)}
}

func TestResolveVersion(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	o := &Originals{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), S3: &fakeS3{versions: []s3types.ObjectVersion{
		version("inbound/abc", "newer", t0.Add(time.Hour)),
		version("inbound/abcd", "other-key", t0.Add(-time.Hour)),
		version("inbound/abc", "first", t0),
	}}}
	v, err := o.ResolveVersion(context.Background(), "b", "inbound/abc")
	if err != nil || v != "first" {
		t.Errorf("version %q err %v, want the oldest version of the exact key", v, err)
	}
	if _, err := o.ResolveVersion(context.Background(), "b", "inbound/missing"); err == nil {
		t.Error("a missing original must be an error")
	}
}

func TestReadPrefix(t *testing.T) {
	f := &fakeS3{body: "Subject: x"}
	o := &Originals{S3: f}
	b, size, err := o.ReadPrefix(context.Background(), "b", "k", "v1", 4)
	if err != nil || string(b) != "Subj" || size != 1929 {
		t.Errorf("prefix %q size %d err %v", b, size, err)
	}
	if aws.ToString(f.got.VersionId) != "v1" || aws.ToString(f.got.Range) != "bytes=0-3" {
		t.Errorf("request = version %q range %q", aws.ToString(f.got.VersionId), aws.ToString(f.got.Range))
	}
}
