package model

import (
	"crypto/sha256"
	"strings"
	"time"
)

// Address is a JMAP EmailAddress (name may be empty).
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Identity is a sending identity. Identities exist for exactly the
// declared aliases (docs/specs/mail-flow.md#alias-policy); the stored
// record holds the owner's editable properties.
type Identity struct {
	ID            string
	Email         string
	Name          string
	ReplyTo       []Address
	Bcc           []Address
	TextSignature string
	HTMLSignature string
}

// IdentityID is the stable id of the identity for a lowercase address.
func IdentityID(email string) string {
	sum := sha256.Sum256([]byte("identity\x00" + strings.ToLower(email)))
	return "I" + strings.ToLower(idEncoding.EncodeToString(sum[:15]))
}

// Submission statuses (docs/specs/mail-flow.md#outbound-email).
const (
	// SubmissionQueued waits for a worker (first try or a retry).
	SubmissionQueued = "queued"
	// SubmissionActive is claimed by a worker under a lease.
	SubmissionActive = "active"
	// SubmissionAccepted means the relay accepted the message.
	SubmissionAccepted = "accepted"
	// SubmissionRejected means the relay definitely did not accept it.
	SubmissionRejected = "rejected"
	// SubmissionUnknown means DATA may have been accepted: never resent
	// automatically.
	SubmissionUnknown = "unknown"
	// SubmissionCanceled was canceled before handoff.
	SubmissionCanceled = "canceled"
)

// RecipientStatus is one recipient's delivery status (RFC 8621 §7).
type RecipientStatus struct {
	SMTPReply string `json:"smtpReply"`
	// Delivered is queued, yes, no or unknown.
	Delivered string `json:"delivered"`
}

// Reservation is the quota a submission holds, by counter key.
type Reservation struct {
	Units int64
	Keys  []string
}

// Submission is an EmailSubmission with its send state. Blob pins the
// exact original version that is sent; it stays readable after the Email
// is destroyed because originals are never deleted.
type Submission struct {
	ID         string
	IdentityID string
	EmailID    string
	ThreadID   string
	MailFrom   string
	RcptTo     []string
	SendAt     time.Time
	Blob       BlobRef
	Size       int64
	Relay      string

	Status        string
	Attempt       int
	Owner         string
	LeaseUntil    time.Time
	Handoff       bool
	NextAttemptAt time.Time
	Recipients    map[string]RecipientStatus
	Reservation   Reservation
	// Destroyed hides the submission from JMAP; its send state and
	// evidence remain.
	Destroyed bool
}

// UndoStatus maps the send state to RFC 8621 undoStatus.
func (s *Submission) UndoStatus() string {
	switch {
	case s.Status == SubmissionCanceled:
		return "canceled"
	case s.Status == SubmissionQueued, s.Status == SubmissionActive && !s.Handoff:
		return "pending"
	}
	return "final"
}

// Terminal reports whether no worker will act on the submission again.
func (s *Submission) Terminal() bool {
	switch s.Status {
	case SubmissionAccepted, SubmissionRejected, SubmissionUnknown, SubmissionCanceled:
		return true
	}
	return false
}

// Clone copies the mutable fields.
func (s Submission) Clone() Submission {
	s.RcptTo = append([]string(nil), s.RcptTo...)
	r := make(map[string]RecipientStatus, len(s.Recipients))
	for k, v := range s.Recipients {
		r[k] = v
	}
	s.Recipients = r
	s.Reservation.Keys = append([]string(nil), s.Reservation.Keys...)
	return s
}

// SetRecipients gives every recipient the same status.
func (s *Submission) SetRecipients(delivered, reply string) {
	s.Recipients = map[string]RecipientStatus{}
	for _, r := range s.RcptTo {
		s.Recipients[r] = RecipientStatus{SMTPReply: reply, Delivered: delivered}
	}
}
