package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/zero11-0203/s3mail/go/internal/mailparse"
)

// Threading bounds: at most this many referenced ids are indexed per
// Email, newest first, and the subject fallback only joins threads with a
// message this close in time.
const (
	maxIndexedReferences = 20
	subjectWindow        = 30 * 24 * time.Hour
)

// ThreadRef is an indexed Email found by a threading lookup.
type ThreadRef struct {
	EmailID    string
	ThreadID   string
	ReceivedAt time.Time
	Addresses  []string
}

// ThreadLookup reads the MESSAGEID and SUBJECT index records strongly.
type ThreadLookup interface {
	// ByMessageID returns the Emails indexed under each key.
	ByMessageID(ctx context.Context, keys []string) (map[string][]ThreadRef, error)
	BySubject(ctx context.Context, key string) ([]ThreadRef, error)
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:12])
}

func messageIDs(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return mailparse.AsMessageIds(raw)
}

// MessageIDKeys returns the MESSAGEID index keys of an Email: its own
// Message-ID, its In-Reply-To ids and its newest References. Indexing both
// directions lets a reply that arrives first and its parent find each
// other.
func (e *Email) MessageIDKeys() []string {
	var ids []string
	ids = append(ids, messageIDs(e.MessageID)...)
	ids = append(ids, messageIDs(e.InReplyTo)...)
	refs := messageIDs(e.References)
	for i := len(refs) - 1; i >= 0 && len(refs)-i <= maxIndexedReferences; i-- {
		ids = append(ids, refs[i])
	}
	var keys []string
	for _, id := range ids {
		if k := hashKey("msgid\x00" + id); !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

var replyPrefix = regexp.MustCompile(`^\s*(?:\[[^\]]{1,40}\]\s*)*(?i:re|fwd?|aw|wg|sv|vs|antw|res|rv|tr|回复|回覆|答复|转发|轉寄)\s*(?:\[\d+\]|\(\d+\))?\s*[:：]\s*`)

var listTag = regexp.MustCompile(`^\s*\[[^\]]{1,40}\]\s*`)

// BaseSubject strips reply/forward prefixes and list tags, then folds case
// and whitespace. isReply reports whether a prefix was removed.
func BaseSubject(subject string) (base string, isReply bool) {
	s := subject
	for {
		if loc := replyPrefix.FindStringIndex(s); loc != nil {
			s = s[loc[1]:]
			isReply = true
			continue
		}
		if loc := listTag.FindStringIndex(s); loc != nil && loc[1] < len(s) {
			s = s[loc[1]:]
			continue
		}
		break
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "(fwd)")
	s = strings.Join(strings.FieldsFunc(strings.ToLower(s), unicode.IsSpace), " ")
	return s, isReply
}

// SubjectKey is the SUBJECT index key, or "" when there is no base subject.
func (e *Email) SubjectKey() string {
	base, _ := BaseSubject(e.Subject)
	if base == "" {
		return ""
	}
	return hashKey("subject\x00" + base)
}

// Addresses returns the lowercased bare From, To and Cc addresses.
func (e *Email) Addresses() []string {
	var out []string
	for _, list := range [][]string{e.From, e.To, e.Cc} {
		for _, s := range list {
			a := strings.ToLower(strings.TrimSpace(s))
			if p, err := mail.ParseAddress(s); err == nil {
				a = strings.ToLower(p.Address)
			}
			if a != "" && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// AssignThread chooses the thread of a new Email: the thread of an Email
// sharing a Message-ID reference (In-Reply-To first, then the newest
// References, then Emails that referenced this one), else, for a reply
// subject, the newest Email with the same base subject and a shared
// address within 30 days, else a new thread. Existing threads are never
// renumbered or merged.
func AssignThread(ctx context.Context, e *Email, l ThreadLookup) (string, error) {
	newThread := "T" + e.ID[1:]
	keys := e.MessageIDKeys()
	if len(keys) > 0 {
		// Own id first in the key list; search parents before children.
		order := append(append([]string{}, keys[1:]...), keys[0])
		if messageIDs(e.MessageID) == nil {
			order = keys
		}
		found, err := l.ByMessageID(ctx, keys)
		if err != nil {
			return "", err
		}
		for _, k := range order {
			if ref, ok := earliest(found[k], e.ID); ok {
				return ref.ThreadID, nil
			}
		}
	}
	base, isReply := BaseSubject(e.Subject)
	if !isReply || base == "" {
		return newThread, nil
	}
	cands, err := l.BySubject(ctx, e.SubjectKey())
	if err != nil {
		return "", err
	}
	addrs := e.Addresses()
	var best *ThreadRef
	for i := range cands {
		c := &cands[i]
		if c.EmailID == e.ID {
			continue
		}
		gap := e.ReceivedAt.Sub(c.ReceivedAt)
		if gap > subjectWindow || gap < -subjectWindow {
			continue
		}
		if !slices.ContainsFunc(c.Addresses, func(a string) bool { return slices.Contains(addrs, a) }) {
			continue
		}
		if best == nil || c.ReceivedAt.After(best.ReceivedAt) || (c.ReceivedAt.Equal(best.ReceivedAt) && c.EmailID > best.EmailID) {
			best = c
		}
	}
	if best != nil {
		return best.ThreadID, nil
	}
	return newThread, nil
}

func earliest(refs []ThreadRef, self string) (ThreadRef, bool) {
	var best ThreadRef
	ok := false
	for _, r := range refs {
		if r.EmailID == self {
			continue
		}
		if !ok || r.ReceivedAt.Before(best.ReceivedAt) || (r.ReceivedAt.Equal(best.ReceivedAt) && r.EmailID < best.EmailID) {
			best, ok = r, true
		}
	}
	return best, ok
}
