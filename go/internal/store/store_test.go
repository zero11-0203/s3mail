package store

import (
	"context"
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
	"github.com/zero11-0203/s3mail/go/internal/model"
)

// cancelDB fails every transaction with a scripted cancellation and
// answers reads as an empty account.
type cancelDB struct {
	reasons []string
	marker  bool
	txs     int
}

func (f *cancelDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if strings.HasPrefix(str(in.Key["SK"]), "RECEIPT#") && f.marker {
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{"SK": in.Key["SK"]}}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *cancelDB) Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return &dynamodb.QueryOutput{}, nil
}

func (f *cancelDB) BatchGetItem(context.Context, *dynamodb.BatchGetItemInput, ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error) {
	return &dynamodb.BatchGetItemOutput{}, nil
}

func (f *cancelDB) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.txs++
	reasons := make([]types.CancellationReason, len(in.TransactItems))
	for i := range reasons {
		reasons[i].Code = aws.String("None")
		if i < len(f.reasons) && f.reasons[i] != "" {
			reasons[i].Code = aws.String(f.reasons[i])
		}
	}
	return nil, &types.TransactionCanceledException{Message: aws.String("canceled"), CancellationReasons: reasons}
}

func TestReceiptCancellationClassification(t *testing.T) {
	ctx := context.Background()
	e := receipt(1, "x", "", "")
	newAcct := func(db DB) *Account {
		return &Account{DB: db, Table: "mail", AccountID: "primary", Backoff: func(int) time.Duration { return 0 }}
	}

	dup := &cancelDB{reasons: []string{"", "ConditionalCheckFailed"}, marker: true}
	if out, err := newAcct(dup).CommitReceipt(ctx, e); err != nil || out != ingest.Duplicate {
		t.Errorf("marker present: %v %v", out, err)
	}
	// A failed marker condition without a marker is not a duplicate.
	ghost := &cancelDB{reasons: []string{"", "ConditionalCheckFailed"}}
	if _, err := newAcct(ghost).CommitReceipt(ctx, e); err == nil {
		t.Error("a marker condition without a marker must be an error")
	}
	busy := &cancelDB{reasons: []string{"ConditionalCheckFailed"}}
	if _, err := newAcct(busy).CommitReceipt(ctx, e); err != ErrContention || busy.txs != maxCommitAttempts {
		t.Errorf("sequence conflicts retry: %v after %d", err, busy.txs)
	}
	invalid := &cancelDB{reasons: []string{"", "", "ValidationError"}}
	if _, err := newAcct(invalid).CommitReceipt(ctx, e); err == nil || invalid.txs != 1 {
		t.Errorf("validation failures are not retried in place: %v after %d", err, invalid.txs)
	}
}

func TestCommitPreflightsTheItemLimit(t *testing.T) {
	var m model.Mutation
	for i := range 40 {
		e := model.Email{ID: "M" + strings.Repeat("a", i+1), ThreadID: "Ta", MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: t0}
		m.Emails = append(m.Emails, model.EmailChange{After: &e})
	}
	db := &cancelDB{}
	if _, err := newAcctNoRetry(db).Commit(context.Background(), model.State{}, m); err != model.ErrTooLarge || db.txs != 0 {
		t.Errorf("oversized commit: %v after %d transactions", err, db.txs)
	}
}

func newAcctNoRetry(db DB) *Account { return &Account{DB: db, Table: "mail", AccountID: "primary"} }

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
