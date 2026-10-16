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

// objectChanges implements /changes over CHANGE records (RFC 8620 §5.2):
// ids are coalesced relative to sinceState and a page never ends inside
// a commit.
func objectChanges(ctx context.Context, c *call, ids func(Change) (created, updated, destroyed []string)) (any, *MethodError) {
	var a changesArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if a.MaxChanges != nil && *a.MaxChanges <= 0 {
		return nil, methodErr("invalidArguments", "maxChanges must be positive")
	}
	st, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	since, ok := parseState(a.SinceState, st)
	if !ok {
		return nil, methodErr("cannotCalculateChanges", "unknown or expired state")
	}
	changes, err := c.s.Store.Changes(ctx, since, st.Seq)
	if err != nil {
		return nil, c.s.fail("read changes", err)
	}
	const (
		created = iota + 1
		updated
		destroyed
	)
	status := map[string]int{}
	var order []string
	newSeq := since
	more := false
	for _, ch := range changes {
		cr, up, de := ids(ch)
		next := map[string]int{}
		for k, v := range status {
			next[k] = v
		}
		var added []string
		// Transitions relative to sinceState; -1 marks an id created and
		// destroyed inside the window, which is not reported at all.
		apply := func(id string, kind int) {
			prev, seen := next[id]
			if !seen {
				added = append(added, id)
			}
			switch kind {
			case created:
				if prev == 0 || prev == -1 {
					next[id] = created
				}
			case updated:
				if prev == 0 {
					next[id] = updated
				}
			case destroyed:
				if prev == created {
					next[id] = -1
				} else if prev != -1 {
					next[id] = destroyed
				}
			}
		}
		for _, id := range cr {
			apply(id, created)
		}
		for _, id := range up {
			apply(id, updated)
		}
		for _, id := range de {
			apply(id, destroyed)
		}
		if a.MaxChanges != nil && int64(countLive(next)) > *a.MaxChanges {
			if newSeq == since {
				return nil, methodErr("cannotCalculateChanges", "one commit holds more than maxChanges changes")
			}
			more = true
			break
		}
		status, order, newSeq = next, append(order, added...), ch.Seq
	}
	if !more {
		newSeq = st.Seq
	}
	resp := changesResponse{
		AccountID: a.AccountID, OldState: a.SinceState,
		NewState: stateToken(State{Seq: newSeq, Epoch: st.Epoch}), HasMoreChanges: more,
		Created: []string{}, Updated: []string{}, Destroyed: []string{},
	}
	for _, id := range order {
		switch status[id] {
		case created:
			resp.Created = append(resp.Created, id)
		case updated:
			resp.Updated = append(resp.Updated, id)
		case destroyed:
			resp.Destroyed = append(resp.Destroyed, id)
		}
	}
	return resp, nil
}

func countLive(m map[string]int) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

func emailChanges(ctx context.Context, c *call) (any, *MethodError) {
	return objectChanges(ctx, c, func(ch Change) ([]string, []string, []string) {
		return ch.Created["Email"], ch.Updated["Email"], ch.Destroyed["Email"]
	})
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

type predicate func(EmailMeta) bool

// parseFilter builds a predicate from a FilterOperator/FilterCondition
// tree. Text and body search wait for the search index (docs/specs/search.md).
func parseFilter(raw json.RawMessage, depth int) (predicate, *MethodError) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return func(EmailMeta) bool { return true }, nil
	}
	if depth > 10 {
		return nil, methodErr("unsupportedFilter", "filter nesting too deep")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, methodErr("invalidArguments", "filter is not an object")
	}
	if _, ok := probe["operator"]; ok {
		var op struct {
			Operator   string            `json:"operator"`
			Conditions []json.RawMessage `json:"conditions"`
		}
		if err := json.Unmarshal(raw, &op); err != nil || len(probe) != 2 {
			return nil, methodErr("invalidArguments", "malformed filter operator")
		}
		var subs []predicate
		for _, cr := range op.Conditions {
			p, e := parseFilter(cr, depth+1)
			if e != nil {
				return nil, e
			}
			subs = append(subs, p)
		}
		switch op.Operator {
		case "AND":
			return func(m EmailMeta) bool {
				for _, p := range subs {
					if !p(m) {
						return false
					}
				}
				return true
			}, nil
		case "OR":
			return func(m EmailMeta) bool {
				for _, p := range subs {
					if p(m) {
						return true
					}
				}
				return false
			}, nil
		case "NOT":
			return func(m EmailMeta) bool {
				for _, p := range subs {
					if p(m) {
						return false
					}
				}
				return true
			}, nil
		}
		return nil, methodErr("invalidArguments", "unknown operator "+op.Operator)
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
	return func(m EmailMeta) bool {
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
		// Every thread has one Email, so the thread keyword conditions
		// reduce to the Email's own keywords.
		case f.AllInThreadHaveKeyword != nil && !m.Keywords[kw(f.AllInThreadHaveKeyword)],
			f.SomeInThreadHaveKeyword != nil && !m.Keywords[kw(f.SomeInThreadHaveKeyword)],
			f.NoneInThreadHaveKeyword != nil && m.Keywords[kw(f.NoneInThreadHaveKeyword)],
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

func sortFunc(sorts []comparator) (func(a, b EmailMeta) int, *MethodError) {
	if len(sorts) == 0 {
		sorts = []comparator{{Property: "receivedAt", IsAscending: new(bool)}}
	}
	var cmps []func(a, b EmailMeta) int
	for _, s := range sorts {
		if s.Collation != "" && s.Collation != "i;unicode-casemap" {
			return nil, methodErr("unsupportedSort", "collation "+s.Collation)
		}
		var cmp func(a, b EmailMeta) int
		switch s.Property {
		case "receivedAt":
			cmp = func(a, b EmailMeta) int { return a.ReceivedAt.Compare(b.ReceivedAt) }
		case "sentAt":
			cmp = func(a, b EmailMeta) int { return a.SentAt.Compare(b.SentAt) }
		case "size":
			cmp = func(a, b EmailMeta) int { return compareInt(a.Size, b.Size) }
		case "from":
			cmp = func(a, b EmailMeta) int { return strings.Compare(firstAddress(a.From), firstAddress(b.From)) }
		case "to":
			cmp = func(a, b EmailMeta) int { return strings.Compare(firstAddress(a.To), firstAddress(b.To)) }
		case "subject":
			cmp = func(a, b EmailMeta) int {
				return strings.Compare(strings.ToLower(a.Subject), strings.ToLower(b.Subject))
			}
		case "hasKeyword", "allInThreadHaveKeyword", "someInThreadHaveKeyword":
			if s.Keyword == "" {
				return nil, methodErr("invalidArguments", s.Property+" needs a keyword")
			}
			k := strings.ToLower(s.Keyword)
			cmp = func(a, b EmailMeta) int { return compareBool(a.Keywords[k], b.Keywords[k]) }
		default:
			return nil, methodErr("unsupportedSort", s.Property)
		}
		if s.IsAscending != nil && !*s.IsAscending {
			inner := cmp
			cmp = func(a, b EmailMeta) int { return -inner(a, b) }
		}
		cmps = append(cmps, cmp)
	}
	return func(a, b EmailMeta) int {
		for _, cmp := range cmps {
			if r := cmp(a, b); r != 0 {
				return r
			}
		}
		return strings.Compare(a.ID, b.ID)
	}, nil
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

func emailQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a emailQueryArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	match, e := parseFilter(a.Filter, 0)
	if e != nil {
		return nil, e
	}
	order, e := sortFunc(a.Sort)
	if e != nil {
		return nil, e
	}
	var emails []EmailMeta
	st, e := c.s.consistent(ctx, func(State) (err error) {
		emails, err = c.s.Store.ListEmails(ctx)
		return err
	})
	if e != nil {
		return nil, e
	}
	var hits []EmailMeta
	for _, m := range emails {
		if match(m) {
			hits = append(hits, m)
		}
	}
	slices.SortStableFunc(hits, order)
	ids := make([]string, len(hits))
	for i, m := range hits {
		ids[i] = m.ID
	}
	// collapseThreads changes nothing while every thread holds one Email.
	return page(ids, a.Position, a.Anchor, a.AnchorOffset, a.Limit, a.CalculateTotal, a.AccountID, "q"+stateToken(st))
}
