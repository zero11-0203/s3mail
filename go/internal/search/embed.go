package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
)

// Semantic search (docs/specs/search.md#semantic-search): optional
// embeddings of each Email's subject and visible text, written by the
// indexer into docs_vec and queried by the search function. Nothing in the
// mail path depends on them; missing vectors only narrow semantic results.

// Embedder turns texts into vectors of one model.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Embedding input caps: the subject and the start of the visible text.
const (
	embedChars = 2000
	embedBatch = 32
	// MaxEmbedPerRun bounds the vectors one publication adds, so a full
	// backfill spreads over several runs.
	MaxEmbedPerRun = 512
	// MaxSemanticLimit bounds the hits of one semantic query.
	MaxSemanticLimit = 64
	maxSemanticText  = 1000
)

// EmbedText is the text one Email contributes to its vector.
func EmbedText(subject, body string) string {
	s := strings.TrimSpace(subject) + "\n" + strings.TrimSpace(body)
	if r := []rune(s); len(r) > embedChars {
		s = string(r[:embedChars])
	}
	return s
}

// OpenRouter embeds through an OpenAI-compatible embeddings API, routed
// only to providers that neither store nor train on the input.
type OpenRouter struct {
	BaseURL string // https://openrouter.ai/api/v1
	Name    string // the pinned model id
	// Key returns the API key; it is read once, on first use.
	Key  func(ctx context.Context) (string, error)
	HTTP *http.Client

	once sync.Once
	key  string
	err  error
}

// Model returns the pinned model id.
func (o *OpenRouter) Model() string { return o.Name }

type embedRequest struct {
	Model          string            `json:"model"`
	Input          []string          `json:"input"`
	EncodingFormat string            `json:"encoding_format"`
	Provider       map[string]string `json:"provider"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one unit-length vector per text, in order.
func (o *OpenRouter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	o.once.Do(func() { o.key, o.err = o.Key(ctx) })
	if o.err != nil {
		return nil, fmt.Errorf("embedding key: %w", o.err)
	}
	body, err := json.Marshal(embedRequest{Model: o.Name, Input: texts, EncodingFormat: "float", Provider: map[string]string{"data_collection": "deny"}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(o.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	req.Header.Set("Content-Type", "application/json")
	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error names the URL only, never the input text.
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: HTTP %d", resp.StatusCode)
	}
	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || vecs[d.Index] != nil || len(d.Embedding) == 0 {
			return nil, errors.New("embeddings: malformed response")
		}
		vecs[d.Index] = normalize(d.Embedding)
	}
	return vecs, nil
}

// normalize scales v to unit length, so a dot product is the cosine.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	n := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * n
	}
	return out
}
