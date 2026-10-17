package model

import (
	"context"
	"slices"
	"testing"
	"time"
)

type derived struct {
	mbt  map[MBThreadKey]MBThread
	size map[string]int64
}

func (d derived) MBThread(k MBThreadKey) MBThread { return d.mbt[k] }
func (d derived) ThreadSize(t string) int64       { return d.size[t] }

func email(id, thread string, mailboxes []string, kw ...string) *Email {
	e := &Email{ID: id, ThreadID: thread, MailboxIDs: map[string]bool{}, Keywords: map[string]bool{}}
	for _, m := range mailboxes {
		e.MailboxIDs[m] = true
	}
	for _, k := range kw {
		e.Keywords[k] = true
	}
	return e
}

func TestPlanCreateIntoNewThread(t *testing.T) {
	a := email("Ma", "Ta", []string{"inbox"})
	ef := Plan(Mutation{Emails: []EmailChange{{After: a}}}, derived{})
	if got := ef.Counts["inbox"]; got != (Counts{1, 1, 1, 1}) {
		t.Errorf("counts %+v", got)
	}
	if ef.Change.Created["Email"][0] != "Ma" || ef.Change.Created["Thread"][0] != "Ta" {
		t.Errorf("change %+v", ef.Change)
	}
	if !slices.Equal(ef.Change.Updated["MailboxCounts"], []string{"inbox"}) || !ef.Email || !ef.Mailbox || !ef.Thread || !ef.Delivery {
		t.Errorf("types %+v", ef)
	}
	if next := ef.Next(State{Seq: 4, Epoch: 1}); next.Delivery != 5 || next.Of("EmailDelivery") != 5 {
		t.Errorf("delivery state %+v", next)
	}
	if ef.MBThreads[MBThreadKey{"inbox", "Ta"}] != (MBThread{1, 1}) {
		t.Errorf("summary %+v", ef.MBThreads)
	}
}

func TestPlanThreadCounters(t *testing.T) {
	// Ta already has one unread Email in the inbox; a second unread one
	// joins: emails +1, unread +1, threads unchanged.
	d := derived{mbt: map[MBThreadKey]MBThread{{"inbox", "Ta"}: {1, 1}}, size: map[string]int64{"Ta": 1}}
	b := email("Mb", "Ta", []string{"inbox"})
	ef := Plan(Mutation{Emails: []EmailChange{{After: b}}}, d)
	if got := ef.Counts["inbox"]; got != (Counts{1, 1, 0, 0}) {
		t.Errorf("join counts %+v", got)
	}
	if !slices.Equal(ef.Change.Updated["Thread"], []string{"Ta"}) {
		t.Errorf("thread change %+v", ef.Change)
	}

	// Marking the thread's only unread Email seen: unreadThreads -1.
	d = derived{mbt: map[MBThreadKey]MBThread{{"inbox", "Ta"}: {2, 1}}, size: map[string]int64{"Ta": 2}}
	before := email("Mb", "Ta", []string{"inbox"})
	after := email("Mb", "Ta", []string{"inbox"}, "$seen")
	ef = Plan(Mutation{Emails: []EmailChange{{Before: before, After: after}}}, d)
	if got := ef.Counts["inbox"]; got != (Counts{0, -1, 0, -1}) {
		t.Errorf("seen counts %+v", got)
	}
	if ef.Thread || ef.Change.Updated["Thread"] != nil {
		t.Error("a keyword change does not change the thread")
	}

	// Moving the last inbox Email of a thread to Archive.
	d = derived{mbt: map[MBThreadKey]MBThread{{"inbox", "Ta"}: {1, 0}}, size: map[string]int64{"Ta": 2}}
	before = email("Mb", "Ta", []string{"inbox"}, "$seen")
	after = email("Mb", "Ta", []string{"archive"}, "$seen")
	ef = Plan(Mutation{Emails: []EmailChange{{Before: before, After: after}}}, d)
	if ef.Counts["inbox"] != (Counts{-1, 0, -1, 0}) || ef.Counts["archive"] != (Counts{1, 0, 1, 0}) {
		t.Errorf("move counts %+v", ef.Counts)
	}
	if v, ok := ef.MBThreads[MBThreadKey{"inbox", "Ta"}]; !ok || v != (MBThread{}) {
		t.Errorf("the emptied summary must be deleted: %+v", ef.MBThreads)
	}

	// Drafts are not unread.
	ef = Plan(Mutation{Emails: []EmailChange{{After: email("Md", "Td", []string{"drafts"}, "$draft")}}}, derived{})
	if ef.Counts["drafts"] != (Counts{1, 0, 1, 0}) {
		t.Errorf("draft counts %+v", ef.Counts)
	}
}

func TestPlanDestroyLastEmailDestroysThread(t *testing.T) {
	d := derived{mbt: map[MBThreadKey]MBThread{{"trash", "Ta"}: {1, 0}}, size: map[string]int64{"Ta": 1}}
	ef := Plan(Mutation{Emails: []EmailChange{{Before: email("Ma", "Ta", []string{"trash"}, "$seen")}}}, d)
	if ef.Change.Destroyed["Email"][0] != "Ma" || ef.Change.Destroyed["Thread"][0] != "Ta" {
		t.Errorf("change %+v", ef.Change)
	}
	if ef.Counts["trash"] != (Counts{-1, 0, -1, 0}) {
		t.Errorf("counts %+v", ef.Counts)
	}
	next := ef.Next(State{Seq: 4, Epoch: 1, Email: 3, Mailbox: 2, Thread: 1})
	if next != (State{Seq: 5, Epoch: 1, Email: 5, Mailbox: 5, Thread: 5}) {
		t.Errorf("next %+v", next)
	}
}

func TestPlanMailboxOnly(t *testing.T) {
	ef := Plan(Mutation{Mailboxes: []MailboxChange{{After: &Mailbox{ID: "Bx", Name: "x"}}}}, derived{})
	if ef.Email || ef.Thread || !ef.Mailbox || ef.Change.Created["Mailbox"][0] != "Bx" || len(ef.Change.Updated) != 0 {
		t.Errorf("effects %+v", ef)
	}
	next := ef.Next(State{Seq: 4, Epoch: 1, Email: 3, Mailbox: 2, Thread: 1})
	if next.Email != 3 || next.Mailbox != 5 || next.Thread != 1 {
		t.Errorf("next %+v", next)
	}
}

func TestBaseSubject(t *testing.T) {
	for in, want := range map[string]struct {
		base  string
		reply bool
	}{
		"Hello  World":              {"hello world", false},
		"Re: Hello":                 {"hello", true},
		"RE: Fwd: [list] Re: Hello": {"hello", true},
		"AW: Treffen":               {"treffen", true},
		"回复：会议":                     {"会议", true},
		"Re[2]: status":             {"status", true},
		"[announce] release":        {"release", false},
		"Re:":                       {"", true},
	} {
		base, reply := BaseSubject(in)
		if base != want.base || reply != want.reply {
			t.Errorf("%q → %q %v, want %q %v", in, base, reply, want.base, want.reply)
		}
	}
}

type lookup struct {
	msg  map[string][]ThreadRef
	subj map[string][]ThreadRef
}

func (l lookup) ByMessageID(_ context.Context, keys []string) (map[string][]ThreadRef, error) {
	out := map[string][]ThreadRef{}
	for _, k := range keys {
		if r, ok := l.msg[k]; ok {
			out[k] = r
		}
	}
	return out, nil
}

func (l lookup) BySubject(_ context.Context, key string) ([]ThreadRef, error) {
	return l.subj[key], nil
}

func TestAssignThread(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	parent := &Email{ID: "Mp", ThreadID: "Tp", MessageID: "<p@example.net>", Subject: "Plan", From: []string{"Bob <bob@example.net>"}, ReceivedAt: now}
	index := lookup{msg: map[string][]ThreadRef{}, subj: map[string][]ThreadRef{}}
	for _, k := range parent.MessageIDKeys() {
		index.msg[k] = append(index.msg[k], ThreadRef{EmailID: parent.ID, ThreadID: parent.ThreadID, ReceivedAt: now})
	}
	index.subj[parent.SubjectKey()] = []ThreadRef{{EmailID: parent.ID, ThreadID: parent.ThreadID, ReceivedAt: now, Addresses: parent.Addresses()}}

	reply := &Email{ID: "Mr", MessageID: "<r@example.com>", InReplyTo: "<p@example.net>", Subject: "Re: Plan", ReceivedAt: now.Add(time.Hour)}
	if got, _ := AssignThread(ctx, reply, index); got != "Tp" {
		t.Errorf("reply by In-Reply-To: %s", got)
	}

	// A reply that lost its references joins by subject and address.
	bare := &Email{ID: "Mb", Subject: "RE: plan", To: []string{"bob@example.net"}, ReceivedAt: now.Add(48 * time.Hour)}
	if got, _ := AssignThread(ctx, bare, index); got != "Tp" {
		t.Errorf("reply by subject: %s", got)
	}
	// Same subject without a reply prefix, or no shared address, or too
	// late: a new thread.
	for _, e := range []*Email{
		{ID: "Mx", Subject: "Plan", To: []string{"bob@example.net"}, ReceivedAt: now},
		{ID: "My", Subject: "Re: Plan", To: []string{"carol@example.net"}, ReceivedAt: now},
		{ID: "Mz", Subject: "Re: Plan", To: []string{"bob@example.net"}, ReceivedAt: now.Add(40 * 24 * time.Hour)},
	} {
		if got, _ := AssignThread(ctx, e, index); got != "T"+e.ID[1:] {
			t.Errorf("%s joined %s", e.ID, got)
		}
	}

	// The parent arriving after a reply finds the reply's thread through
	// the reply's indexed In-Reply-To.
	late := lookup{msg: map[string][]ThreadRef{}}
	child := &Email{ID: "Mc", ThreadID: "Tc", MessageID: "<c@example.com>", InReplyTo: "<q@example.net>"}
	for _, k := range child.MessageIDKeys() {
		late.msg[k] = append(late.msg[k], ThreadRef{EmailID: child.ID, ThreadID: child.ThreadID})
	}
	q := &Email{ID: "Mq", MessageID: "<q@example.net>"}
	if got, _ := AssignThread(ctx, q, late); got != "Tc" {
		t.Errorf("parent after child: %s", got)
	}
}

func TestMailboxesMergeOverrides(t *testing.T) {
	got := Mailboxes([]Mailbox{{ID: "inbox", Name: "Posteingang", Role: "inbox"}, {ID: "Bz"}, {ID: "Ba"}})
	if len(got) != 9 || got[0].Name != "Posteingang" || got[7].ID != "Ba" || got[8].ID != "Bz" {
		t.Errorf("merged %+v", got)
	}
}
