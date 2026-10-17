package jmap

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// Searcher is the search function (docs/specs/search.md#index-pipeline).
type Searcher interface {
	Search(ctx context.Context, req search.Request) (search.Response, error)
}

type snippetArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	EmailIDs  []string        `json:"emailIds"`
}

type snippetObject struct {
	EmailID string  `json:"emailId"`
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
}

// searchSnippetGet highlights the filter's text terms in live Emails only;
// destroyed or unknown ids are notFound before any text is read
// (RFC 8621 §5.1).
func searchSnippetGet(ctx context.Context, c *call) (any, *MethodError) {
	var a snippetArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	if a.EmailIDs == nil {
		return nil, methodErr("invalidArguments", "emailIds is required")
	}
	if len(a.EmailIDs) > MaxObjectsInGet {
		return nil, methodErr("requestTooLarge", "")
	}
	q, e := planEmailQuery(a.Filter, nil, false)
	if e != nil {
		return nil, e
	}
	var ids []string
	for _, raw := range a.EmailIDs {
		id, _ := c.resolveID(raw)
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	var live map[string]EmailMeta
	if _, e := c.s.consistent(ctx, func(State) (err error) {
		live, err = c.s.Store.GetEmails(ctx, ids)
		return err
	}); e != nil {
		return nil, e
	}
	resp := struct {
		AccountID string          `json:"accountId"`
		List      []snippetObject `json:"list"`
		NotFound  []string        `json:"notFound"`
	}{AccountID: a.AccountID, List: []snippetObject{}, NotFound: []string{}}
	var found []string
	for _, id := range ids {
		if _, ok := live[id]; ok {
			found = append(found, id)
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	snippets := map[string]search.Snippet{}
	if len(found) > 0 && len(q.fp.terms) > 0 {
		r, e := c.s.search(ctx, search.Request{Op: "snippets", EmailIDs: found, Terms: q.fp.terms})
		if e != nil {
			return nil, e
		}
		snippets = r.Snippets
	}
	for _, id := range found {
		s := snippets[id]
		resp.List = append(resp.List, snippetObject{EmailID: id, Subject: s.Subject, Preview: s.Preview})
	}
	return resp, nil
}
