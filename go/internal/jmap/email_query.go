package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/mail"
	"slices"
	"strings"
	"time"
)

func emailChanges(ctx context.Context, c *call) (any, *MethodError) {
	resp, _, e := objectChanges(ctx, c, "Email", func(ch Change) ([]string, []string, []string) {
		return ch.Created["Email"], ch.Updated["Email"], ch.Destroyed["Email"]
	})
	if e != nil {
		return nil, e
	}
	return resp, nil
}

type emailQueryArgs struct {
	AccountID       string          `json:"accountId"`
	Filter          json.RawMessage `json:"filter"`
	Sort            []comparator    `json:"sort"`
	Position        int64           `json:"position"`
	Anchor          *string         `json:"anchor"`
	AnchorOffset    int64           `json:"anchorOffset"`
	Limit           *int64          `json:"limit"`
	CalculateTotal  bool            `json:"calculateTotal"`
	CollapseThreads bool            `json:"collapseThreads"`
}

type emailQueryChangesArgs struct {
	AccountID       string          `json:"accountId"`
	Filter          json.RawMessage `json:"filter"`
	Sort            []comparator    `json:"sort"`
	SinceQueryState string          `json:"sinceQueryState"`
	MaxChanges      *int64          `json:"maxChanges"`
	UpToID          *string         `json:"upToId"`
	CalculateTotal  bool            `json:"calculateTotal"`
	CollapseThreads bool            `json:"collapseThreads"`
}

// emailCtx is one Email with the other members of its thread.
type emailCtx struct {
	m      EmailMeta
	thread []EmailMeta
}

type predicate = func(emailCtx) bool

// filterParser builds predicates and records whether any condition reads
// other thread members.
type filterParser struct{ threadDependent bool }

// parse builds a predicate from a FilterOperator/FilterCondition tree.
// Text and body search use the search index (docs/specs/search.md).
func (fp *filterParser) parse(raw json.RawMessage, depth int) (predicate, *MethodError) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return func(emailCtx) bool { return true }, nil
	}
	if depth > 10 {
		return nil, methodErr("unsupportedFilter", "filter nesting too deep")
	}
	if p, ok, e := parseOperator(raw, depth, fp.parse); ok || e != nil {
		return p, e
	}
	var f struct {
		InMailbox               *string    `json:"inMailbox"`
		InMailboxOtherThan      []string   `json:"inMailboxOtherThan"`
		Before                  *time.Time `json:"before"`
		After                   *time.Time `json:"after"`
		MinSize                 *int64     `json:"minSize"`
		MaxSize                 *int64     `json:"maxSize"`
		AllInThreadHaveKeyword  *string    `json:"allInThreadHaveKeyword"`
		SomeInThreadHaveKeyword *string    `json:"someInThreadHaveKeyword"`
		NoneInThreadHaveKeyword *string    `json:"noneInThreadHaveKeyword"`
		HasKeyword              *string    `json:"hasKeyword"`
		NotKeyword              *string    `json:"notKeyword"`
		From                    *string    `json:"from"`
		To                      *string    `json:"to"`
		Cc                      *string    `json:"cc"`
		Subject                 *string    `json:"subject"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, methodErr("unsupportedFilter", err.Error())
	}
	if f.AllInThreadHaveKeyword != nil || f.SomeInThreadHaveKeyword != nil || f.NoneInThreadHaveKeyword != nil {
		fp.threadDependent = true
	}
	contains := func(list []string, needle string) bool {
		needle = strings.ToLower(needle)
		for _, s := range list {
			if strings.Contains(strings.ToLower(s), needle) {
				return true
			}
		}
		return false
	}
	kw := func(k *string) string { return strings.ToLower(*k) }
	return func(x emailCtx) bool {
		m := x.m
		switch {
		case f.InMailbox != nil && !m.MailboxIDs[*f.InMailbox]:
			return false
		case f.InMailboxOtherThan != nil && !inOther(m, f.InMailboxOtherThan):
			return false
		case f.Before != nil && !m.ReceivedAt.Before(*f.Before):
			return false
		case f.After != nil && m.ReceivedAt.Before(*f.After):
			return false
		case f.MinSize != nil && m.Size < *f.MinSize:
			return false
		case f.MaxSize != nil && m.Size >= *f.MaxSize:
			return false
		case f.AllInThreadHaveKeyword != nil && !allHave(x.thread, kw(f.AllInThreadHaveKeyword)),
			f.SomeInThreadHaveKeyword != nil && !someHave(x.thread, kw(f.SomeInThreadHaveKeyword)),
			f.NoneInThreadHaveKeyword != nil && someHave(x.thread, kw(f.NoneInThreadHaveKeyword)),
			f.HasKeyword != nil && !m.Keywords[kw(f.HasKeyword)],
			f.NotKeyword != nil && m.Keywords[kw(f.NotKeyword)]:
			return false
		case f.From != nil && !contains(m.From, *f.From),
			f.To != nil && !contains(m.To, *f.To),
			f.Cc != nil && !contains(m.Cc, *f.Cc),
			f.Subject != nil && !contains([]string{m.Subject}, *f.Subject):
			return false
		}
		return true
	}, nil
}

func allHave(thread []EmailMeta, k string) bool {
	return !slices.ContainsFunc(thread, func(m EmailMeta) bool { return !m.Keywords[k] })
}

func someHave(thread []EmailMeta, k string) bool {
	return slices.ContainsFunc(thread, func(m EmailMeta) bool { return m.Keywords[k] })
}

func inOther(m EmailMeta, excluded []string) bool {
	for id := range m.MailboxIDs {
		if !slices.Contains(excluded, id) {
			return true
		}
	}
	return false
}

func firstAddress(list []string) string {
	if len(list) == 0 {
		return ""
	}
	if a, err := mail.ParseAddress(list[0]); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(list[0])
}

// sortFunc builds the comparator and reports whether it reads other
// thread members.
func sortFunc(sorts []comparator) (func(a, b emailCtx) int, bool, *MethodError) {
	if len(sorts) == 0 {
		sorts = []comparator{{Property: "receivedAt", IsAscending: new(bool)}}
	}
	threadDependent := false
	var cmps []func(a, b emailCtx) int
	for _, s := range sorts {
		if s.Collation != "" && s.Collation != "i;unicode-casemap" {
			return nil, false, methodErr("unsupportedSort", "collation "+s.Collation)
		}
		var cmp func(a, b emailCtx) int
		switch s.Property {
		case "receivedAt":
			cmp = func(a, b emailCtx) int { return a.m.ReceivedAt.Compare(b.m.ReceivedAt) }
		case "sentAt":
			cmp = func(a, b emailCtx) int { return a.m.SentAt.Compare(b.m.SentAt) }
		case "size":
			cmp = func(a, b emailCtx) int { return compareInt(a.m.Size, b.m.Size) }
		case "from":
			cmp = func(a, b emailCtx) int { return strings.Compare(firstAddress(a.m.From), firstAddress(b.m.From)) }
		case "to":
			cmp = func(a, b emailCtx) int { return strings.Compare(firstAddress(a.m.To), firstAddress(b.m.To)) }
		case "subject":
			cmp = func(a, b emailCtx) int {
				return strings.Compare(strings.ToLower(a.m.Subject), strings.ToLower(b.m.Subject))
			}
		case "hasKeyword", "allInThreadHaveKeyword", "someInThreadHaveKeyword":
			if s.Keyword == "" {
				return nil, false, methodErr("invalidArguments", s.Property+" needs a keyword")
			}
			k := strings.ToLower(s.Keyword)
			has := func(x emailCtx) bool { return x.m.Keywords[k] }
			switch s.Property {
			case "allInThreadHaveKeyword":
				has = func(x emailCtx) bool { return allHave(x.thread, k) }
				threadDependent = true
			case "someInThreadHaveKeyword":
				has = func(x emailCtx) bool { return someHave(x.thread, k) }
				threadDependent = true
			}
			cmp = func(a, b emailCtx) int { return compareBool(has(a), has(b)) }
		default:
			return nil, false, methodErr("unsupportedSort", s.Property)
		}
		if !s.ascending() {
			inner := cmp
			cmp = func(a, b emailCtx) int { return -inner(a, b) }
		}
		cmps = append(cmps, cmp)
	}
	return func(a, b emailCtx) int {
		for _, cmp := range cmps {
			if r := cmp(a, b); r != 0 {
				return r
			}
		}
		return strings.Compare(a.m.ID, b.m.ID)
	}, threadDependent, nil
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// emailQueryPlan is a parsed Email query.
type emailQueryPlan struct {
	match           predicate
	order           func(a, b emailCtx) int
	collapse        bool
	threadDependent bool
	args            []any
}

func planEmailQuery(filter json.RawMessage, sorts []comparator, collapse bool) (emailQueryPlan, *MethodError) {
	var fp filterParser
	match, e := fp.parse(filter, 0)
	if e != nil {
		return emailQueryPlan{}, e
	}
	order, sortThread, e := sortFunc(sorts)
	if e != nil {
		return emailQueryPlan{}, e
	}
	return emailQueryPlan{
		match: match, order: order, collapse: collapse,
		threadDependent: collapse || fp.threadDependent || sortThread,
		args:            []any{"Email", compactJSON(filter), sorts, collapse},
	}, nil
}

// results evaluates the query over every Email; with collapseThreads only
// the first matching Email of each thread is kept (RFC 8621 §4.4.3).
func (q emailQueryPlan) results(emails []EmailMeta) []string {
	threads := map[string][]EmailMeta{}
	for _, m := range emails {
		threads[m.ThreadID] = append(threads[m.ThreadID], m)
	}
	var hits []emailCtx
	for _, m := range emails {
		x := emailCtx{m: m, thread: threads[m.ThreadID]}
		if q.match(x) {
			hits = append(hits, x)
		}
	}
	slices.SortStableFunc(hits, q.order)
	ids := make([]string, 0, len(hits))
	seen := map[string]bool{}
	for _, x := range hits {
		if q.collapse {
			if seen[x.m.ThreadID] {
				continue
			}
			seen[x.m.ThreadID] = true
		}
		ids = append(ids, x.m.ID)
	}
	return ids
}

func (s *Server) listEmails(ctx context.Context) ([]EmailMeta, State, *MethodError) {
	var emails []EmailMeta
	st, e := s.consistent(ctx, func(State) (err error) {
		emails, err = s.Store.ListEmails(ctx)
		return err
	})
	return emails, st, e
}

func emailQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a emailQueryArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	q, e := planEmailQuery(a.Filter, a.Sort, a.CollapseThreads)
	if e != nil {
		return nil, e
	}
	emails, st, e := c.s.listEmails(ctx)
	if e != nil {
		return nil, e
	}
	qs := queryState(st.Epoch, st.Email, q.args...)
	return page(q.results(emails), a.Position, a.Anchor, a.AnchorOffset, a.Limit, a.CalculateTotal, a.AccountID, qs, true)
}

// emailQueryChanges treats every Email changed since the query state as
// possibly moved. When the query reads other thread members, every member
// of a touched thread is included, since its matching, order or
// collapsed representative may have changed.
func emailQueryChanges(ctx context.Context, c *call) (any, *MethodError) {
	var a emailQueryChangesArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	q, e := planEmailQuery(a.Filter, a.Sort, a.CollapseThreads)
	if e != nil {
		return nil, e
	}
	emails, st, e := c.s.listEmails(ctx)
	if e != nil {
		return nil, e
	}
	since, ok := parseQueryState(a.SinceQueryState, st, q.args...)
	if !ok {
		return nil, methodErr("cannotCalculateChanges", "unknown query state, or different query arguments")
	}
	changes, err := c.s.Store.Changes(ctx, since, st.Email)
	if err != nil {
		return nil, c.s.fail("read changes", err)
	}
	changed := map[string]bool{}
	touched := map[string]bool{}
	for _, ch := range changes {
		for _, list := range [][]string{ch.Created["Email"], ch.Updated["Email"], ch.Destroyed["Email"]} {
			for _, id := range list {
				changed[id] = true
			}
		}
		for _, list := range [][]string{ch.Created["Thread"], ch.Updated["Thread"], ch.Destroyed["Thread"]} {
			for _, id := range list {
				touched[id] = true
			}
		}
	}
	if q.threadDependent {
		for _, m := range emails {
			if changed[m.ID] {
				touched[m.ThreadID] = true
			}
		}
		for _, m := range emails {
			if touched[m.ThreadID] {
				changed[m.ID] = true
			}
		}
	}
	return queryChangesResult(a.AccountID, a.SinceQueryState, queryState(st.Epoch, st.Email, q.args...), q.results(emails), changed, a.MaxChanges, a.CalculateTotal)
}
