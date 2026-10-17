// Command mail-admin runs owner maintenance against the mail table with the
// caller's AWS credentials (docs/operations/recovery.md).
//
//	mail-admin -table NAME -account ID rebuild-derived
//
// rebuild-derived recomputes listing, thread-membership, threading-index,
// summary and counter records from the stored Emails in one guarded
// transaction. It never touches Emails, originals or receipt markers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/zero11-0203/s3mail/go/internal/store"
)

func main() {
	table := flag.String("table", "", "mail table name")
	account := flag.String("account", "primary", "internal account id")
	flag.Parse()
	if *table == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mail-admin -table NAME [-account ID] rebuild-derived")
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
	default:
		fmt.Fprintln(os.Stderr, "unknown command", flag.Arg(0))
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mail-admin:", err)
	os.Exit(1)
}
