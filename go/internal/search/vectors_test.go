package search

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// topicEmbedder maps topic words to fixed dimensions, so nearness is
// predictable: money words, food words and travel words.
type topicEmbedder struct {
	model string
	fail  error
	calls int
	texts int
}

var topics = [][]string{{"budget", "invoice", "money", "costs"}, {"lunch", "dinner", "food", "午饭"}, {"flight", "hotel", "trip"}}

func (e *topicEmbedder) Model() string { return e.model }

func (e *topicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.fail != nil {
		return nil, e.fail
	}
	e.texts += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(topics)+1)
		v[len(topics)] = 0.1
		for d, words := range topics {
			for _, w := range words {
				v[d] += float32(strings.Count(strings.ToLower(t), w))
			}
		}
		out[i] = normalize(v)
	}
	return out, nil
}

func semantic(t *testing.T, rd *Reader, text string) []string {
	t.Helper()
	resp, err := rd.Serve(context.Background(), Request{Op: "semantic", Text: text, Limit: 2})
	if err != nil || resp.Error != "" {
		t.Fatalf("semantic %q: %v %q", text, err, resp.Error)
	}
	ids := []string{}
	for _, h := range resp.Hits {
		ids = append(ids, h.EmailID)
	}
	return ids
}

func TestSemanticSearchFollowsTheIndex(t *testing.T) {
	ctx := context.Background()
	src, _, ix, rd := pipeline(t)
	emb := &topicEmbedder{model: "test/embed-1"}
	ix.Embedder, rd.Embedder = emb, emb
	src.add("Ma", "Budget", "the annual budget and costs")
	src.add("Mb", "Lunch", "会议之后吃午饭 dinner")
	src.add("Mc", "Trip", "flight and hotel")
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := semantic(t, rd, "how much money"); got[0] != "Ma" {
		t.Errorf("money: %v", got)
	}
	if got := semantic(t, rd, "food plans"); got[0] != "Mb" {
		t.Errorf("food: %v", got)
	}

	// A destroyed Email leaves the vectors with its document.
	src.destroy("Ma")
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range semantic(t, rd, "money") {
		if id == "Ma" {
			t.Error("destroyed email still matches")
		}
	}
}

func TestEmbeddingFailureStillPublishesAndBackfills(t *testing.T) {
	ctx := context.Background()
	src, _, ix, rd := pipeline(t)
	emb := &topicEmbedder{model: "test/embed-1", fail: errors.New("HTTP 503")}
	ix.Embedder, rd.Embedder = emb, &topicEmbedder{model: "test/embed-1"}
	src.add("Ma", "Budget", "the annual budget")
	if err := ix.Run(ctx); err != nil {
		t.Fatalf("keyword publication waits on embeddings: %v", err)
	}
	if ids, _ := match(t, rd, "budget"); len(ids) != 1 {
		t.Errorf("keyword search %v", ids)
	}
	if got := semantic(t, rd, "money"); len(got) != 0 {
		t.Errorf("hits without vectors: %v", got)
	}

	// The next publication backfills the Email left without a vector.
	emb.fail = nil
	src.add("Mb", "Lunch", "dinner")
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.texts != 2 {
		t.Errorf("embedded %d texts, want the new and the backfilled one", emb.texts)
	}
	if got := semantic(t, rd, "money"); len(got) == 0 || got[0] != "Ma" {
		t.Errorf("backfilled: %v", got)
	}
}

func TestModelChangeClearsVectors(t *testing.T) {
	x, err := Open(t.TempDir() + "/i.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = x.Close() }()
	if err := x.Apply(testDocs(), nil, Meta{Generation: "g", Seq: 1, Epoch: 1, Schema: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if err := x.UseModel("a"); err != nil {
		t.Fatal(err)
	}
	missing, _ := x.MissingVectors(10)
	put := map[int64][]float32{}
	for _, p := range missing {
		put[p.Row] = normalize([]float32{1, 0})
	}
	if err := x.PutVectors(put); err != nil {
		t.Fatal(err)
	}
	if m, _ := x.MissingVectors(10); len(m) != 0 {
		t.Fatalf("still missing %d", len(m))
	}
	if hits, _ := x.Nearest([]float32{1, 0}, 10); len(hits) != len(put) || hits[0].Score < 0.99 {
		t.Fatalf("hits %v", hits)
	}
	// Vectors of another length are not compared.
	if hits, _ := x.Nearest([]float32{1, 0, 0}, 10); len(hits) != 0 {
		t.Fatalf("mismatched hits %v", hits)
	}
	if err := x.UseModel("b"); err != nil {
		t.Fatal(err)
	}
	if m, _ := x.MissingVectors(10); len(m) != len(put) {
		t.Fatalf("a new model keeps old vectors: %d missing", len(m))
	}
}

func TestSemanticQueriesNeedAnEmbedderAndBoundedInput(t *testing.T) {
	ctx := context.Background()
	_, _, _, rd := pipeline(t)
	if resp, err := rd.Serve(ctx, Request{Op: "semantic", Text: "x", Limit: 1}); err != nil || resp.Error != "semanticUnavailable" {
		t.Fatalf("no embedder: %+v %v", resp, err)
	}
	rd.Embedder = &topicEmbedder{model: "m"}
	for _, req := range []Request{
		{Op: "semantic", Text: "", Limit: 1},
		{Op: "semantic", Text: "x", Limit: 0},
		{Op: "semantic", Text: "x", Limit: MaxSemanticLimit + 1},
		{Op: "semantic", Text: strings.Repeat("x", 1001), Limit: 1},
	} {
		if _, err := rd.Serve(ctx, req); err == nil {
			t.Errorf("accepted %d chars, limit %d", len(req.Text), req.Limit)
		}
	}
}

func TestOpenRouterEmbedder(t *testing.T) {
	var got embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k-test" {
			http.Error(w, "bad", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if len(got.Input) == 3 {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
			return
		}
		// Out of order, as the API allows.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,2]},{"index":0,"embedding":[3,4]}]}`))
	}))
	defer srv.Close()
	keys := 0
	o := &OpenRouter{BaseURL: srv.URL + "/api/v1/", Name: "test/embed", HTTP: srv.Client(), Key: func(context.Context) (string, error) { keys++; return "k-test", nil }}
	vecs, err := o.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(vecs[0][0])-0.6) > 1e-6 || vecs[1][1] != 1 {
		t.Errorf("vectors %v", vecs)
	}
	if got.Model != "test/embed" || got.Provider["data_collection"] != "deny" || got.EncodingFormat != "float" {
		t.Errorf("request %+v", got)
	}
	if _, err := o.Embed(context.Background(), []string{"a", "b", "c"}); err == nil {
		t.Error("accepted fewer vectors than inputs")
	}
	if keys != 1 {
		t.Errorf("key read %d times", keys)
	}

	bad := &OpenRouter{BaseURL: srv.URL + "/api/v1", Name: "m", HTTP: srv.Client(), Key: func(context.Context) (string, error) { return "wrong", nil }}
	if _, err := bad.Embed(context.Background(), []string{"secret text"}); err == nil || strings.Contains(err.Error(), "secret text") {
		t.Errorf("err %v", err)
	}
}
