package model

import (
	"slices"
	"strings"
)

// MBThreadKey names one mailbox/thread membership summary.
type MBThreadKey struct{ Mailbox, Thread string }

// MBThread counts one thread's Emails in one mailbox, and how many of them
// are unread. Mailbox thread counters derive from these summaries.
type MBThread struct{ Emails, Unread int64 }

// Derived is the current derived state a plan needs, read strongly by the
// store before it builds the guarded transaction.
type Derived interface {
	MBThread(k MBThreadKey) MBThread
	// ThreadSize is the number of Emails in the thread before the commit.
	ThreadSize(threadID string) int64
}

// Effects are everything a mutation changes besides the objects
// themselves.
type Effects struct {
	// MBThreads holds the new value of every touched summary; a zero value
	// means the summary is deleted.
	MBThreads map[MBThreadKey]MBThread
	// Counts holds counter deltas per mailbox.
	Counts map[string]Counts
	Change Change
	// The data types whose state advances.
	Email, Mailbox, Thread bool
	// SearchDirty lists Emails the index must re-read.
	SearchDirty []string
}

// Keys returns the summaries and threads a mutation touches, for the store
// to read before calling Plan.
func (m Mutation) Keys() (summaries []MBThreadKey, threads []string) {
	add := func(e *Email) {
		if e == nil {
			return
		}
		for mb := range e.MailboxIDs {
			k := MBThreadKey{mb, e.ThreadID}
			if !slices.Contains(summaries, k) {
				summaries = append(summaries, k)
			}
		}
	}
	for _, c := range m.Emails {
		add(c.Before)
		add(c.After)
		if (c.Before == nil) != (c.After == nil) {
			e := c.After
			if e == nil {
				e = c.Before
			}
			if !slices.Contains(threads, e.ThreadID) {
				threads = append(threads, e.ThreadID)
			}
		}
	}
	slices.SortFunc(summaries, compareKeys)
	slices.Sort(threads)
	return summaries, threads
}

func compareKeys(a, b MBThreadKey) int {
	if c := strings.Compare(a.Mailbox, b.Mailbox); c != 0 {
		return c
	}
	return strings.Compare(a.Thread, b.Thread)
}

// Plan derives counters, thread changes and the change record from a
// mutation (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
// Thread membership is fixed at creation, so updates never move an Email
// between threads.
func Plan(m Mutation, d Derived) Effects {
	ef := Effects{
		MBThreads: map[MBThreadKey]MBThread{},
		Counts:    map[string]Counts{},
		Change:    Change{Created: map[string][]string{}, Updated: map[string][]string{}, Destroyed: map[string][]string{}},
	}
	delta := map[MBThreadKey]MBThread{}
	apply := func(e *Email, sign int64) {
		if e == nil {
			return
		}
		unread := int64(0)
		if e.Unread() {
			unread = 1
		}
		for mb := range e.MailboxIDs {
			k := MBThreadKey{mb, e.ThreadID}
			v := delta[k]
			v.Emails += sign
			v.Unread += sign * unread
			delta[k] = v
		}
	}
	threadDelta := map[string]int64{}
	var threadOrder []string
	note := func(list map[string][]string, typ, id string) {
		if !slices.Contains(list[typ], id) {
			list[typ] = append(list[typ], id)
		}
	}
	for _, c := range m.Emails {
		apply(c.Before, -1)
		apply(c.After, +1)
		switch {
		case c.Before == nil && c.After != nil:
			note(ef.Change.Created, "Email", c.After.ID)
			threadDelta[c.After.ThreadID]++
			if !slices.Contains(threadOrder, c.After.ThreadID) {
				threadOrder = append(threadOrder, c.After.ThreadID)
			}
			ef.SearchDirty = append(ef.SearchDirty, c.After.ID)
		case c.Before != nil && c.After == nil:
			note(ef.Change.Destroyed, "Email", c.Before.ID)
			threadDelta[c.Before.ThreadID]--
			if !slices.Contains(threadOrder, c.Before.ThreadID) {
				threadOrder = append(threadOrder, c.Before.ThreadID)
			}
			ef.SearchDirty = append(ef.SearchDirty, c.Before.ID)
		case c.Before != nil:
			note(ef.Change.Updated, "Email", c.After.ID)
			ef.SearchDirty = append(ef.SearchDirty, c.After.ID)
		}
	}
	ef.Email = len(m.Emails) > 0

	keys := make([]MBThreadKey, 0, len(delta))
	for k := range delta {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareKeys)
	for _, k := range keys {
		dv := delta[k]
		if dv == (MBThread{}) {
			continue
		}
		old := d.MBThread(k)
		nv := MBThread{old.Emails + dv.Emails, old.Unread + dv.Unread}
		ef.MBThreads[k] = nv
		c := ef.Counts[k.Mailbox]
		c = c.add(Counts{
			TotalEmails:   dv.Emails,
			UnreadEmails:  dv.Unread,
			TotalThreads:  b2i(nv.Emails > 0) - b2i(old.Emails > 0),
			UnreadThreads: b2i(nv.Unread > 0) - b2i(old.Unread > 0),
		})
		ef.Counts[k.Mailbox] = c
	}
	for mb, c := range ef.Counts {
		if c.IsZero() {
			delete(ef.Counts, mb)
		}
	}

	for _, t := range threadOrder {
		before := d.ThreadSize(t)
		after := before + threadDelta[t]
		switch {
		case before == 0 && after > 0:
			note(ef.Change.Created, "Thread", t)
		case before > 0 && after == 0:
			note(ef.Change.Destroyed, "Thread", t)
		case before > 0:
			note(ef.Change.Updated, "Thread", t)
		}
	}
	ef.Thread = len(threadOrder) > 0

	for _, c := range m.Mailboxes {
		switch {
		case c.Before == nil:
			note(ef.Change.Created, "Mailbox", c.After.ID)
		case c.After == nil:
			note(ef.Change.Destroyed, "Mailbox", c.Before.ID)
		default:
			note(ef.Change.Updated, "Mailbox", c.After.ID)
		}
	}
	countOnly := []string{}
	for mb := range ef.Counts {
		if !slices.Contains(ef.Change.Created["Mailbox"], mb) && !slices.Contains(ef.Change.Updated["Mailbox"], mb) &&
			!slices.Contains(ef.Change.Destroyed["Mailbox"], mb) {
			countOnly = append(countOnly, mb)
		}
	}
	slices.Sort(countOnly)
	if len(countOnly) > 0 {
		ef.Change.Updated["MailboxCounts"] = countOnly
	}
	ef.Mailbox = len(m.Mailboxes) > 0 || len(ef.Counts) > 0
	for _, list := range []map[string][]string{ef.Change.Created, ef.Change.Updated, ef.Change.Destroyed} {
		for k, v := range list {
			if len(v) == 0 {
				delete(list, k)
			}
		}
	}
	return ef
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Next returns the state after committing effects at prev.
func (ef Effects) Next(prev State) State {
	next := prev
	next.Seq = prev.Seq + 1
	if ef.Email {
		next.Email = next.Seq
	}
	if ef.Mailbox {
		next.Mailbox = next.Seq
	}
	if ef.Thread {
		next.Thread = next.Seq
	}
	return next
}
