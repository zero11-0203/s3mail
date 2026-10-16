package ingest

import (
	"slices"
	"strings"
)

// Mailbox roles ingestion can file into. Quarantine is an ordinary folder
// without a JMAP role (docs/specs/storage-sync.md#account-model).
const (
	Inbox      = "inbox"
	Junk       = "junk"
	Quarantine = "quarantine"
)

// CanaryLocalPart is the reserved deployment-canary address.
const CanaryLocalPart = "canary"

// Registry is the runtime alias registry, generated from the same Terraform
// declaration as the SES recipient conditions.
type Registry struct {
	Domain  string
	Aliases []string // base local parts, lowercase
}

// Route returns the envelope recipients that belong to a declared alias,
// keeping the exact delivered address (including any +tag).
func (r Registry) Route(recipients []string) []string {
	var matched []string
	for _, rcpt := range recipients {
		local, domain, ok := strings.Cut(strings.ToLower(rcpt), "@")
		if !ok || domain != r.Domain {
			continue
		}
		base, _, _ := strings.Cut(local, "+")
		if slices.Contains(r.Aliases, base) && !slices.Contains(matched, rcpt) {
			matched = append(matched, rcpt)
		}
	}
	return matched
}

// Disposition picks the mailbox for an accepted delivery and the reason
// for a non-Inbox choice (docs/specs/mail-flow.md#recipient-and-processing-policy).
// SES has already accepted the message, so nothing here bounces it.
func Disposition(v Verdicts, matched []string) (mailbox, reason string) {
	switch {
	case len(matched) == 0:
		// SES accepted mail the registry does not know: a registry and
		// deployment race. Keep it and alert instead of discarding.
		return Quarantine, "unmatched-recipient"
	case v.Virus == "FAIL":
		return Quarantine, "virus"
	case !known(v.Virus) || !known(v.Spam):
		return Quarantine, "verdict-unknown"
	case v.DMARC == "FAIL" && (v.DMARCPolicy == "QUARANTINE" || v.DMARCPolicy == "REJECT"):
		return Quarantine, "dmarc"
	case v.Spam == "FAIL":
		return Junk, "spam"
	case onlyCanary(matched):
		// No canary run records exist yet, so a canary delivery cannot be
		// validated against a nonce; keep it out of the Inbox.
		return Quarantine, "canary"
	}
	return Inbox, ""
}

func known(status string) bool { return status == "PASS" || status == "FAIL" }

func onlyCanary(matched []string) bool {
	for _, m := range matched {
		local, _, _ := strings.Cut(strings.ToLower(m), "@")
		if base, _, _ := strings.Cut(local, "+"); base != CanaryLocalPart {
			return false
		}
	}
	return true
}
