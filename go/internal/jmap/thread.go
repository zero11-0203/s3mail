package jmap

import (
	"context"
	"slices"
)

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
	var ids []string
	for _, raw := range *a.IDs {
		if len(raw) > 1 && raw[0] == 'T' && validCore(raw[1:]) && !slices.Contains(ids, raw) {
			ids = append(ids, raw)
		}
	}
	var members map[string][]string
	st, e := c.s.consistent(ctx, func(State) (err error) {
		members, err = c.s.Store.ThreadMembers(ctx, ids)
		return err
	})
	if e != nil {
		return nil, e
	}
	resp := getResponse{AccountID: a.AccountID, State: stateToken(st, "Thread"), List: []map[string]any{}, NotFound: []string{}}
	seen := map[string]bool{}
	for _, tid := range *a.IDs {
		if seen[tid] {
			continue
		}
		seen[tid] = true
		if len(members[tid]) == 0 {
			resp.NotFound = append(resp.NotFound, tid)
			continue
		}
		resp.List = append(resp.List, pick(map[string]any{"id": tid, "emailIds": members[tid]}, props))
	}
	return resp, nil
}

func threadChanges(ctx context.Context, c *call) (any, *MethodError) {
	resp, _, e := objectChanges(ctx, c, "Thread", func(ch Change) ([]string, []string, []string) {
		return ch.Created["Thread"], ch.Updated["Thread"], ch.Destroyed["Thread"]
	})
	if e != nil {
		return nil, e
	}
	return resp, nil
}
