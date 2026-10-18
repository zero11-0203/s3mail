package jmap

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// CapSemantic is s3mail's semantic search capability
// (docs/specs/jmap.md#semantic-search), advertised only when the server
// is configured with it. It is read-only and needs mail.read.
const CapSemantic = "urn:s3mail:params:jmap:semantic"

const maxSemanticText = 1000

func semanticCapability() map[string]any {
	return map[string]any{"maxLimit": search.MaxSemanticLimit, "maxTextLength": maxSemanticText}
}

// semantic reports whether SemanticEmail/query is offered.
func (s *Server) semantic() bool { return s.Semantic && s.Searcher != nil }

type semanticArgs struct {
	AccountID string `json:"accountId"`
	Text      string `json:"text"`
	Limit     *int   `json:"limit"`
}

// semanticEmailQuery returns the ids of live Emails whose meaning is
// closest to text, best first. Ranking comes from the index's vectors;
// every id is checked against the live account before it is returned, so
// destroyed mail never appears.
func semanticEmailQuery(ctx context.Context, c *call) (any, *MethodError) {
	var a semanticArgs
	if e := c.decode(&a, &a.AccountID); e != nil {
		return nil, e
	}
	text := strings.TrimSpace(a.Text)
	if text == "" || utf8.RuneCountInString(text) > maxSemanticText {
		return nil, methodErr("invalidArguments", "text is 1 to 1000 characters")
	}
	limit := 20
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit < 1 || limit > search.MaxSemanticLimit {
		return nil, methodErr("invalidArguments", "limit is 1 to 64")
	}
	resp, e := c.s.search(ctx, search.Request{Op: "semantic", Text: text, Limit: limit})
	if e != nil {
		return nil, e
	}
	ids := make([]string, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		ids = append(ids, h.EmailID)
	}
	var live map[string]EmailMeta
	if _, e := c.s.consistent(ctx, func(State) (err error) {
		live, err = c.s.Store.GetEmails(ctx, ids)
		return err
	}); e != nil {
		return nil, e
	}
	out := []string{}
	for _, id := range ids {
		if _, ok := live[id]; ok {
			out = append(out, id)
		}
	}
	return struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{a.AccountID, out}, nil
}
