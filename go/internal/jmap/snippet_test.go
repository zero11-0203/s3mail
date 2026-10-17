package jmap

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// indexSearcher answers from a local index built from the fixture, plus a
// stale document for an Email that no longer exists.
type indexSearcher struct {
	x     *search.Index
	gen   string
	err   string
	calls int
}

func (s *indexSearcher) Search(_ context.Context, req search.Request) (search.Response, error) {
	s.calls++
	if s.err != "" {
		return search.Response{Error: s.err}, nil
	}
	resp := search.Response{Generation: s.gen}
	switch req.Op {
	case "match":
		for _, c := range req.Conditions {
			ids, err := s.x.Match(c)
			if err != nil {
				return resp, err
			}
			resp.Matches = append(resp.Matches, ids)
		}
	case "snippets":
		stored, err := s.x.Stored(req.EmailIDs)
		if err != nil {
			return resp, err
		}
		resp.Snippets = map[string]search.Snippet{}
		for id, v := range stored {
			resp.Snippets[id] = search.Highlight(v[0], v[1], req.Terms)
		}
	}
	return resp, nil
}

func searchFixture(t *testing.T) (*Server, *indexSearcher) {
	s, m := fixture()
	x, err := search.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = x.Close() })
	var docs []search.Doc
	for id, e := range m.emails {
		docs = append(docs, search.Extract(id, e.ThreadID, e.ReceivedAt, m.raw[id]))
	}
	docs = append(docs, search.Extract("Mgone", "Tgone", t0, []byte("Subject: hello gone\r\n\r\nhello\r\n")))
	if err := x.Apply(docs, nil, search.Meta{Generation: "g1", Seq: 1, Epoch: 1, Schema: search.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	ix := &indexSearcher{x: x, gen: "g1"}
	s.Searcher = ix
	return s, ix
}

func TestTextQueryAndSnippets(t *testing.T) {
	s, ix := searchFixture(t)
	out := run(t, s, owner, `[
		["Email/query",{"accountId":"$ACCT","filter":{"text":"hello"}},"0"],
		["Email/query",{"accountId":"$ACCT","filter":{"operator":"AND","conditions":[{"hasAttachment":true},{"inMailbox":"inbox"}]}},"1"],
		["Email/query",{"accountId":"$ACCT","filter":{"operator":"NOT","conditions":[{"body":"body"}]}},"2"],
		["SearchSnippet/get",{"accountId":"$ACCT","filter":{"text":"hello"},"emailIds":["`+idA+`","Mgone","`+idB+`"]},"3"],
		["Email/queryChanges",{"accountId":"$ACCT","filter":{"text":"hello"},"sinceQueryState":"x"},"4"]]`)
	q := result(t, out, 0, "Email/query")
	if ids := q["ids"].([]any); len(ids) != 1 || ids[0] != idA || q["canCalculateChanges"] != false {
		t.Errorf("text query: destroyed Emails never match; %v", q)
	}
	if ids := result(t, out, 1, "Email/query")["ids"].([]any); len(ids) != 1 || ids[0] != idA {
		t.Errorf("hasAttachment %v", ids)
	}
	if ids := result(t, out, 2, "Email/query")["ids"].([]any); slices.Contains(ids, any(idB)) || len(ids) != 2 {
		t.Errorf("NOT body %v", ids)
	}
	sn := result(t, out, 3, "SearchSnippet/get")
	list := sn["list"].([]any)
	if nf := sn["notFound"].([]any); len(nf) != 1 || nf[0] != "Mgone" || len(list) != 2 {
		t.Fatalf("snippets: destroyed Emails are notFound; %v", sn)
	}
	a := list[0].(map[string]any)
	if a["emailId"] != idA || a["subject"] != nil || a["preview"] != "<mark>hello</mark> there, this is A" {
		t.Errorf("snippet %v", a)
	}
	if b := list[1].(map[string]any); b["preview"] != nil {
		t.Errorf("no match, no preview: %v", b)
	}
	if errType(t, out, 4) != "cannotCalculateChanges" {
		t.Errorf("text queryChanges: %v", out[4])
	}

	// Different generations never share a query state.
	ix.gen = "g2"
	out = run(t, s, owner, `[["Email/query",{"accountId":"$ACCT","filter":{"text":"hello"}},"0"]]`)
	if st := result(t, out, 0, "Email/query")["queryState"]; st == q["queryState"] {
		t.Errorf("same query state across generations: %v", st)
	}

	ix.err = "lagging"
	out = run(t, s, owner, `[["Email/query",{"accountId":"$ACCT","filter":{"text":"hello"}},"0"],
		["Email/query",{"accountId":"$ACCT","filter":{"inMailbox":"inbox"}},"1"]]`)
	if errType(t, out, 0) != "serverUnavailable" {
		t.Errorf("lagging index: %v", out[0])
	}
	if ids := result(t, out, 1, "Email/query")["ids"].([]any); len(ids) != 2 {
		t.Errorf("mailbox listing never depends on search: %v", ids)
	}
}
