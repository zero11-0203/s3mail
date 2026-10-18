package jmap

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zero11-0203/s3mail/go/internal/search"
)

// semanticSearcher ranks a fixed list of ids, including one destroyed Email.
type semanticSearcher struct {
	hits []search.Hit
	err  string
	got  search.Request
}

func (s *semanticSearcher) Search(_ context.Context, req search.Request) (search.Response, error) {
	s.got = req
	return search.Response{Generation: "g1", Hits: s.hits, Error: s.err}, nil
}

func runSemantic(t *testing.T, s *Server, acct Account, calls string) []any {
	t.Helper()
	calls = strings.ReplaceAll(calls, "$ACCT", acctID())
	var req Request
	if err := json.Unmarshal([]byte(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","`+CapSemantic+`"],"methodCalls":`+calls+`}`), &req); err != nil {
		t.Fatal(err)
	}
	if p := s.validate(&req); p != nil {
		return []any{[]any{"problem", map[string]any{"type": p.Type}}}
	}
	resp := s.Execute(context.Background(), acct, &req)
	b, _ := json.Marshal(resp.MethodResponses)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
}

func TestSemanticQueryReturnsLiveEmailsOnly(t *testing.T) {
	s, _ := fixture()
	ss := &semanticSearcher{hits: []search.Hit{{EmailID: "Mgone", Score: 0.9}, {EmailID: idB, Score: 0.8}, {EmailID: idA, Score: 0.5}}}
	s.Searcher, s.Semantic = ss, true
	readOnly := Account{InternalID: "primary", Scopes: []string{ScopeRead}}
	out := runSemantic(t, s, readOnly, `[
		["SemanticEmail/query",{"accountId":"$ACCT","text":"  lunch plans  ","limit":3},"0"],
		["Email/get",{"accountId":"$ACCT","#ids":{"resultOf":"0","name":"SemanticEmail/query","path":"/ids"},"properties":["subject"]},"1"]]`)
	r := result(t, out, 0, "SemanticEmail/query")
	ids := []string{}
	for _, v := range r["ids"].([]any) {
		ids = append(ids, v.(string))
	}
	if !slices.Equal(ids, []string{idB, idA}) {
		t.Errorf("ids %v: ranked, destroyed mail dropped", ids)
	}
	if ss.got.Op != "semantic" || ss.got.Text != "lunch plans" || ss.got.Limit != 3 {
		t.Errorf("request %+v", ss.got)
	}
	if list := result(t, out, 1, "Email/get")["list"].([]any); len(list) != 2 {
		t.Errorf("result reference: %v", list)
	}
}

func TestSemanticQueryArgumentsAndAvailability(t *testing.T) {
	s, _ := fixture()
	ss := &semanticSearcher{}
	s.Searcher, s.Semantic = ss, true
	for _, args := range []string{
		`{"accountId":"$ACCT","text":""}`,
		`{"accountId":"$ACCT","text":"` + strings.Repeat("x", 1001) + `"}`,
		`{"accountId":"$ACCT","text":"x","limit":0}`,
		`{"accountId":"$ACCT","text":"x","limit":65}`,
		`{"accountId":"$ACCT","text":"x","filter":{}}`,
	} {
		if e := errType(t, runSemantic(t, s, owner, `[["SemanticEmail/query",`+args+`,"0"]]`), 0); e != "invalidArguments" {
			t.Errorf("%.60s: %s", args, e)
		}
	}
	ss.err = "semanticUnavailable"
	if e := errType(t, runSemantic(t, s, owner, `[["SemanticEmail/query",{"accountId":"$ACCT","text":"x"},"0"]]`), 0); e != "serverUnavailable" {
		t.Errorf("unconfigured search function: %s", e)
	}
	if ss.got.Limit != 20 {
		t.Errorf("default limit %d", ss.got.Limit)
	}
	none := Account{InternalID: "primary"}
	if e := errType(t, runSemantic(t, s, none, `[["SemanticEmail/query",{"accountId":"$ACCT","text":"x"},"0"]]`), 0); e != "forbidden" {
		t.Errorf("without mail.read: %s", e)
	}

	// Not configured: neither advertised nor accepted.
	s.Semantic = false
	sess, err := s.Session(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sess.Capabilities[CapSemantic]; ok {
		t.Error("advertised while off")
	}
	out := runSemantic(t, s, owner, `[["SemanticEmail/query",{"accountId":"$ACCT","text":"x"},"0"]]`)
	if inv := out[0].([]any); inv[0] != "problem" || inv[1].(map[string]any)["type"] != "urn:ietf:params:jmap:error:unknownCapability" {
		t.Errorf("off: %v", out)
	}
	s.Semantic = true
	sess, _ = s.Session(context.Background(), owner)
	caps := sess.Accounts[acctID()].AccountCapabilities[CapSemantic].(map[string]any)
	if _, ok := sess.Capabilities[CapSemantic]; !ok || caps["maxLimit"] != search.MaxSemanticLimit {
		t.Errorf("session %v", sess.Capabilities)
	}
}
