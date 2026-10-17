package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/zero11-0203/s3mail/go/internal/ingest"
	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Component tests run against DynamoDB Local (docs/operations/deployment.md#tiers)
// when S3MAIL_DYNAMODB_ENDPOINT is set, e.g. http://localhost:8765.
func localAccount(t *testing.T) *Account {
	t.Helper()
	endpoint := os.Getenv("S3MAIL_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3MAIL_DYNAMODB_ENDPOINT is not set")
	}
	db := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	table := fmt.Sprintf("mail-%d", time.Now().UnixNano())
	_, err := db.CreateTable(context.Background(), &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	})
	return &Account{DB: db, Table: table, AccountID: "primary", Backoff: func(int) time.Duration { return 0 }}
}

var t0 = time.Date(2026, 10, 17, 6, 40, 12, 345e6, time.UTC)

func receipt(n int, subject, msgID, inReplyTo string) ingest.Email {
	ses := fmt.Sprintf("s2examplemessageid%022d", n)
	id := ingest.EmailID("primary", ses)
	return ingest.Email{
		ID: id, ThreadID: "T" + id[1:], Mailbox: ingest.Inbox, SESMessageID: ses,
		Bucket: "s3mail-originals-test", Key: "inbound/" + ses, VersionID: "v1", Size: 1000 + int64(n),
		ReceivedAt: t0.Add(time.Duration(n) * time.Minute), EnvelopeSender: "probe@example.net",
		EnvelopeRecipients: []string{"alice@example.com"}, DeliveredTo: []string{"alice@example.com"}, ParserVersion: 1,
		Headers: ingest.Headers{Subject: subject, From: []string{"Probe <probe@example.net>"}, To: []string{"alice@example.com"},
			MessageID: msgID, InReplyTo: inReplyTo},
	}
}

func counts(t *testing.T, a *Account, mb string) model.Counts {
	t.Helper()
	c, err := a.MailboxCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c[mb]
}

func TestComponentReceiptsThreadsAndCounters(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	first := receipt(1, "Plan", "<p@example.net>", "")
	if out, err := a.CommitReceipt(ctx, first); err != nil || out != ingest.Committed {
		t.Fatalf("first: %v %v", out, err)
	}
	if out, err := a.CommitReceipt(ctx, first); err != nil || out != ingest.Duplicate {
		t.Fatalf("redelivery: %v %v", out, err)
	}
	reply := receipt(2, "Re: Plan", "<r@example.com>", "<p@example.net>")
	if out, err := a.CommitReceipt(ctx, reply); err != nil || out != ingest.Committed {
		t.Fatalf("reply: %v %v", out, err)
	}
	other := receipt(3, "Unrelated", "<u@example.org>", "")
	if _, err := a.CommitReceipt(ctx, other); err != nil {
		t.Fatal(err)
	}

	st, _ := a.State(ctx)
	if st != (model.State{Seq: 3, Epoch: 1, Email: 3, Mailbox: 3, Thread: 3}) {
		t.Errorf("state %+v", st)
	}
	emails, err := a.ListEmails(ctx)
	if err != nil || len(emails) != 3 {
		t.Fatalf("list %d %v", len(emails), err)
	}
	got, _ := a.GetEmails(ctx, []string{reply.ID, "Mmissing"})
	if len(got) != 1 || got[reply.ID].ThreadID != first.ThreadID || got[reply.ID].InReplyTo != "<p@example.net>" ||
		got[reply.ID].Envelope == nil || got[reply.ID].Blob.VersionID != "v1" || got[reply.ID].Source != model.SourceReceipt {
		t.Errorf("reply %+v", got[reply.ID])
	}
	if c := counts(t, a, "inbox"); c != (cnt(3, 3, 2, 2)) {
		t.Errorf("inbox counts %+v", c)
	}
	members, _ := a.ThreadMembers(ctx, []string{first.ThreadID, other.ThreadID, "Tnone"})
	if !slices.Equal(members[first.ThreadID], []string{first.ID, reply.ID}) || len(members[other.ThreadID]) != 1 || members["Tnone"] != nil {
		t.Errorf("members %v", members)
	}
	changes, _ := a.Changes(ctx, 1, 3)
	if len(changes) != 2 || changes[0].Updated["Thread"][0] != first.ThreadID || changes[1].Created["Thread"][0] != other.ThreadID {
		t.Errorf("changes %+v", changes)
	}
}

func TestComponentUpdateMoveAndDestroy(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	r := receipt(1, "Hello", "<h@example.net>", "")
	if _, err := a.CommitReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	st, _ := a.State(ctx)
	before := a.mustGet(t, r.ID)
	after := before.Clone()
	after.Keywords["$seen"] = true
	after.MailboxIDs = map[string]bool{"archive": true}
	st2, err := a.Commit(ctx, st, model.Mutation{Emails: []model.EmailChange{{Before: &before, After: &after}}})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Seq != 2 || st2.Thread != 1 || st2.Email != 2 || st2.Mailbox != 2 {
		t.Errorf("state after move %+v", st2)
	}
	if c := counts(t, a, "inbox"); !c.IsZero() {
		t.Errorf("inbox %+v", c)
	}
	if c := counts(t, a, "archive"); c != (cnt(1, 0, 1, 0)) {
		t.Errorf("archive %+v", c)
	}
	if _, err := a.Commit(ctx, st, model.Mutation{Emails: []model.EmailChange{{Before: &before, After: &after}}}); !errors.Is(err, model.ErrStateChanged) {
		t.Errorf("stale commit: %v", err)
	}

	moved := a.mustGet(t, r.ID)
	if !moved.Keywords["$seen"] || !moved.MailboxIDs["archive"] || moved.MailboxIDs["inbox"] {
		t.Errorf("stored %+v", moved)
	}
	st3, err := a.Commit(ctx, st2, model.Mutation{Emails: []model.EmailChange{{Before: &moved}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := a.GetEmails(ctx, []string{r.ID}); len(got) != 0 {
		t.Error("destroyed Email is still readable")
	}
	if c := counts(t, a, "archive"); !c.IsZero() {
		t.Errorf("archive after destroy %+v", c)
	}
	marker, _ := a.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: &a.Table, Key: a.key("DELETED#" + r.ID), ConsistentRead: aws.Bool(true)})
	if marker.Item == nil || str(marker.Item["versionId"]) != "v1" || str(marker.Item["sesMessageId"]) != r.SESMessageID {
		t.Errorf("deletion marker %v", marker.Item)
	}
	changes, _ := a.Changes(ctx, st2.Seq, st3.Seq)
	if len(changes) != 1 || changes[0].Destroyed["Email"][0] != r.ID || changes[0].Destroyed["Thread"][0] != r.ThreadID {
		t.Errorf("destroy change %+v", changes)
	}
	// Replaying the receipt after destruction cannot resurrect the Email.
	if out, err := a.CommitReceipt(ctx, r); err != nil || out != ingest.Duplicate {
		t.Errorf("replay after destroy: %v %v", out, err)
	}
	if refs, _ := a.ByMessageID(ctx, moved.MessageIDKeys()); len(refs) != 0 {
		t.Errorf("index records survive destruction: %v", refs)
	}
}

func (a *Account) mustGet(t *testing.T, id string) model.Email {
	t.Helper()
	got, err := a.GetEmails(context.Background(), []string{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("get %s: %v", id, err)
	}
	return got[id]
}

func TestComponentMailboxesAndUploads(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	st, _ := a.State(ctx)
	inbox := model.SystemMailboxes[0]
	renamed := inbox
	renamed.Name = "Posteingang"
	custom := model.Mailbox{ID: "Bcustom", Name: "Receipts", ParentID: "archive", SortOrder: 10, IsSubscribed: true}
	up := model.Upload{ID: "Uabc", Type: "text/plain", Size: 3, Bucket: "blobs", Key: "uploads/Uabc", Created: t0, ExpiresAt: t0.Add(24 * time.Hour)}
	st, err := a.Commit(ctx, st, model.Mutation{
		Mailboxes: []model.MailboxChange{{Before: &inbox, After: &renamed}, {After: &custom}},
		Uploads:   []model.Upload{up},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Email != 0 || st.Mailbox != 1 {
		t.Errorf("state %+v", st)
	}
	merged := model.Mailboxes(mustMailboxes(t, a))
	if merged[0].Name != "Posteingang" || merged[0].Role != "inbox" || merged[7] != custom {
		t.Errorf("mailboxes %+v", merged)
	}
	u, ok, err := a.GetUpload(ctx, "Uabc")
	if err != nil || !ok || u != up {
		t.Errorf("upload %+v %v %v", u, ok, err)
	}
	if _, err := a.Commit(ctx, st, model.Mutation{Mailboxes: []model.MailboxChange{{Before: &custom}}}); err != nil {
		t.Fatal(err)
	}
	if got := mustMailboxes(t, a); len(got) != 1 {
		t.Errorf("after destroy %+v", got)
	}
}

func mustMailboxes(t *testing.T, a *Account) []model.Mailbox {
	t.Helper()
	m, err := a.ListMailboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func cnt(total, unread, threads, unreadThreads int64) model.Counts {
	return model.Counts{TotalEmails: total, UnreadEmails: unread, TotalThreads: threads, UnreadThreads: unreadThreads}
}

func TestComponentRebuildDerived(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	first := receipt(1, "Plan", "<p@example.net>", "")
	reply := receipt(2, "Re: Plan", "<r@example.com>", "<p@example.net>")
	for _, r := range []ingest.Email{first, reply} {
		if _, err := a.CommitReceipt(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the first schema: drop every derived record but LIST and
	// leave a stale counter behind.
	for _, p := range []string{"MEMBER#", "MAILBOXTHREAD#", "COUNT#", "MESSAGEID#", "SUBJECT#"} {
		_ = a.queryPrefix(ctx, p, aws.String("SK"), nil, func(it map[string]types.AttributeValue) error {
			_, err := a.DB.(*dynamodb.Client).DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &a.Table, Key: a.key(str(it["SK"]))})
			return err
		})
	}
	_, _ = a.DB.(*dynamodb.Client).PutItem(ctx, &dynamodb.PutItemInput{TableName: &a.Table, Item: mergeKey(a.key("COUNT#junk"), map[string]types.AttributeValue{"totalEmails": n(9)})})
	before, _ := a.State(ctx)
	st, err := a.RebuildDerived(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Seq != before.Seq+1 || st.Mailbox != st.Seq || st.Email != before.Email {
		t.Errorf("state %+v after %+v", st, before)
	}
	all, _ := a.MailboxCounts(ctx)
	if all["inbox"] != cnt(2, 2, 1, 1) || len(all) != 1 {
		t.Errorf("counts %+v", all)
	}
	members, _ := a.ThreadMembers(ctx, []string{first.ThreadID})
	if len(members[first.ThreadID]) != 2 {
		t.Errorf("members %v", members)
	}
	if refs, _ := a.ByMessageID(ctx, []string{"x"}); len(refs) != 0 {
		t.Error("unexpected refs")
	}
	changes, _ := a.Changes(ctx, before.Seq, st.Seq)
	if len(changes) != 1 || !slices.Contains(changes[0].Updated["MailboxCounts"], "junk") {
		t.Errorf("change %+v", changes)
	}
	// A further receipt threads against the rebuilt index.
	again := receipt(3, "Re: Plan", "<s@example.com>", "<r@example.com>")
	if _, err := a.CommitReceipt(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got := a.mustGet(t, again.ID); got.ThreadID != first.ThreadID {
		t.Errorf("thread %s", got.ThreadID)
	}
}

func TestComponentSearchRegistration(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	if _, err := a.CommitReceipt(ctx, receipt(1, "Plan", "<p@example.net>", "")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.SearchIndex(ctx); ok || err != nil {
		t.Fatalf("no registration yet: %v %v", ok, err)
	}
	pub := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	reg := func(prev, next string, seq int64) error {
		st, err := a.State(ctx)
		if err != nil {
			return err
		}
		_, err = a.Commit(ctx, st, model.Mutation{Search: &model.SearchRegistration{Prev: prev, Next: model.SearchIndex{
			Generation: next, Seq: seq, Epoch: st.Epoch, Key: "index/x.sqlite", VersionID: "v-" + next, ETag: `"e"`, PublishedAt: pub}}})
		return err
	}
	before, _ := a.State(ctx)
	if err := reg("", "g1", before.Seq); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := a.SearchIndex(ctx)
	if !ok || got.Generation != "g1" || got.Seq != before.Seq || got.VersionID != "v-g1" || !got.PublishedAt.Equal(pub) {
		t.Errorf("registration %+v", got)
	}
	if st, _ := a.State(ctx); st.Email != st.Seq || st.Seq != before.Seq+1 {
		t.Errorf("registration advances Email state: %+v", st)
	}
	// A stale writer cannot replace a newer generation, nor create one.
	for _, prev := range []string{"", "g0"} {
		if err := reg(prev, "g2", before.Seq); !errors.Is(err, model.ErrSearchConflict) {
			t.Errorf("prev %q: %v", prev, err)
		}
	}
	if err := reg("g1", "g2", before.Seq+1); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := a.SearchIndex(ctx); got.Generation != "g2" {
		t.Errorf("after CAS %+v", got)
	}
}

func TestComponentSubmissionRecords(t *testing.T) {
	a := localAccount(t)
	ctx := context.Background()
	commit := func(m model.Mutation) (model.State, error) {
		st, err := a.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a.Commit(ctx, st, m)
	}
	id := model.Identity{ID: model.IdentityID("me@example.com"), Email: "me@example.com", Name: "Me"}
	if _, err := commit(model.Mutation{Identities: []model.IdentityChange{{After: &id}}}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.ListIdentities(ctx)
	if len(got) != 1 || got[0].Email != id.Email || got[0].Name != "Me" || got[0].ID != id.ID {
		t.Errorf("identities %+v", got)
	}

	sub := model.Submission{ID: "Sa", IdentityID: id.ID, EmailID: "Ma", ThreadID: "Ta", MailFrom: id.Email, RcptTo: []string{"x@example.net", "y@example.net"},
		SendAt: time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC), Status: model.SubmissionQueued, Reservation: model.Reservation{Units: 2, Keys: []string{"smtp2go#day#2026-10-17"}}}
	sub.SetRecipients("queued", "250 2.0.0 queued")
	quota := []model.QuotaDelta{{Key: "smtp2go#day#2026-10-17", Units: 2, Limit: 3}}
	st, err := commit(model.Mutation{Submissions: []model.SubmissionChange{{After: &sub}}, Quota: quota, Send: []string{"Sa"},
		Attempts: []model.Attempt{{SubmissionID: "Sa", Attempt: 0, Phase: "created", At: sub.SendAt}}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Submission != st.Seq || st.Identity == st.Seq {
		t.Errorf("state %+v", st)
	}
	back, ok, _ := a.GetSubmission(ctx, "Sa")
	if !ok || back.Status != model.SubmissionQueued || back.Recipients["y@example.net"].Delivered != "queued" || !back.SendAt.Equal(sub.SendAt) {
		t.Errorf("submission %+v", back)
	}
	// Another reservation of two would exceed three.
	sub2 := sub
	sub2.ID = "Sb"
	if _, err := commit(model.Mutation{Submissions: []model.SubmissionChange{{After: &sub2}}, Quota: quota}); !errors.Is(err, model.ErrOverQuota) {
		t.Errorf("over quota: %v", err)
	}
	if _, found, _ := a.GetSubmission(ctx, "Sb"); found {
		t.Error("the rejected commit wrote nothing")
	}
	// Releasing restores capacity.
	if _, err := commit(model.Mutation{Quota: []model.QuotaDelta{{Key: quota[0].Key, Units: -2}}}); err != nil {
		t.Fatal(err)
	}
	if used, _ := a.QuotaUsed(ctx, quota[0].Key); used != 0 {
		t.Errorf("used %d", used)
	}
	ob, _ := a.Outbox(ctx)
	var submit []OutboxEntry
	for _, e := range ob {
		if e.Destination == "submit" {
			submit = append(submit, e)
		}
	}
	if len(submit) != 1 || submit[0].IDs[0] != "Sa" {
		t.Fatalf("outbox %+v", ob)
	}
	if err := a.AckOutbox(ctx, submit[0].Key); err != nil {
		t.Fatal(err)
	}
	if ob, _ = a.Outbox(ctx); slices.ContainsFunc(ob, func(e OutboxEntry) bool { return e.Destination == "submit" }) {
		t.Error("acknowledged entry remains")
	}
	ats, _ := a.Attempts(ctx, "Sa")
	if len(ats) != 1 || ats[0].Phase != "created" {
		t.Errorf("attempts %+v", ats)
	}
	changes, _ := a.Changes(ctx, 0, st.Seq)
	if last := changes[len(changes)-1]; !slices.Contains(last.Created["EmailSubmission"], "Sa") {
		t.Errorf("change %+v", last)
	}
}
