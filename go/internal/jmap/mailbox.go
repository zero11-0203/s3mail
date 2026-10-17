package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

type getArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties *[]string `json:"properties"`
}

type getResponse struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

var mailboxProperties = []string{"id", "name", "parentId", "role", "sortOrder", "totalEmails", "unreadEmails", "totalThreads", "unreadThreads", "myRights", "isSubscribed"}

var countProperties = []string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

// Mailbox limits advertised in the session.
const (
	maxMailboxDepth    = 10
	maxMailboxNameSize = 255
)

// checkProperties validates requested properties; id is always returned.
func checkProperties(req *[]string, known []string) ([]string, *MethodError) {
	if req == nil {
		return known, nil
	}
	out := []string{"id"}
	for _, p := range *req {
		if !slices.Contains(known, p) {
			return nil, methodErr("invalidArguments", "unknown property "+p)
		}
		if p != "id" {
			out = append(out, p)
		}
	}
	return out, nil
}

func pick(full map[string]any, props []string) map[string]any {
	out := make(map[string]any, len(props))
	for _, p := range props {
		out[p] = full[p]
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mailboxObject(m Mailbox, c model.Counts) map[string]any {
	system := m.IsSystem()
	return map[string]any{
		"id": m.ID, "name": m.Name, "parentId": nullable(m.ParentID), "role": nullable(m.Role), "sortOrder": m.SortOrder,
		"totalEmails": c.TotalEmails, "unreadEmails": c.UnreadEmails, "totalThreads": c.TotalThreads, "unreadThreads": c.UnreadThreads,
		"myRights": map[string]bool{
			"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true,
			"maySetSeen": true, "maySetKeywords": true, "mayCreateChild": true,
			"mayRename": true, "mayDelete": !system, "maySubmit": false,
		},
		"isSubscribed": m.IsSubscribed,
	}
}

// mailboxView is one consistent read of the mailbox tree and counters.
type mailboxView struct {
	list   []Mailbox
	byID   map[string]Mailbox
	counts map[string]model.Counts
}

func (s *Server) readMailboxes(ctx context.Context) (mailboxView, error) {
	stored, err := s.Store.ListMailboxes(ctx)
	if err != nil {
		return mailboxView{}, err
	}
	counts, err := s.Store.MailboxCounts(ctx)
	if err != nil {
		return mailboxView{}, err
	}
	v := mailboxView{list: model.Mailboxes(stored), byID: map[string]Mailbox{}, counts: counts}
	for _, m := range v.list {
		v.byID[m.ID] = m
	}
	return v, nil
}

func (v mailboxView) depth(id string) int {
	d := 0
	for id != "" && d <= maxMailboxDepth+1 {
		d++
		id = v.byID[id].ParentID
	}
	return d
}

// isAncestor reports whether a is b or one of b's ancestors.
func (v mailboxView) isAncestor(a, b string) bool {
	for i := 0; b != "" && i <= maxMailboxDepth+1; i++ {
		if a == b {
			return true
		}
		b = v.byID[b].ParentID
	}
	return false
}

func (v mailboxView) hasChildren(id string) bool {
	return slices.ContainsFunc(v.list, func(m Mailbox) bool { return m.ParentID == id })
}

func (s *Server) mailboxExists(ctx context.Context, id string) (bool, error) {
	if (Mailbox{ID: id}).IsSystem() {
		return true, nil
	}
	stored, err := s.Store.ListMailboxes(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(stored, func(m Mailbox) bool { return m.ID == id }), nil
}

func mailboxGet(ctx context.Context, c *call) (any, *MethodError) {
	var a getArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(a.Properties, mailboxProperties)
	if e != nil {
		return nil, e
	}
	if a.IDs != nil && len(*a.IDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	var v mailboxView
	st, e := c.s.consistent(ctx, func(State) (err error) {
		v, err = c.s.readMailboxes(ctx)
		return err
	})
	if e != nil {
		return nil, e
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st, "Mailbox"), List: []map[string]any{}, NotFound: []string{}}
	if a.IDs == nil {
		for _, m := range v.list {
			resp.List = append(resp.List, pick(mailboxObject(m, v.counts[m.ID]), props))
		}
		return resp, nil
	}
	seen := map[string]bool{}
	for _, raw := range *a.IDs {
		id, _ := c.resolveID(raw)
		m, ok := v.byID[id]
		switch {
		case !ok:
			if !slices.Contains(resp.NotFound, raw) {
				resp.NotFound = append(resp.NotFound, raw)
			}
		case !seen[id]:
			seen[id] = true
			resp.List = append(resp.List, pick(mailboxObject(m, v.counts[m.ID]), props))
		}
	}
	return resp, nil
}

// mailboxChanges reports counter-only updates through updatedProperties
// when no other mailbox property changed in the window.
func mailboxChanges(ctx context.Context, c *call) (any, *MethodError) {
	resp, used, e := objectChanges(ctx, c, "Mailbox", func(ch Change) ([]string, []string, []string) {
		return ch.Created["Mailbox"], append(slices.Clone(ch.Updated["Mailbox"]), ch.Updated["MailboxCounts"]...), ch.Destroyed["Mailbox"]
	})
	if e != nil {
		return nil, e
	}
	out := mailboxChangesResponse{changesResponse: resp}
	countsOnly := len(resp.Updated) > 0
	for _, ch := range used {
		if len(ch.Updated["Mailbox"]) > 0 {
			countsOnly = false
		}
	}
	if countsOnly {
		out.UpdatedProperties = countProperties
	}
	return out, nil
}

type comparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
	Collation   string `json:"collation"`
	Keyword     string `json:"keyword"`
}

func (c comparator) ascending() bool { return c.IsAscending == nil || *c.IsAscending }

type queryResponse struct {
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int64    `json:"position"`
	IDs                 []string `json:"ids"`
	Total               *int64   `json:"total,omitempty"`
	Limit               *int64   `json:"limit,omitempty"`
}

type mailboxQueryArgs struct {
	AccountID      string          `json:"accountId"`
	Filter         json.RawMessage `json:"filter"`
	Sort           []comparator    `json:"sort"`
	Position       int64           `json:"position"`
	Anchor         *string         `json:"anchor"`
	AnchorOffset   int64           `json:"anchorOffset"`
	Limit          *int64          `json:"limit"`
	CalculateTotal bool            `json:"calculateTotal"`
	SortAsTree     bool            `json:"sortAsTree"`
	FilterAsTree   bool            `json:"filterAsTree"`
}

type mailboxQueryChangesArgs struct {
	AccountID       string          `json:"accountId"`
	Filter          json.RawMessage `json:"filter"`
	Sort            []comparator    `json:"sort"`
	SinceQueryState string          `json:"sinceQueryState"`
	MaxChanges      *int64          `json:"maxChanges"`
	UpToID          *string         `json:"upToId"`
	CalculateTotal  bool            `json:"calculateTotal"`
	SortAsTree      bool            `json:"sortAsTree"`
	FilterAsTree    bool            `json:"filterAsTree"`
}

type mailboxPredicate = func(Mailbox) bool

func parseMailboxFilter(raw json.RawMessage, depth int) (mailboxPredicate, *MethodError) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return func(Mailbox) bool { return true }, nil
	}
	if depth > 10 {
		return nil, methodErr("unsupportedFilter", "filter nesting too deep")
	}
	if p, ok, e := parseOperator(raw, depth, parseMailboxFilter); ok || e != nil {
		return p, e
	}
	var f struct {
		ParentID     *json.RawMessage `json:"parentId"`
		Name         *string          `json:"name"`
		Role         *json.RawMessage `json:"role"`
		HasAnyRole   *bool            `json:"hasAnyRole"`
		IsSubscribed *bool            `json:"isSubscribed"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, methodErr("unsupportedFilter", err.Error())
	}
	var parent, role *string
	if f.ParentID != nil {
		if err := json.Unmarshal(*f.ParentID, &parent); err != nil {
			return nil, methodErr("invalidArguments", "parentId is a string or null")
		}
	}
	if f.Role != nil {
		if err := json.Unmarshal(*f.Role, &role); err != nil {
			return nil, methodErr("invalidArguments", "role is a string or null")
		}
	}
	return func(m Mailbox) bool {
		switch {
		case f.ParentID != nil && (parent == nil) != (m.ParentID == ""),
			f.ParentID != nil && parent != nil && *parent != m.ParentID,
			f.Role != nil && (role == nil) != (m.Role == ""),
			f.Role != nil && role != nil && *role != m.Role,
			f.HasAnyRole != nil && *f.HasAnyRole != (m.Role != ""),
			f.IsSubscribed != nil && *f.IsSubscribed != m.IsSubscribed,
			f.Name != nil && !strings.Contains(strings.ToLower(m.Name), strings.ToLower(*f.Name)):
			return false
		}
		return true
	}, nil
}

// parseOperator handles a FilterOperator for any object type.
func parseOperator[T any](raw json.RawMessage, depth int, cond func(json.RawMessage, int) (func(T) bool, *MethodError)) (func(T) bool, bool, *MethodError) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false, methodErr("invalidArguments", "filter is not an object")
	}
	if _, ok := probe["operator"]; !ok {
		return nil, false, nil
	}
	var op struct {
		Operator   string            `json:"operator"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &op); err != nil || len(probe) != 2 {
		return nil, true, methodErr("invalidArguments", "malformed filter operator")
	}
	var subs []func(T) bool
	for _, cr := range op.Conditions {
		p, e := cond(cr, depth+1)
		if e != nil {
			return nil, true, e
		}
		subs = append(subs, p)
	}
	switch op.Operator {
	case "AND":
		return func(x T) bool {
			for _, p := range subs {
				if !p(x) {
					return false
				}
			}
			return true
		}, true, nil
	case "OR":
		return func(x T) bool {
			for _, p := range subs {
				if p(x) {
					return true
				}
			}
			return false
		}, true, nil
	case "NOT":
		return func(x T) bool {
			for _, p := range subs {
				if p(x) {
					return false
				}
			}
			return true
		}, true, nil
	}
	return nil, true, methodErr("invalidArguments", "unknown operator "+op.Operator)
}

func mailboxOrder(sorts []comparator) (func(a, b Mailbox) int, *MethodError) {
	var cmps []func(a, b Mailbox) int
	for _, s := range sorts {
		if s.Collation != "" && s.Collation != "i;unicode-casemap" {
			return nil, methodErr("unsupportedSort", "collation "+s.Collation)
		}
		var cmp func(a, b Mailbox) int
		switch s.Property {
		case "sortOrder":
			cmp = func(a, b Mailbox) int { return compareInt(a.SortOrder, b.SortOrder) }
		case "name":
			cmp = func(a, b Mailbox) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) }
		default:
			return nil, methodErr("unsupportedSort", s.Property)
		}
		if !s.ascending() {
			inner := cmp
			cmp = func(a, b Mailbox) int { return -inner(a, b) }
		}
		cmps = append(cmps, cmp)
	}
	// Ties keep the stable default order: system mailboxes, then by id.
	return func(a, b Mailbox) int {
		for _, cmp := range cmps {
			if r := cmp(a, b); r != 0 {
				return r
			}
		}
		return 0
	}, nil
}

// mailboxResults evaluates a Mailbox query (RFC 8621 §2.3).
func mailboxResults(v mailboxView, filter json.RawMessage, sorts []comparator, sortAsTree, filterAsTree bool) ([]string, *MethodError) {
	match, e := parseMailboxFilter(filter, 0)
	if e != nil {
		return nil, e
	}
	order, e := mailboxOrder(sorts)
	if e != nil {
		return nil, e
	}
	included := map[string]bool{}
	for _, m := range v.list {
		included[m.ID] = match(m)
	}
	if filterAsTree {
		for _, m := range v.list {
			for p := m.ParentID; p != "" && included[m.ID]; p = v.byID[p].ParentID {
				if !included[p] {
					included[m.ID] = false
				}
			}
		}
	}
	list := slices.Clone(v.list)
	slices.SortStableFunc(list, order)
	var ids []string
	if sortAsTree {
		var walk func(parent string)
		walk = func(parent string) {
			for _, m := range list {
				if m.ParentID == parent {
					if included[m.ID] {
						ids = append(ids, m.ID)
					}
					walk(m.ID)
				}
			}
		}
		walk("")
		return ids, nil
	}
	for _, m := range list {
		if included[m.ID] {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func mailboxQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a mailboxQueryArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	var v mailboxView
	st, e := c.s.consistent(ctx, func(State) (err error) {
		v, err = c.s.readMailboxes(ctx)
		return err
	})
	if e != nil {
		return nil, e
	}
	ids, e := mailboxResults(v, a.Filter, a.Sort, a.SortAsTree, a.FilterAsTree)
	if e != nil {
		return nil, e
	}
	qs := queryState(st.Epoch, st.Mailbox, compactJSON(a.Filter), a.Sort, a.SortAsTree, a.FilterAsTree)
	return page(ids, a.Position, a.Anchor, a.AnchorOffset, a.Limit, a.CalculateTotal, a.AccountID, qs, true)
}

func mailboxQueryChanges(ctx context.Context, c *call) (any, *MethodError) {
	var a mailboxQueryChangesArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	var v mailboxView
	st, e := c.s.consistent(ctx, func(State) (err error) {
		v, err = c.s.readMailboxes(ctx)
		return err
	})
	if e != nil {
		return nil, e
	}
	args := []any{compactJSON(a.Filter), a.Sort, a.SortAsTree, a.FilterAsTree}
	since, ok := parseQueryState(a.SinceQueryState, st, args...)
	if !ok {
		return nil, methodErr("cannotCalculateChanges", "unknown query state, or different query arguments")
	}
	ids, e := mailboxResults(v, a.Filter, a.Sort, a.SortAsTree, a.FilterAsTree)
	if e != nil {
		return nil, e
	}
	changes, err := c.s.Store.Changes(ctx, since, st.Mailbox)
	if err != nil {
		return nil, c.s.fail("read changes", err)
	}
	changed := map[string]bool{}
	for _, ch := range changes {
		// Counter changes do not affect Mailbox filters or sorts.
		for _, list := range [][]string{ch.Created["Mailbox"], ch.Updated["Mailbox"], ch.Destroyed["Mailbox"]} {
			for _, id := range list {
				changed[id] = true
			}
		}
	}
	if a.SortAsTree || a.FilterAsTree {
		// A changed ancestor can move or hide its descendants.
		for _, m := range v.list {
			for p := m.ParentID; p != ""; p = v.byID[p].ParentID {
				if changed[p] {
					changed[m.ID] = true
					break
				}
			}
		}
	}
	return queryChangesResult(a.AccountID, a.SinceQueryState, queryState(st.Epoch, st.Mailbox, args...), ids, changed, a.MaxChanges, a.CalculateTotal)
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if len(bytes.TrimSpace(raw)) == 0 || json.Compact(&buf, raw) != nil {
		return "null"
	}
	return buf.String()
}

// page applies position/anchor/limit (RFC 8620 §5.5).
func page(ids []string, position int64, anchor *string, anchorOffset int64, limit *int64, calcTotal bool, accountID, queryState string, canCalc bool) (any, *MethodError) {
	n := int64(len(ids))
	start := position
	if anchor != nil {
		idx := int64(slices.Index(ids, *anchor))
		if idx < 0 {
			return nil, methodErr("anchorNotFound", "")
		}
		start = max(idx+anchorOffset, 0)
	} else if start < 0 {
		start = max(n+start, 0)
	}
	start = min(start, n)
	capped := int64(maxQueryLimit)
	var limitOut *int64
	if limit != nil {
		if *limit < 0 {
			return nil, methodErr("invalidArguments", "negative limit")
		}
		if *limit < capped {
			capped = *limit
		} else {
			limitOut = &capped
		}
	}
	end := min(start+capped, n)
	resp := queryResponse{AccountID: accountID, QueryState: queryState, CanCalculateChanges: canCalc, Position: start, IDs: append([]string{}, ids[start:end]...), Limit: limitOut}
	if calcTotal {
		resp.Total = &n
	}
	return resp, nil
}

type setArgs struct {
	AccountID string                                `json:"accountId"`
	IfInState *string                               `json:"ifInState"`
	Create    map[string]json.RawMessage            `json:"create"`
	Update    map[string]map[string]json.RawMessage `json:"update"`
	Destroy   []string                              `json:"destroy"`
	// Mailbox/set: remove (and destroy if orphaned) contained Emails.
	OnDestroyRemoveEmails bool `json:"onDestroyRemoveEmails"`
}

type setResponse struct {
	AccountID    string               `json:"accountId"`
	OldState     string               `json:"oldState"`
	NewState     string               `json:"newState"`
	Created      map[string]any       `json:"created"`
	Updated      map[string]any       `json:"updated"`
	Destroyed    []string             `json:"destroyed"`
	NotCreated   map[string]*SetError `json:"notCreated"`
	NotUpdated   map[string]*SetError `json:"notUpdated"`
	NotDestroyed map[string]*SetError `json:"notDestroyed"`
}

func newSetResponse(accountID, state string) setResponse {
	return setResponse{
		AccountID: accountID, OldState: state, NewState: state,
		Created: map[string]any{}, Updated: map[string]any{}, Destroyed: []string{},
		NotCreated: map[string]*SetError{}, NotUpdated: map[string]*SetError{}, NotDestroyed: map[string]*SetError{},
	}
}

func checkSetSize(a setArgs) *MethodError {
	if len(a.Create)+len(a.Update)+len(a.Destroy) > MaxObjectsInSet {
		return methodErr("requestTooLarge", "")
	}
	return nil
}

func invalidProps(desc string, props ...string) *SetError {
	return &SetError{Type: "invalidProperties", Description: desc, Properties: props}
}

var errBusy = &SetError{Type: "serverFail", Description: "account busy; retry"}

// commitLoop retries one per-object commit from a fresh read while other
// commits win the sequence race.
func (s *Server) commitLoop(ctx context.Context, attempt func(st State) (*model.Mutation, *SetError)) (State, *SetError) {
	for range maxSetAttempts {
		st, err := s.Store.State(ctx)
		if err != nil {
			return State{}, &SetError{Type: "serverFail"}
		}
		m, serr := attempt(st)
		if serr != nil || m == nil {
			return st, serr
		}
		next, err := s.Store.Commit(ctx, st, *m)
		if errors.Is(err, ErrStateChanged) {
			continue
		}
		if errors.Is(err, model.ErrTooLarge) {
			return State{}, &SetError{Type: "tooLarge", Description: "the change exceeds the transaction limits"}
		}
		if err != nil {
			s.Log.Error("jmap", "outcome", "serverFail", "step", "commit", "error", err)
			return State{}, &SetError{Type: "serverFail"}
		}
		return next, nil
	}
	return State{}, errBusy
}

func validMailboxName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxMailboxNameSize {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// mailboxPatch is the settable Mailbox properties (RFC 8621 §2.5).
type mailboxPatch struct {
	Name         *string
	ParentID     *string // "" is null; nil is unchanged
	Role         *string
	SortOrder    *int64
	IsSubscribed *bool
}

func parseMailboxPatch(c *call, obj map[string]json.RawMessage) (mailboxPatch, *SetError) {
	var p mailboxPatch
	for k, raw := range obj {
		switch k {
		case "name":
			var v string
			if json.Unmarshal(raw, &v) != nil {
				return p, invalidProps("name is a string", k)
			}
			p.Name = &v
		case "parentId":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				return p, invalidProps("parentId is a string or null", k)
			}
			id := ""
			if v != nil {
				resolved, ok := c.resolveID(*v)
				if !ok {
					return p, invalidProps("unknown creation id "+*v, k)
				}
				id = resolved
			}
			p.ParentID = &id
		case "role":
			var v *string
			if json.Unmarshal(raw, &v) != nil {
				return p, invalidProps("role is a string or null", k)
			}
			r := ""
			if v != nil {
				r = *v
			}
			p.Role = &r
		case "sortOrder":
			var v int64
			if json.Unmarshal(raw, &v) != nil || v < 0 {
				return p, invalidProps("sortOrder is an unsigned integer", k)
			}
			p.SortOrder = &v
		case "isSubscribed":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return p, invalidProps("isSubscribed is a boolean", k)
			}
			p.IsSubscribed = &v
		case "id", "totalEmails", "unreadEmails", "totalThreads", "unreadThreads", "myRights":
			return p, invalidProps("server-set property", k)
		default:
			return p, invalidProps("unknown property", k)
		}
	}
	return p, nil
}

// checkMailbox validates m as it would exist in v.
func checkMailbox(v mailboxView, m Mailbox) *SetError {
	if !validMailboxName(m.Name) {
		return invalidProps("name must be 1-255 characters without control characters", "name")
	}
	if m.ParentID != "" {
		if _, ok := v.byID[m.ParentID]; !ok {
			return invalidProps("no parent mailbox "+m.ParentID, "parentId")
		}
		if v.isAncestor(m.ID, m.ParentID) {
			return invalidProps("a mailbox cannot be its own ancestor", "parentId")
		}
		if v.depth(m.ParentID)+1 > maxMailboxDepth {
			return invalidProps("mailbox tree is too deep", "parentId")
		}
	}
	for _, o := range v.list {
		if o.ID != m.ID && o.ParentID == m.ParentID && o.Name == m.Name {
			return invalidProps("a sibling already has this name", "name")
		}
	}
	return nil
}

func mailboxSet(ctx context.Context, c *call) (any, *MethodError) {
	var a setArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if e := checkSetSize(a); e != nil {
		return nil, e
	}
	start, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	if a.IfInState != nil && !inState(*a.IfInState, start, "Mailbox") {
		return nil, methodErr("stateMismatch", "")
	}
	resp := newSetResponse(a.AccountID, stateToken(start, "Mailbox"))
	last := start
	advance := func(st State) {
		if st.Seq > last.Seq {
			last = st
		}
	}

	// Creates run in an order that resolves parentId creation references.
	pending := slices.Sorted(func(yield func(string) bool) {
		for k := range a.Create {
			if !yield(k) {
				return
			}
		}
	})
	for progress := true; progress && len(pending) > 0; {
		progress = false
		for _, cid := range slices.Clone(pending) {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(a.Create[cid], &obj); err != nil {
				resp.NotCreated[cid] = invalidProps("not an object")
				pending = slices.DeleteFunc(pending, func(x string) bool { return x == cid })
				progress = true
				continue
			}
			if raw, ok := obj["parentId"]; ok {
				var ref *string
				if json.Unmarshal(raw, &ref) == nil && ref != nil && strings.HasPrefix(*ref, "#") {
					if _, done := c.created[(*ref)[1:]]; !done && slices.Contains(pending, (*ref)[1:]) {
						continue // wait for the parent's creation
					}
				}
			}
			pending = slices.DeleteFunc(pending, func(x string) bool { return x == cid })
			progress = true
			id := model.NewID("B")
			var made Mailbox
			st, serr := c.s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
				p, serr := parseMailboxPatch(c, obj)
				if serr != nil {
					return nil, serr
				}
				if p.Name == nil {
					return nil, invalidProps("name is required", "name")
				}
				if p.Role != nil && *p.Role != "" {
					return nil, invalidProps("roles belong to the system mailboxes", "role")
				}
				m := Mailbox{ID: id, Name: *p.Name, IsSubscribed: true}
				if p.ParentID != nil {
					m.ParentID = *p.ParentID
				}
				if p.SortOrder != nil {
					m.SortOrder = *p.SortOrder
				}
				if p.IsSubscribed != nil {
					m.IsSubscribed = *p.IsSubscribed
				}
				v, err := c.s.readMailboxes(ctx)
				if err != nil {
					return nil, &SetError{Type: "serverFail"}
				}
				if serr := checkMailbox(v, m); serr != nil {
					return nil, serr
				}
				made = m
				return &model.Mutation{Mailboxes: []model.MailboxChange{{After: &m}}}, nil
			})
			if serr != nil {
				resp.NotCreated[cid] = serr
				continue
			}
			advance(st)
			c.created[cid] = id
			resp.Created[cid] = map[string]any{
				"id": id, "totalEmails": 0, "unreadEmails": 0, "totalThreads": 0, "unreadThreads": 0,
				"myRights": mailboxObject(made, model.Counts{})["myRights"], "isSubscribed": made.IsSubscribed, "sortOrder": made.SortOrder,
			}
		}
	}
	for _, cid := range pending {
		resp.NotCreated[cid] = invalidProps("parentId references a creation that cannot be resolved", "parentId")
	}

	for _, rawID := range slices.Sorted(func(yield func(string) bool) {
		for k := range a.Update {
			if !yield(k) {
				return
			}
		}
	}) {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotUpdated[rawID] = &SetError{Type: "notFound"}
			continue
		}
		obj := a.Update[rawID]
		st, serr := c.s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
			p, serr := parseMailboxPatch(c, obj)
			if serr != nil {
				return nil, serr
			}
			v, err := c.s.readMailboxes(ctx)
			if err != nil {
				return nil, &SetError{Type: "serverFail"}
			}
			before, ok := v.byID[id]
			if !ok {
				return nil, &SetError{Type: "notFound"}
			}
			after := before
			if p.Name != nil {
				after.Name = *p.Name
			}
			if p.ParentID != nil {
				if before.IsSystem() && *p.ParentID != before.ParentID {
					return nil, &SetError{Type: "forbidden", Description: "system mailboxes stay at the top level", Properties: []string{"parentId"}}
				}
				after.ParentID = *p.ParentID
			}
			if p.Role != nil && *p.Role != before.Role {
				return nil, invalidProps("mailbox roles are fixed", "role")
			}
			if p.SortOrder != nil {
				after.SortOrder = *p.SortOrder
			}
			if p.IsSubscribed != nil {
				after.IsSubscribed = *p.IsSubscribed
			}
			if after == before {
				return nil, nil
			}
			if serr := checkMailbox(v, after); serr != nil {
				return nil, serr
			}
			return &model.Mutation{Mailboxes: []model.MailboxChange{{Before: &before, After: &after}}}, nil
		})
		if serr != nil {
			resp.NotUpdated[rawID] = serr
			continue
		}
		advance(st)
		resp.Updated[rawID] = nil
	}

	for _, rawID := range a.Destroy {
		id, ok := c.resolveID(rawID)
		if !ok {
			resp.NotDestroyed[rawID] = &SetError{Type: "notFound"}
			continue
		}
		st, serr := c.s.destroyMailbox(ctx, id, a.OnDestroyRemoveEmails)
		if serr != nil {
			resp.NotDestroyed[rawID] = serr
			continue
		}
		advance(st)
		resp.Destroyed = append(resp.Destroyed, rawID)
	}
	resp.NewState = stateToken(last, "Mailbox")
	return resp, nil
}

// emptyBudget bounds how long one Mailbox/set spends emptying a mailbox.
const emptyBudget = 15 * time.Second

// destroyMailbox empties the mailbox in per-Email commits when asked,
// then deletes it with a final guarded commit
// (docs/specs/storage-sync.md#dynamodb-storage-and-consistency-contract).
func (s *Server) destroyMailbox(ctx context.Context, id string, removeEmails bool) (State, *SetError) {
	deadline := time.Now().Add(emptyBudget)
	for {
		var target *Mailbox
		var contained []EmailMeta
		st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
			v, err := s.readMailboxes(ctx)
			if err != nil {
				return nil, &SetError{Type: "serverFail"}
			}
			m, ok := v.byID[id]
			switch {
			case !ok:
				return nil, &SetError{Type: "notFound"}
			case m.IsSystem():
				return nil, &SetError{Type: "forbidden", Description: "system mailboxes cannot be destroyed"}
			case v.hasChildren(id):
				return nil, &SetError{Type: "mailboxHasChild"}
			}
			all, err := s.Store.ListEmails(ctx)
			if err != nil {
				return nil, &SetError{Type: "serverFail"}
			}
			contained = slices.DeleteFunc(all, func(e EmailMeta) bool { return !e.MailboxIDs[id] })
			if len(contained) > 0 {
				if !removeEmails {
					return nil, &SetError{Type: "mailboxHasEmail"}
				}
				return nil, nil // empty it first
			}
			target = &m
			return &model.Mutation{Mailboxes: []model.MailboxChange{{Before: &m}}}, nil
		})
		if serr != nil {
			return State{}, serr
		}
		if target != nil {
			return st, nil
		}
		for _, e := range contained {
			if time.Now().After(deadline) {
				return State{}, &SetError{Type: "mailboxHasEmail", Description: "emptying did not finish; repeat the request to continue"}
			}
			if _, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
				found, err := s.Store.GetEmails(ctx, []string{e.ID})
				if err != nil {
					return nil, &SetError{Type: "serverFail"}
				}
				cur, ok := found[e.ID]
				if !ok || !cur.MailboxIDs[id] {
					return nil, nil
				}
				if len(cur.MailboxIDs) == 1 {
					return &model.Mutation{Emails: []model.EmailChange{{Before: &cur}}}, nil
				}
				after := cur.Clone()
				delete(after.MailboxIDs, id)
				return &model.Mutation{Emails: []model.EmailChange{{Before: &cur, After: &after}}}, nil
			}); serr != nil {
				return State{}, serr
			}
		}
	}
}
