package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type changesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	MaxChanges *int64 `json:"maxChanges"`
}

type changesResponse struct {
	AccountID      string   `json:"accountId"`
	OldState       string   `json:"oldState"`
	NewState       string   `json:"newState"`
	HasMoreChanges bool     `json:"hasMoreChanges"`
	Created        []string `json:"created"`
	Updated        []string `json:"updated"`
	Destroyed      []string `json:"destroyed"`
}

// mailboxChangesResponse adds Mailbox/changes' updatedProperties.
type mailboxChangesResponse struct {
	changesResponse
	UpdatedProperties []string `json:"updatedProperties"`
}

// objectChanges implements /changes over CHANGE records (RFC 8620 §5.2):
// ids are coalesced relative to sinceState and a page never ends inside a
// commit. It also returns the change records the response covers.
func objectChanges(ctx context.Context, c *call, typ string, ids func(Change) (created, updated, destroyed []string)) (changesResponse, []Change, *MethodError) {
	var a changesArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return changesResponse{}, nil, e
	}
	if a.MaxChanges != nil && *a.MaxChanges <= 0 {
		return changesResponse{}, nil, methodErr("invalidArguments", "maxChanges must be positive")
	}
	st, err := c.s.Store.State(ctx)
	if err != nil {
		return changesResponse{}, nil, c.s.fail("read state", err)
	}
	c.s.epoch = st.Epoch
	since, ok := parseState(a.SinceState, st)
	if !ok {
		return changesResponse{}, nil, methodErr("cannotCalculateChanges", "unknown or expired state")
	}
	changes, err := c.s.Store.Changes(ctx, since, st.Of(typ))
	if err != nil {
		return changesResponse{}, nil, c.s.fail("read changes", err)
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
	used := 0
	for _, ch := range changes {
		cr, up, de := ids(ch)
		next := make(map[string]int, len(status))
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
				return changesResponse{}, nil, methodErr("cannotCalculateChanges", "one commit holds more than maxChanges changes")
			}
			more = true
			break
		}
		status, order, newSeq = next, append(order, added...), ch.Seq
		used++
	}
	resp := changesResponse{
		AccountID: a.AccountID, OldState: a.SinceState, NewState: stateToken(st, typ), HasMoreChanges: more,
		Created: []string{}, Updated: []string{}, Destroyed: []string{},
	}
	if more {
		resp.NewState = seqToken(st.Epoch, newSeq)
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
	return resp, changes[:used], nil
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

// Query states: "q<epoch>-<seq>-<hash>", where hash covers the normalized
// query arguments, so different queries never share a state.
func queryState(epoch, seq int64, args ...any) string {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("q%d-%d-%s", epoch, seq, hex.EncodeToString(sum[:6]))
}

func parseQueryState(tok string, cur State, args ...any) (int64, bool) {
	rest, ok := strings.CutPrefix(tok, "q")
	if !ok {
		return 0, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) != 3 {
		return 0, false
	}
	epoch, err1 := strconv.ParseInt(parts[0], 10, 64)
	seq, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || epoch != cur.Epoch || seq < 0 || seq > cur.Seq {
		return 0, false
	}
	return seq, queryState(epoch, seq, args...) == tok
}

type addedItem struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}

type queryChangesResponse struct {
	AccountID     string      `json:"accountId"`
	OldQueryState string      `json:"oldQueryState"`
	NewQueryState string      `json:"newQueryState"`
	Total         *int64      `json:"total,omitempty"`
	Removed       []string    `json:"removed"`
	Added         []addedItem `json:"added"`
}

// queryChangesResult reports every possibly-changed id as removed and
// re-adds the ones in the current results at their index. RFC 8620 §5.6
// allows extra removed ids, and requires re-adding every current result
// whose filter or sort input may have changed; unchanged objects keep
// their relative order because every input they are evaluated on is
// included in changed.
func queryChangesResult(accountID, oldQS, newQS string, current []string, changed map[string]bool, maxChanges *int64, calcTotal bool) (any, *MethodError) {
	resp := queryChangesResponse{AccountID: accountID, OldQueryState: oldQS, NewQueryState: newQS, Removed: []string{}, Added: []addedItem{}}
	for id := range changed {
		resp.Removed = append(resp.Removed, id)
	}
	slices.Sort(resp.Removed)
	for i, id := range current {
		if changed[id] {
			resp.Added = append(resp.Added, addedItem{ID: id, Index: i})
		}
	}
	if maxChanges != nil {
		if *maxChanges <= 0 {
			return nil, methodErr("invalidArguments", "maxChanges must be positive")
		}
		if int64(len(resp.Removed)+len(resp.Added)) > *maxChanges {
			return nil, methodErr("tooManyChanges", "")
		}
	}
	if calcTotal {
		n := int64(len(current))
		resp.Total = &n
	}
	return resp, nil
}
