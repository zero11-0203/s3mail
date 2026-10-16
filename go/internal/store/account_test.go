package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/jmap"
)

// fakeAccountDB pages Query results and returns unprocessed keys once.
type fakeAccountDB struct {
	fakeDB
	pages      [][]map[string]types.AttributeValue
	queries    []*dynamodb.QueryInput
	items      map[string]map[string]types.AttributeValue
	unprocOnce bool
	batches    int
}

func (f *fakeAccountDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if !aws.ToBool(in.ConsistentRead) {
		return nil, errors.New("queries must be strongly consistent")
	}
	f.queries = append(f.queries, in)
	i := len(f.queries) - 1
	out := &dynamodb.QueryOutput{Items: f.pages[i]}
	if i+1 < len(f.pages) {
		out.LastEvaluatedKey = map[string]types.AttributeValue{"SK": s("next")}
	}
	return out, nil
}

func (f *fakeAccountDB) BatchGetItem(_ context.Context, in *dynamodb.BatchGetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.BatchGetItemOutput, error) {
	f.batches++
	ka := in.RequestItems["mail"]
	if !aws.ToBool(ka.ConsistentRead) {
		return nil, errors.New("batch reads must be strongly consistent")
	}
	out := &dynamodb.BatchGetItemOutput{Responses: map[string][]map[string]types.AttributeValue{}}
	keys := ka.Keys
	if f.unprocOnce && len(keys) > 1 {
		f.unprocOnce = false
		ka.Keys = keys[1:]
		out.UnprocessedKeys = map[string]types.KeysAndAttributes{"mail": ka}
		keys = keys[:1]
	}
	for _, k := range keys {
		if it, ok := f.items[str(k["SK"])]; ok {
			out.Responses["mail"] = append(out.Responses["mail"], it)
		}
	}
	return out, nil
}

func newAccount(db AccountDB) *Account { return &Account{DB: db, Table: "mail", AccountID: "primary"} }

func TestEmailRecordRoundTrip(t *testing.T) {
	e := testEmail
	e.Headers.From = []string{"probe@example.net"}
	m, err := decodeEmail(emailItem(e, 3))
	if err != nil {
		t.Fatal(err)
	}
	want := jmap.EmailMeta{
		ID: e.ID, ThreadID: e.ThreadID, MailboxIDs: map[string]bool{"inbox": true}, Keywords: map[string]bool{},
		ReceivedAt: e.ReceivedAt, Size: e.Size, Subject: "hello", From: []string{"probe@example.net"}, To: []string{}, Cc: []string{},
		Blob: jmap.BlobRef{Bucket: e.Bucket, Key: e.Key, VersionID: "v1"},
	}
	if m.ID != want.ID || !m.ReceivedAt.Equal(want.ReceivedAt) || m.Blob != want.Blob || !m.MailboxIDs["inbox"] ||
		m.Size != want.Size || m.Subject != want.Subject || !slices.Equal(m.From, want.From) {
		t.Errorf("decoded %+v\nwant %+v", m, want)
	}
	if _, err := decodeEmail(map[string]types.AttributeValue{}); err == nil {
		t.Error("a record without emailId must fail")
	}
}

func TestStateDefaults(t *testing.T) {
	st, err := newAccount(&fakeAccountDB{}).State(context.Background())
	if err != nil || st != (jmap.State{Epoch: 1}) {
		t.Errorf("fresh account %+v %v", st, err)
	}
}

func TestListEmailsPages(t *testing.T) {
	a, b := testEmail, testEmail
	b.ID = "Mdef"
	db := &fakeAccountDB{pages: [][]map[string]types.AttributeValue{{emailItem(a, 1)}, {emailItem(b, 2)}}}
	got, err := newAccount(db).ListEmails(context.Background())
	if err != nil || len(got) != 2 || got[1].ID != "Mdef" {
		t.Fatalf("list %v %v", got, err)
	}
	for _, ref := range strings.Split(aws.ToString(db.queries[0].ProjectionExpression), ", ") {
		if !strings.HasPrefix(ref, "#") {
			t.Errorf("projection attribute %q is not aliased", ref)
		}
	}
	if db.queries[1].ExclusiveStartKey == nil {
		t.Error("the second page must continue from LastEvaluatedKey")
	}
}

func TestGetEmailsRetriesUnprocessed(t *testing.T) {
	a, b := testEmail, testEmail
	b.ID = "Mdef"
	db := &fakeAccountDB{unprocOnce: true, items: map[string]map[string]types.AttributeValue{
		"EMAIL#Mabc": emailItem(a, 1), "EMAIL#Mdef": emailItem(b, 2),
	}}
	got, err := newAccount(db).GetEmails(context.Background(), []string{"Mabc", "Mdef", "Mmissing"})
	if err != nil || len(got) != 2 || db.batches != 2 {
		t.Errorf("got %d emails in %d batches: %v", len(got), db.batches, err)
	}
}

func TestChangesRange(t *testing.T) {
	db := &fakeAccountDB{pages: [][]map[string]types.AttributeValue{{{
		"SK":      s("CHANGE#" + padded(4)),
		"created": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"Email": l("Mabc")}},
	}}}}
	got, err := newAccount(db).Changes(context.Background(), 3, 5)
	if err != nil || len(got) != 1 || got[0].Seq != 4 || got[0].Created["Email"][0] != "Mabc" {
		t.Fatalf("changes %+v %v", got, err)
	}
	v := db.queries[0].ExpressionAttributeValues
	if str(v[":from"]) != "CHANGE#"+padded(4) || str(v[":to"]) != "CHANGE#"+padded(5) {
		t.Errorf("range %v..%v", str(v[":from"]), str(v[":to"]))
	}
	if got, _ := newAccount(db).Changes(context.Background(), 5, 5); got != nil || len(db.queries) != 1 {
		t.Error("an empty range must not query")
	}
}

func TestUpdateEmailTransaction(t *testing.T) {
	db := &fakeAccountDB{}
	before := jmap.EmailMeta{ID: "Mabc", MailboxIDs: map[string]bool{"inbox": true}, ReceivedAt: testEmail.ReceivedAt}
	after := before
	after.MailboxIDs = map[string]bool{"archive": true}
	after.Keywords = map[string]bool{"$seen": true}
	if err := newAccount(db).UpdateEmail(context.Background(), jmap.State{Seq: 7, Epoch: 1}, before, after); err != nil {
		t.Fatal(err)
	}
	items := db.txs[0].TransactItems
	var got []string
	for _, it := range items {
		switch {
		case it.Delete != nil:
			got = append(got, "delete "+str(it.Delete.Key["SK"]))
		default:
			got = append(got, sk(it))
		}
	}
	listTime := sortTime(testEmail.ReceivedAt)
	want := []string{"STATE", "EMAIL#Mabc", "delete LIST#inbox#" + listTime + "#Mabc", "LIST#archive#" + listTime + "#Mabc",
		"CHANGE#" + padded(8), "OUTBOX#" + padded(8) + "#index"}
	if !slices.Equal(got, want) {
		t.Errorf("items %v\nwant %v", got, want)
	}
	if v := items[0].Update.ExpressionAttributeValues; v[":prev"].(*types.AttributeValueMemberN).Value != "7" {
		t.Error("the transaction must be guarded by the previous sequence")
	}
	kw := items[1].Update.ExpressionAttributeValues[":k"].(*types.AttributeValueMemberM).Value
	if _, ok := kw["$seen"]; !ok {
		t.Errorf("keywords %v", kw)
	}

	db.txErrs = []error{canceled(0, "ConditionalCheckFailed"), canceled(3, "TransactionConflict"), canceled(1, "ConditionalCheckFailed")}
	for i, want := range []bool{true, true, false} {
		err := newAccount(db).UpdateEmail(context.Background(), jmap.State{Seq: 7, Epoch: 1}, before, after)
		if errors.Is(err, jmap.ErrStateChanged) != want || err == nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
}
