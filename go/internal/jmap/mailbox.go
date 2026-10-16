package jmap

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

type getArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties *[]string `json:"properties"`
}

type changesArgs struct {
	AccountID  string `json:"accountId"`
	SinceState string `json:"sinceState"`
	MaxChanges *int64 `json:"maxChanges"`
}

type changesResponse struct {
	AccountID         string   `json:"accountId"`
	OldState          string   `json:"oldState"`
	NewState          string   `json:"newState"`
	HasMoreChanges    bool     `json:"hasMoreChanges"`
	Created           []string `json:"created"`
	Updated           []string `json:"updated"`
	Destroyed         []string `json:"destroyed"`
	UpdatedProperties []string `json:"updatedProperties,omitempty"`
}

type getResponse struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

var mailboxProperties = []string{"id", "name", "parentId", "role", "sortOrder", "totalEmails", "unreadEmails", "totalThreads", "unreadThreads", "myRights", "isSubscribed"}

var countProperties = []string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

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

func mailboxObject(m mailboxDef, emails []EmailMeta) map[string]any {
	var total, unread int
	for _, e := range emails {
		if e.MailboxIDs[m.ID] {
			total++
			if !e.Keywords["$seen"] {
				unread++
			}
		}
	}
	var role any
	if m.Role != "" {
		role = m.Role
	}
	return map[string]any{
		"id": m.ID, "name": m.Name, "parentId": nil, "role": role, "sortOrder": m.SortOrder,
		// Each Email is its own thread until threading is implemented, so
		// thread counts equal email counts.
		"totalEmails": total, "unreadEmails": unread, "totalThreads": total, "unreadThreads": unread,
		"myRights": map[string]bool{
			"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true,
			"maySetSeen": true, "maySetKeywords": true, "mayCreateChild": false,
			"mayRename": false, "mayDelete": false, "maySubmit": false,
		},
		"isSubscribed": true,
	}
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
	var emails []EmailMeta
	st, e := c.s.consistent(ctx, func(State) (err error) {
		emails, err = c.s.Store.ListEmails(ctx)
		return err
	})
	if e != nil {
		return nil, e
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st), List: []map[string]any{}, NotFound: []string{}}
	want := func(id string) bool { return a.IDs == nil || slices.Contains(*a.IDs, id) }
	for _, m := range mailboxes {
		if want(m.ID) {
			resp.List = append(resp.List, pick(mailboxObject(m, emails), props))
		}
	}
	if a.IDs != nil {
		if len(*a.IDs) > MaxObjectsInGet {
			return nil, methodErr("requestTooLarge", "")
		}
		for _, id := range *a.IDs {
			if !mailboxExists(id) && !slices.Contains(resp.NotFound, id) {
				resp.NotFound = append(resp.NotFound, id)
			}
		}
	}
	return resp, nil
}

// mailboxChanges reports count updates only: the mailbox set itself is
// fixed, and every Email change can change counts.
func mailboxChanges(ctx context.Context, c *call) (any, *MethodError) {
	var a changesArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	st, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	since, ok := parseState(a.SinceState, st)
	if !ok {
		return nil, methodErr("cannotCalculateChanges", "unknown or expired state")
	}
	resp := changesResponse{AccountID: a.AccountID, OldState: a.SinceState, NewState: stateToken(st), Created: []string{}, Updated: []string{}, Destroyed: []string{}}
	if since == st.Seq {
		return resp, nil
	}
	if a.MaxChanges != nil && *a.MaxChanges < int64(len(mailboxes)) {
		return nil, methodErr("cannotCalculateChanges", "maxChanges is smaller than the mailbox count")
	}
	for _, m := range mailboxes {
		resp.Updated = append(resp.Updated, m.ID)
	}
	resp.UpdatedProperties = countProperties
	return resp, nil
}

type mailboxQueryArgs struct {
	AccountID      string            `json:"accountId"`
	Filter         json.RawMessage   `json:"filter"`
	Sort           []comparator      `json:"sort"`
	Position       int64             `json:"position"`
	Anchor         *string           `json:"anchor"`
	AnchorOffset   int64             `json:"anchorOffset"`
	Limit          *int64            `json:"limit"`
	CalculateTotal bool              `json:"calculateTotal"`
	SortAsTree     bool              `json:"sortAsTree"`
	FilterAsTree   bool              `json:"filterAsTree"`
	Extra          map[string]string `json:"-"`
}

type comparator struct {
	Property    string `json:"property"`
	IsAscending *bool  `json:"isAscending"`
	Collation   string `json:"collation"`
	Keyword     string `json:"keyword"`
}

type queryResponse struct {
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
	Position            int64    `json:"position"`
	IDs                 []string `json:"ids"`
	Total               *int64   `json:"total,omitempty"`
	Limit               *int64   `json:"limit,omitempty"`
}

func mailboxQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a mailboxQueryArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	match := func(mailboxDef) bool { return true }
	if len(a.Filter) > 0 && string(a.Filter) != "null" {
		var f struct {
			Role       *json.RawMessage `json:"role"`
			HasAnyRole *bool            `json:"hasAnyRole"`
			Name       *string          `json:"name"`
			ParentID   *json.RawMessage `json:"parentId"`
		}
		dec := json.NewDecoder(strings.NewReader(string(a.Filter)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, methodErr("unsupportedFilter", err.Error())
		}
		match = func(m mailboxDef) bool {
			if f.Role != nil {
				var role *string
				_ = json.Unmarshal(*f.Role, &role)
				if (role == nil) != (m.Role == "") || (role != nil && *role != m.Role) {
					return false
				}
			}
			if f.HasAnyRole != nil && *f.HasAnyRole != (m.Role != "") {
				return false
			}
			if f.Name != nil && !strings.Contains(strings.ToLower(m.Name), strings.ToLower(*f.Name)) {
				return false
			}
			if f.ParentID != nil && string(*f.ParentID) != "null" {
				return false
			}
			return true
		}
	}
	for _, s := range a.Sort {
		if s.Property != "sortOrder" && s.Property != "name" {
			return nil, methodErr("unsupportedSort", s.Property)
		}
	}
	st, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	var ids []string
	for _, m := range mailboxes {
		if match(m) {
			ids = append(ids, m.ID)
		}
	}
	if len(a.Sort) > 0 && a.Sort[0].Property == "name" {
		asc := a.Sort[0].IsAscending == nil || *a.Sort[0].IsAscending
		names := map[string]string{}
		for _, m := range mailboxes {
			names[m.ID] = strings.ToLower(m.Name)
		}
		slices.SortStableFunc(ids, func(x, y string) int {
			if asc {
				return strings.Compare(names[x], names[y])
			}
			return strings.Compare(names[y], names[x])
		})
	}
	return page(ids, a.Position, a.Anchor, a.AnchorOffset, a.Limit, a.CalculateTotal, a.AccountID, "q"+stateToken(st))
}

// page applies position/anchor/limit (RFC 8620 §5.5).
func page(ids []string, position int64, anchor *string, anchorOffset int64, limit *int64, calcTotal bool, accountID, queryState string) (any, *MethodError) {
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
	resp := queryResponse{AccountID: accountID, QueryState: queryState, Position: start, IDs: append([]string{}, ids[start:end]...), Limit: limitOut}
	if calcTotal {
		resp.Total = &n
	}
	return resp, nil
}

func queryChangesUnsupported(_ context.Context, c *call) (any, *MethodError) {
	var a struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(c.args, &a); err != nil {
		return nil, methodErr("invalidArguments", err.Error())
	}
	if a.AccountID != c.s.publicID(c.acct) {
		return nil, methodErr("accountNotFound", "")
	}
	// Every query reports canCalculateChanges: false.
	return nil, methodErr("cannotCalculateChanges", "query changes are not supported yet; rerun the query")
}

type setArgs struct {
	AccountID string                                `json:"accountId"`
	IfInState *string                               `json:"ifInState"`
	Create    map[string]json.RawMessage            `json:"create"`
	Update    map[string]map[string]json.RawMessage `json:"update"`
	Destroy   []string                              `json:"destroy"`
	// Email/set and Mailbox/set extras.
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

func newSetResponse(accountID string, st State) setResponse {
	return setResponse{
		AccountID: accountID, OldState: stateToken(st), NewState: stateToken(st),
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

// mailboxSet rejects every change: the mailbox set is fixed for now.
func mailboxSet(ctx context.Context, c *call) (any, *MethodError) {
	var a setArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if e := checkSetSize(a); e != nil {
		return nil, e
	}
	st, err := c.s.Store.State(ctx)
	if err != nil {
		return nil, c.s.fail("read state", err)
	}
	if a.IfInState != nil && *a.IfInState != stateToken(st) {
		return nil, methodErr("stateMismatch", "")
	}
	resp := newSetResponse(a.AccountID, st)
	denied := &SetError{Type: "forbidden", Description: "the mailbox set is fixed in this release"}
	for id := range a.Create {
		resp.NotCreated[id] = denied
	}
	for id := range a.Update {
		resp.NotUpdated[id] = denied
	}
	for _, id := range a.Destroy {
		resp.NotDestroyed[id] = denied
	}
	return resp, nil
}

func threadGet(ctx context.Context, c *call) (any, *MethodError) {
	var a getArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(a.Properties, []string{"id", "emailIds"})
	if e != nil {
		return nil, e
	}
	if a.IDs == nil {
		return nil, methodErr("requestTooLarge", "Thread/get requires ids")
	}
	if len(*a.IDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	var emailIDs []string
	for _, tid := range *a.IDs {
		if eid, ok := emailIDOfThread(tid); ok {
			emailIDs = append(emailIDs, eid)
		}
	}
	var found map[string]EmailMeta
	st, e := c.s.consistent(ctx, func(State) (err error) {
		found, err = c.s.Store.GetEmails(ctx, emailIDs)
		return err
	})
	if e != nil {
		return nil, e
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st), List: []map[string]any{}, NotFound: []string{}}
	for _, tid := range *a.IDs {
		eid, ok := emailIDOfThread(tid)
		if _, exists := found[eid]; !ok || !exists {
			resp.NotFound = append(resp.NotFound, tid)
			continue
		}
		resp.List = append(resp.List, pick(map[string]any{"id": tid, "emailIds": []string{eid}}, props))
	}
	return resp, nil
}

// threadChanges: threads are created with their Email and never change
// membership, so only Email creations matter.
func threadChanges(ctx context.Context, c *call) (any, *MethodError) {
	return objectChanges(ctx, c, func(ch Change) (created, updated, destroyed []string) {
		for _, id := range ch.Created["Email"] {
			created = append(created, threadIDOf(id))
		}
		for _, id := range ch.Destroyed["Email"] {
			destroyed = append(destroyed, threadIDOf(id))
		}
		return
	})
}
