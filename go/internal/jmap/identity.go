package jmap

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/send"
)

// Sending enables urn:ietf:params:jmap:submission. Identities exist for
// exactly the declared sending aliases
// (docs/specs/mail-flow.md#alias-policy).
type Sending struct {
	Domain  string
	Aliases []string // base local parts, lowercase; never the canary
	Policy  send.Policy
}

func (sd *Sending) addresses() []string {
	var out []string
	for _, a := range sd.Aliases {
		out = append(out, a+"@"+sd.Domain)
	}
	return out
}

var identityProperties = []string{"id", "name", "email", "replyTo", "bcc", "textSignature", "htmlSignature", "mayDelete"}

// Identity property limits.
const (
	maxIdentityName      = 256
	maxIdentitySignature = 64 << 10
	maxIdentityAddresses = 16
)

func addressList(list []model.Address) any {
	if list == nil {
		return nil
	}
	return list
}

func identityObject(x model.Identity) map[string]any {
	return map[string]any{
		"id": x.ID, "name": x.Name, "email": x.Email, "replyTo": addressList(x.ReplyTo), "bcc": addressList(x.Bcc),
		"textSignature": x.TextSignature, "htmlSignature": x.HTMLSignature,
		// Identities follow the alias registry; removing one is an
		// infrastructure change.
		"mayDelete": false,
	}
}

// identities reconciles the stored identities with the alias registry and
// returns them by id. The commit is guarded like any other, so concurrent
// requests converge on one result.
func (s *Server) identities(ctx context.Context) (map[string]model.Identity, State, *MethodError) {
	var out map[string]model.Identity
	st, serr := s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
		stored, err := s.Store.ListIdentities(ctx)
		if err != nil {
			return nil, &SetError{Type: "serverFail"}
		}
		out = map[string]model.Identity{}
		for _, x := range stored {
			out[x.ID] = x
		}
		want := map[string]string{}
		for _, addr := range s.Sending.addresses() {
			want[model.IdentityID(addr)] = addr
		}
		var m model.Mutation
		for _, id := range slices.Sorted(maps.Keys(want)) {
			if _, ok := out[id]; !ok {
				x := model.Identity{ID: id, Email: want[id]}
				m.Identities = append(m.Identities, model.IdentityChange{After: &x})
				out[id] = x
			}
		}
		for _, id := range slices.Sorted(maps.Keys(out)) {
			if _, ok := want[id]; !ok {
				x := out[id]
				m.Identities = append(m.Identities, model.IdentityChange{Before: &x})
				delete(out, id)
			}
		}
		if len(m.Identities) == 0 {
			return nil, nil
		}
		return &m, nil
	})
	if serr != nil {
		return nil, State{}, methodErr("serverFail", "synchronize identities")
	}
	s.epoch = st.Epoch
	return out, st, nil
}

func identityGet(ctx context.Context, c *call) (any, *MethodError) {
	var a getArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	props, e := checkProperties(a.Properties, identityProperties)
	if e != nil {
		return nil, e
	}
	if a.IDs != nil && len(*a.IDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	all, st, e := c.s.identities(ctx)
	if e != nil {
		return nil, e
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st, "Identity"), List: []map[string]any{}, NotFound: []string{}}
	ids := slices.Sorted(maps.Keys(all))
	if a.IDs != nil {
		ids = nil
		for _, raw := range *a.IDs {
			id, _ := c.resolveID(raw)
			if _, ok := all[id]; !ok {
				if !slices.Contains(resp.NotFound, raw) {
					resp.NotFound = append(resp.NotFound, raw)
				}
			} else if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		resp.List = append(resp.List, pick(identityObject(all[id]), props))
	}
	return resp, nil
}

func identityChanges(ctx context.Context, c *call) (any, *MethodError) {
	if _, _, e := c.s.identities(ctx); e != nil {
		return nil, e
	}
	resp, _, e := objectChanges(ctx, c, "Identity", func(ch Change) ([]string, []string, []string) {
		return ch.Created["Identity"], ch.Updated["Identity"], ch.Destroyed["Identity"]
	})
	if e != nil {
		return nil, e
	}
	return resp, nil
}

func identitySet(ctx context.Context, c *call) (any, *MethodError) {
	var a setArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if e := checkSetSize(a); e != nil {
		return nil, e
	}
	_, start, e := c.s.identities(ctx)
	if e != nil {
		return nil, e
	}
	ctx, g, e := guardState(ctx, a.IfInState, start, "Identity")
	if e != nil {
		return nil, e
	}
	resp := newSetResponse(a.AccountID, stateToken(start, "Identity"))
	last := start
	for cid := range a.Create {
		resp.NotCreated[cid] = &SetError{Type: "forbidden", Description: "identities follow the declared aliases; add an alias to create one"}
	}
	for _, rawID := range slices.Sorted(maps.Keys(a.Update)) {
		id, _ := c.resolveID(rawID)
		patch := a.Update[rawID]
		st, serr := c.s.commitLoop(ctx, func(State) (*model.Mutation, *SetError) {
			stored, err := c.s.Store.ListIdentities(ctx)
			if err != nil {
				return nil, &SetError{Type: "serverFail"}
			}
			i := slices.IndexFunc(stored, func(x model.Identity) bool { return x.ID == id })
			if i < 0 {
				return nil, &SetError{Type: "notFound"}
			}
			before := stored[i]
			after, serr := patchIdentity(before, patch)
			if serr != nil {
				return nil, serr
			}
			return &model.Mutation{Identities: []model.IdentityChange{{Before: &before, After: &after}}}, nil
		})
		if serr != nil {
			resp.NotUpdated[rawID] = serr
			continue
		}
		if st.Seq > last.Seq {
			last = st
		}
		resp.Updated[id] = nil
	}
	for _, rawID := range a.Destroy {
		resp.NotDestroyed[rawID] = &SetError{Type: "forbidden", Description: "identities follow the declared aliases; remove the alias to remove one"}
	}
	if g.aborted() {
		return nil, methodErr("stateMismatch", "")
	}
	resp.NewState = stateToken(last, "Identity")
	return resp, nil
}

func patchIdentity(x model.Identity, patch map[string]json.RawMessage) (model.Identity, *SetError) {
	for path, raw := range patch {
		var err error
		switch path {
		case "name":
			err = json.Unmarshal(raw, &x.Name)
			if err == nil && (len(x.Name) > maxIdentityName || strings.ContainsAny(x.Name, "\r\n")) {
				return x, invalidProps("name is one line of at most 256 bytes", path)
			}
		case "textSignature", "htmlSignature":
			var v string
			if err = json.Unmarshal(raw, &v); err == nil && len(v) > maxIdentitySignature {
				return x, invalidProps(path+" is at most 64 KiB", path)
			}
			if path == "textSignature" {
				x.TextSignature = v
			} else {
				x.HTMLSignature = v
			}
		case "replyTo", "bcc":
			var list []model.Address
			if err = json.Unmarshal(raw, &list); err == nil {
				if serr := checkAddresses(path, list); serr != nil {
					return x, serr
				}
			}
			if path == "replyTo" {
				x.ReplyTo = list
			} else {
				x.Bcc = list
			}
		default:
			return x, invalidProps("property is immutable or unknown", strings.SplitN(path, "/", 2)[0])
		}
		if err != nil {
			return x, invalidProps("wrong type", path)
		}
	}
	return x, nil
}

func checkAddresses(prop string, list []model.Address) *SetError {
	if len(list) > maxIdentityAddresses {
		return invalidProps("too many addresses", prop)
	}
	for _, a := range list {
		if !validAddress(a.Email) || len(a.Name) > maxIdentityName || strings.ContainsAny(a.Name, "\r\n") {
			return invalidProps("invalid address", prop)
		}
	}
	return nil
}

// validAddress accepts a bare addr-spec without whitespace or controls.
func validAddress(addr string) bool {
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" || len(addr) > 254 || strings.Contains(domain, "@") {
		return false
	}
	for _, r := range addr {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`<>,;"()[]\`, r) {
			return false
		}
	}
	return true
}
