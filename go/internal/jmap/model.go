// Package jmap serves JMAP Core and Mail (RFC 8620, RFC 8621) over the
// mail table (docs/specs/jmap.md). Every advertised method follows the
// conformance matrix in docs/specs/jmap-conformance.md.
package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// State is the account commit boundary: STATE.seq and the restore epoch.
type State struct {
	Seq   int64
	Epoch int64
}

// BlobRef is the exact original object behind an Email.
type BlobRef struct {
	Bucket    string
	Key       string
	VersionID string
}

// EmailMeta is the stored metadata of one Email.
type EmailMeta struct {
	ID         string
	ThreadID   string
	MailboxIDs map[string]bool
	Keywords   map[string]bool
	ReceivedAt time.Time
	Size       int64
	Blob       BlobRef
	// Header projection for filters and sorting; the Email/get header
	// properties are parsed from the raw message instead.
	Subject string
	From    []string
	To      []string
	Cc      []string
	SentAt  time.Time
}

// Change is one committed CHANGE record: object ids by type.
type Change struct {
	Seq       int64
	Created   map[string][]string
	Updated   map[string][]string
	Destroyed map[string][]string
}

// ErrStateChanged means another commit advanced the account sequence.
var ErrStateChanged = errors.New("account state changed")

// Store reads and mutates one account's records.
type Store interface {
	State(ctx context.Context) (State, error)
	ListEmails(ctx context.Context) ([]EmailMeta, error)
	GetEmails(ctx context.Context, ids []string) (map[string]EmailMeta, error)
	// Changes returns the CHANGE records with after < seq <= upto.
	Changes(ctx context.Context, after, upto int64) ([]Change, error)
	// UpdateEmail commits after in place of before, guarded by prev.
	UpdateEmail(ctx context.Context, prev State, before, after EmailMeta) error
}

// Originals reads raw MIME by exact version.
type Originals interface {
	Fetch(ctx context.Context, ref BlobRef) ([]byte, error)
}

// Account is the caller's mapped JMAP account.
type Account struct {
	InternalID string
	Username   string
	Scopes     []string
	Expires    time.Time
}

// Scopes, exactly as Cognito issues them (docs/specs/security.md#scopes).
const (
	ScopeRead   = "s3mail/mail.read"
	ScopeWrite  = "s3mail/mail.write"
	ScopeSubmit = "s3mail/mail.submit"
)

func (a Account) has(scope string) bool {
	for _, s := range a.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// PublicAccountID is the JMAP accountId: it changes with the restore epoch
// so a rebuilt account forces full client resynchronization.
func PublicAccountID(internalID string, epoch int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("account\x00%s\x00%d", internalID, epoch)))
	return "A" + strings.ToLower(idEncoding.EncodeToString(sum[:10]))
}

// stateToken encodes a type state; all types currently share the account
// sequence, which advances conservatively.
func stateToken(s State) string { return fmt.Sprintf("%d-%d", s.Epoch, s.Seq) }

// parseState decodes a token issued under the current epoch.
func parseState(tok string, cur State) (int64, bool) {
	e, q, ok := strings.Cut(tok, "-")
	if !ok {
		return 0, false
	}
	epoch, err1 := strconv.ParseInt(e, 10, 64)
	seq, err2 := strconv.ParseInt(q, 10, 64)
	if err1 != nil || err2 != nil || epoch != cur.Epoch || seq < 0 || seq > cur.Seq {
		return 0, false
	}
	return seq, true
}

// Blob ids: "G<core>" is the raw message of Email "M<core>";
// "P<core>-<partId>" is one leaf part of it.
func rawBlobID(emailID string) string { return "G" + emailID[1:] }

func partBlobID(emailID, partID string) string { return "P" + emailID[1:] + "-" + partID }

// ParseBlobID returns the Email id and part id ("" for the raw message).
func ParseBlobID(blobID string) (emailID, partID string, ok bool) {
	switch {
	case len(blobID) > 1 && blobID[0] == 'G' && validCore(blobID[1:]):
		return "M" + blobID[1:], "", true
	case len(blobID) > 1 && blobID[0] == 'P':
		core, part, found := strings.Cut(blobID[1:], "-")
		if found && validCore(core) && part != "" && strings.Trim(part, "0123456789") == "" {
			return "M" + core, part, true
		}
	}
	return "", "", false
}

func validCore(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func threadIDOf(emailID string) string { return "T" + emailID[1:] }

func emailIDOfThread(threadID string) (string, bool) {
	if len(threadID) < 2 || threadID[0] != 'T' || !validCore(threadID[1:]) {
		return "", false
	}
	return "M" + threadID[1:], true
}

// Mailboxes are fixed until Mailbox/set is implemented; Quarantine has no
// JMAP role (docs/specs/storage-sync.md#account-model).
type mailboxDef struct {
	ID, Name, Role string
	SortOrder      int
}

var mailboxes = []mailboxDef{
	{"inbox", "Inbox", "inbox", 1},
	{"archive", "Archive", "archive", 2},
	{"drafts", "Drafts", "drafts", 3},
	{"sent", "Sent", "sent", 4},
	{"junk", "Junk", "junk", 5},
	{"trash", "Trash", "trash", 6},
	{"quarantine", "Quarantine", "", 7},
}

func mailboxExists(id string) bool {
	for _, m := range mailboxes {
		if m.ID == id {
			return true
		}
	}
	return false
}
