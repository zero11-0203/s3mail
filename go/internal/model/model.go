// Package model is the mail account's object model and the pure rules
// derived from it: threading, mailbox/thread counters and change records
// (docs/specs/storage-sync.md). The DynamoDB store, ingestion and the JMAP
// server share it, so every writer derives the same records.
package model

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"slices"
	"strings"
	"time"
)

// State is the account commit boundary: the account sequence, the restore
// epoch and, per data type, the sequence of the last commit that changed
// that type. A type's state may advance conservatively.
type State struct {
	Seq        int64
	Epoch      int64
	Email      int64
	Mailbox    int64
	Thread     int64
	Identity   int64
	Submission int64
}

// Of returns the state sequence of one JMAP data type.
func (s State) Of(typ string) int64 {
	switch typ {
	case "Email":
		return s.Email
	case "Mailbox":
		return s.Mailbox
	case "Thread":
		return s.Thread
	case "Identity":
		return s.Identity
	case "EmailSubmission":
		return s.Submission
	}
	return s.Seq
}

// BlobRef is the exact original object behind an Email.
type BlobRef struct {
	Bucket    string
	Key       string
	VersionID string
}

// Email sources: where the original MIME came from.
const (
	SourceReceipt  = "receipt"
	SourceComposed = "composed"
	SourceImported = "imported"
)

// Email is the stored metadata of one Email.
type Email struct {
	ID         string
	ThreadID   string
	MailboxIDs map[string]bool
	Keywords   map[string]bool
	ReceivedAt time.Time
	Size       int64
	Blob       BlobRef
	Source     string

	// Header projection for filters, sorting and threading; Email/get
	// header properties are parsed from the raw message instead.
	Subject    string
	From       []string
	To         []string
	Cc         []string
	SentAt     time.Time
	MessageID  string
	InReplyTo  string
	References string

	// Envelope is set for SES deliveries only.
	Envelope *Envelope
}

// Envelope is the SMTP and processing record of an SES delivery.
type Envelope struct {
	SESMessageID       string
	EnvelopeSender     string
	EnvelopeRecipients []string
	DeliveredTo        []string
	Verdicts           map[string]string
	Reason             string
	ReplyTo            []string
	Incomplete         []string
	ParserVersion      int
}

// Unread follows RFC 8621 §2: neither $seen nor $draft.
func (e *Email) Unread() bool { return !e.Keywords["$seen"] && !e.Keywords["$draft"] }

// Clone copies the mutable maps.
func (e Email) Clone() Email {
	e.MailboxIDs = cloneSet(e.MailboxIDs)
	e.Keywords = cloneSet(e.Keywords)
	return e
}

func cloneSet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		if v {
			out[k] = true
		}
	}
	return out
}

// Mailbox is a stored mailbox. ParentID and Role are "" for null.
type Mailbox struct {
	ID           string
	Name         string
	ParentID     string
	Role         string
	SortOrder    int64
	IsSubscribed bool
}

// IsSystem reports whether the mailbox is one of the fixed system mailboxes,
// which cannot be destroyed or re-parented and keep their role.
func (m Mailbox) IsSystem() bool {
	_, ok := systemByID[m.ID]
	return ok
}

// System mailboxes exist without a stored record; a stored MAILBOX# record
// overrides the default (docs/specs/storage-sync.md#account-model).
// Quarantine has no JMAP role.
var SystemMailboxes = []Mailbox{
	{ID: "inbox", Name: "Inbox", Role: "inbox", SortOrder: 1, IsSubscribed: true},
	{ID: "archive", Name: "Archive", Role: "archive", SortOrder: 2, IsSubscribed: true},
	{ID: "drafts", Name: "Drafts", Role: "drafts", SortOrder: 3, IsSubscribed: true},
	{ID: "sent", Name: "Sent", Role: "sent", SortOrder: 4, IsSubscribed: true},
	{ID: "junk", Name: "Junk", Role: "junk", SortOrder: 5, IsSubscribed: true},
	{ID: "trash", Name: "Trash", Role: "trash", SortOrder: 6, IsSubscribed: true},
	{ID: "quarantine", Name: "Quarantine", SortOrder: 7, IsSubscribed: true},
}

var systemByID = func() map[string]Mailbox {
	m := map[string]Mailbox{}
	for _, b := range SystemMailboxes {
		m[b.ID] = b
	}
	return m
}()

// Mailboxes merges stored records over the system defaults, in stable
// order: system mailboxes first, then stored ones by id.
func Mailboxes(stored []Mailbox) []Mailbox {
	byID := map[string]Mailbox{}
	for _, m := range stored {
		byID[m.ID] = m
	}
	var out []Mailbox
	for _, d := range SystemMailboxes {
		if m, ok := byID[d.ID]; ok {
			out = append(out, m)
			delete(byID, d.ID)
		} else {
			out = append(out, d)
		}
	}
	var rest []Mailbox
	for _, m := range byID {
		rest = append(rest, m)
	}
	sortMailboxes(rest)
	return append(out, rest...)
}

func sortMailboxes(ms []Mailbox) {
	slices.SortFunc(ms, func(a, b Mailbox) int { return strings.Compare(a.ID, b.ID) })
}

// Counts are a mailbox's RFC 8621 §2 counters.
type Counts struct {
	TotalEmails   int64
	UnreadEmails  int64
	TotalThreads  int64
	UnreadThreads int64
}

func (c Counts) add(d Counts) Counts {
	return Counts{c.TotalEmails + d.TotalEmails, c.UnreadEmails + d.UnreadEmails, c.TotalThreads + d.TotalThreads, c.UnreadThreads + d.UnreadThreads}
}

// IsZero reports whether every counter is zero.
func (c Counts) IsZero() bool { return c == Counts{} }

// Change is one committed CHANGE record: object ids by type. Updated
// "MailboxCounts" lists mailboxes whose counters alone changed.
type Change struct {
	Seq       int64
	Created   map[string][]string
	Updated   map[string][]string
	Destroyed map[string][]string
	// At is the commit time; the search reader measures index lag by it.
	At time.Time
}

// SearchIndex is a published index generation registered in the account
// (docs/specs/search.md#index-pipeline).
type SearchIndex struct {
	Generation  string
	Seq, Epoch  int64
	Key         string
	VersionID   string
	ETag        string
	PublishedAt time.Time
}

// SearchRegistration replaces the registered generation Prev ("" for
// none) with Next.
type SearchRegistration struct {
	Prev string
	Next SearchIndex
}

// Upload is an uploaded blob in the disposable blob bucket.
type Upload struct {
	ID        string
	Type      string
	Size      int64
	Bucket    string
	Key       string
	Created   time.Time
	ExpiresAt time.Time
}

// Event is outbox work for the event bus (no content).
type Event struct {
	ID         string
	Type       string
	EmailID    string
	MailboxIDs []string
}

// Receipt is the durable replay marker of an SES delivery.
type Receipt struct {
	SESMessageID  string
	EmailID       string
	Blob          BlobRef
	ReceivedAt    time.Time
	ParserVersion int
}

// EmailChange is a create (Before nil), destroy (After nil) or update.
type EmailChange struct{ Before, After *Email }

// MailboxChange is a create, destroy or update of a stored mailbox.
// Before may be a system default that has no stored record yet.
type MailboxChange struct{ Before, After *Mailbox }

// IdentityChange is a create, destroy or update of a stored identity.
type IdentityChange struct{ Before, After *Identity }

// SubmissionChange is a create or update of a submission; submissions are
// never deleted, a destroyed one is only hidden from JMAP.
type SubmissionChange struct{ Before, After *Submission }

// QuotaDelta adds Units to one relay quota counter. A positive delta is a
// reservation and fails the commit with ErrOverQuota when the counter
// would exceed Limit; a negative delta releases a reservation.
type QuotaDelta struct {
	Key   string
	Units int64
	Limit int64
}

// Attempt is one appended record of a submission attempt's history
// (docs/specs/mail-flow.md#outbound-email); records are never rewritten.
type Attempt struct {
	SubmissionID string
	Attempt      int
	Seq          int // record number within the attempt
	Phase        string
	Owner        string
	At           time.Time
	Detail       string
}

// DeliveryMarker is the durable deduplication record of one relay
// delivery report (docs/specs/mail-flow.md#webhook-idempotency).
type DeliveryMarker struct {
	Relay        string
	EventID      string
	SubmissionID string
	At           time.Time
}

// Mutation is one guarded commit.
type Mutation struct {
	Emails      []EmailChange
	Mailboxes   []MailboxChange
	Identities  []IdentityChange
	Submissions []SubmissionChange
	Quota       []QuotaDelta
	Attempts    []Attempt
	// Send lists submissions to hand to the submission queue through the
	// outbox.
	Send []string
	// Delivery records a relay delivery report as applied; the commit
	// fails with ErrDuplicateEvent when it already was.
	Delivery *DeliveryMarker
	Receipt  *Receipt
	Uploads  []Upload
	Events   []Event
	// Search registers a published index; it advances the Email state so
	// clients rerun text queries, without making anything search-dirty.
	Search *SearchRegistration
}

// Errors returned by stores.
var (
	// ErrStateChanged means another commit advanced the account sequence.
	ErrStateChanged = errors.New("account state changed")
	// ErrSearchConflict means another generation was registered first.
	ErrSearchConflict = errors.New("search registration changed")
	// ErrDuplicateReceipt means the receipt marker already exists.
	ErrDuplicateReceipt = errors.New("receipt already committed")
	// ErrTooLarge means the commit exceeds the transaction limits.
	ErrTooLarge = errors.New("commit exceeds the transaction limits")
	// ErrOverQuota means a quota reservation would exceed its limit.
	ErrOverQuota = errors.New("relay quota exhausted")
	// ErrDuplicateEvent means the delivery report was already applied.
	ErrDuplicateEvent = errors.New("delivery event already applied")
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewID returns prefix + 24 random lowercase base32 characters (120 bits),
// the same shape as derived Email ids.
func NewID(prefix string) string {
	var b [15]byte
	_, _ = rand.Read(b[:])
	return prefix + strings.ToLower(idEncoding.EncodeToString(b[:]))
}

// ValidID reports whether s is prefix plus 1-64 id characters.
func ValidID(s string, prefix byte) bool {
	if len(s) < 2 || len(s) > 65 || s[0] != prefix {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}
