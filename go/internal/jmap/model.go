// Package jmap serves JMAP Core and Mail (RFC 8620, RFC 8621) over the
// mail table (docs/specs/jmap.md). Every advertised method follows the
// conformance matrix in docs/specs/jmap-conformance.md.
package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// The object model is shared with the store and ingestion.
type (
	State     = model.State
	BlobRef   = model.BlobRef
	EmailMeta = model.Email
	Mailbox   = model.Mailbox
	Change    = model.Change
)

// ErrStateChanged means another commit advanced the account sequence.
var ErrStateChanged = model.ErrStateChanged

// Store reads and mutates one account's records.
type Store interface {
	State(ctx context.Context) (State, error)
	ListEmails(ctx context.Context) ([]EmailMeta, error)
	GetEmails(ctx context.Context, ids []string) (map[string]EmailMeta, error)
	ListMailboxes(ctx context.Context) ([]Mailbox, error)
	MailboxCounts(ctx context.Context) (map[string]model.Counts, error)
	// ThreadMembers returns each thread's Email ids, oldest first.
	ThreadMembers(ctx context.Context, threadIDs []string) (map[string][]string, error)
	// Changes returns the CHANGE records with after < seq <= upto.
	Changes(ctx context.Context, after, upto int64) ([]Change, error)
	// Commit applies one mutation guarded by prev and returns the new state.
	Commit(ctx context.Context, prev State, m model.Mutation) (State, error)
	GetUpload(ctx context.Context, id string) (model.Upload, bool, error)
	ListIdentities(ctx context.Context) ([]model.Identity, error)
	// ListSubmissions includes destroyed (hidden) submissions.
	ListSubmissions(ctx context.Context) ([]model.Submission, error)
	GetSubmission(ctx context.Context, id string) (model.Submission, bool, error)
	model.ThreadLookup
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

// stateToken encodes a type's state: "<epoch>-<seq>", where seq is the
// last commit that changed the type. Any sequence between that commit and
// the current one describes the same data, so tokens advance
// conservatively.
func stateToken(s State, typ string) string { return fmt.Sprintf("%d-%d", s.Epoch, s.Of(typ)) }

// seqToken encodes an arbitrary commit boundary (a /changes page end).
func seqToken(epoch, seq int64) string { return fmt.Sprintf("%d-%d", epoch, seq) }

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

// inState reports whether a client's ifInState token names the type's
// current state: any sequence from the type's last change on.
func inState(tok string, cur State, typ string) bool {
	seq, ok := parseState(tok, cur)
	return ok && seq >= cur.Of(typ)
}

func validCore(s string) bool { return model.ValidID("x"+s, 'x') }
