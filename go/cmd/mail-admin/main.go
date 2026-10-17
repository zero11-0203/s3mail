// Command mail-admin runs owner maintenance against the mail table with the
// caller's AWS credentials (docs/operations/recovery.md).
//
//	mail-admin -table NAME -account ID rebuild-derived
//	mail-admin -table NAME submissions
//	mail-admin -table NAME submission ID
//	mail-admin -table NAME resolve ID accepted|rejected|retry
//
// rebuild-derived recomputes listing, thread-membership, threading-index,
// summary and counter records from the stored Emails in one guarded
// transaction. It never touches Emails, originals or receipt markers.
//
// submissions lists every submission's send state; submission shows one
// with its attempt history. resolve is the owner's reconciliation of an
// acceptance-unknown submission after checking the relay: retry
// explicitly authorizes a new attempt
// (docs/specs/mail-flow.md#outbound-email).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/send"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	table := flag.String("table", "", "mail table name")
	account := flag.String("account", "primary", "internal account id")
	flag.Parse()
	if *table == "" || flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: mail-admin -table NAME [-account ID] rebuild-derived | submissions | submission ID | resolve ID accepted|rejected|retry")
		os.Exit(2)
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fail(err)
	}
	a := &store.Account{DB: dynamodb.NewFromConfig(cfg), Table: *table, AccountID: *account}
	switch flag.Arg(0) {
	case "rebuild-derived":
		st, err := a.RebuildDerived(ctx)
		if err != nil {
			fail(err)
		}
		fmt.Printf("rebuilt derived records at seq %d\n", st.Seq)
	case "submissions":
		subs, err := a.ListSubmissions(ctx)
		if err != nil {
			fail(err)
		}
		slices.SortFunc(subs, func(x, y model.Submission) int { return x.SendAt.Compare(y.SendAt) })
		for _, x := range subs {
			hidden := ""
			if x.Destroyed {
				hidden = " (destroyed)"
			}
			fmt.Printf("%s  %s  %-9s attempt %d  %d recipients  email %s%s\n", x.ID, x.SendAt.UTC().Format(time.RFC3339), x.Status, x.Attempt, len(x.RcptTo), x.EmailID, hidden)
		}
	case "submission":
		x := submission(ctx, a)
		fmt.Printf("id %s\nstatus %s (undoStatus %s)\nemail %s\nfrom %s\nrecipients %s\nrelay %s\nblob %s@%s\nattempt %d handoff %v lease %s\n",
			x.ID, x.Status, x.UndoStatus(), x.EmailID, x.MailFrom, strings.Join(x.RcptTo, ", "), x.Relay, x.Blob.Key, x.Blob.VersionID,
			x.Attempt, x.Handoff, x.LeaseUntil.UTC().Format(time.RFC3339))
		for r, st := range x.Recipients {
			fmt.Printf("  %s: %s %s\n", r, st.Delivered, st.SMTPReply)
		}
		history, err := a.Attempts(ctx, x.ID)
		if err != nil {
			fail(err)
		}
		for _, at := range history {
			fmt.Printf("  #%d.%d %s %s owner=%s %s\n", at.Attempt, at.Seq, at.At.UTC().Format(time.RFC3339), at.Phase, at.Owner, at.Detail)
		}
	case "resolve":
		if flag.NArg() != 3 {
			fail(fmt.Errorf("usage: resolve ID accepted|rejected|retry"))
		}
		for range 5 {
			st, err := a.State(ctx)
			if err != nil {
				fail(err)
			}
			m, err := send.Resolve(submission(ctx, a), flag.Arg(2), time.Now())
			if err != nil {
				fail(err)
			}
			if _, err = a.Commit(ctx, st, *m); err == nil {
				fmt.Println("resolved", flag.Arg(1), "as", flag.Arg(2))
				return
			} else if !errors.Is(err, model.ErrStateChanged) {
				fail(err)
			}
		}
		fail(fmt.Errorf("account busy; retry"))
	default:
		fmt.Fprintln(os.Stderr, "unknown command", flag.Arg(0))
		os.Exit(2)
	}
}

func submission(ctx context.Context, a *store.Account) model.Submission {
	if flag.NArg() < 2 {
		fail(fmt.Errorf("a submission id is required"))
	}
	x, ok, err := a.GetSubmission(ctx, flag.Arg(1))
	if err != nil {
		fail(err)
	}
	if !ok {
		fail(fmt.Errorf("no submission %s", flag.Arg(1)))
	}
	return x
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mail-admin:", err)
	os.Exit(1)
}
