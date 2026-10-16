// Package spike implements the Phase 0 response-streaming proof: a Go
// Lambda behind a Regional REST API with STREAM integrations
// (docs/roadmap.md, Phase 0). It is temporary and holds no mail data.
package spike

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// MaxUpload is the JMAP maxSizeUpload under test (docs/specs/jmap.md).
const MaxUpload = 4 * 1024 * 1024

const (
	maxDownload   = 100 * 1024 * 1024
	maxEvents     = 60
	maxInterval   = 60 * time.Second
	maxStreamTime = 14 * time.Minute
)

// Handle routes one API Gateway proxy request. Every route returns a
// streaming response, because every integration uses STREAM mode.
func Handle(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
	// The probe tags requests so CloudWatch Logs show which ones reached
	// this function; requests the authorizer rejects must never appear.
	log.Printf("spike: %s %s probe=%q", req.HTTPMethod, req.Resource, req.QueryStringParameters["probe"])
	switch {
	case req.HTTPMethod == http.MethodGet && req.Resource == "/stream":
		return stream(ctx, req.QueryStringParameters)
	case req.HTTPMethod == http.MethodGet && req.Resource == "/download":
		return download(req.QueryStringParameters)
	case req.HTTPMethod == http.MethodPost && req.Resource == "/upload":
		return upload(req)
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "notFound"}), nil
	}
}

// Event is one server-sent event payload on /stream.
type Event struct {
	Seq    int   `json:"seq"`
	SentMs int64 `json:"sentMs"`
}

// stream writes count events, interval apart, as text/event-stream.
func stream(ctx context.Context, q map[string]string) (*events.APIGatewayProxyStreamingResponse, error) {
	count, err := intParam(q, "count", 4, 1, maxEvents)
	if err != nil {
		return badRequest(err), nil
	}
	secs, err := intParam(q, "interval", 10, 0, int(maxInterval/time.Second))
	if err != nil {
		return badRequest(err), nil
	}
	interval := time.Duration(secs) * time.Second
	if time.Duration(count-1)*interval > maxStreamTime {
		return badRequest(fmt.Errorf("stream longer than %s", maxStreamTime)), nil
	}

	pr, pw := io.Pipe()
	go func() {
		// An initial comment lets the client see the stream open at once.
		if _, err := io.WriteString(pw, ": open\n\n"); err != nil {
			return
		}
		for i := range count {
			if i > 0 {
				select {
				case <-ctx.Done():
					_ = pw.CloseWithError(ctx.Err())
					return
				case <-time.After(interval):
				}
			}
			data, _ := json.Marshal(Event{Seq: i, SentMs: time.Now().UnixMilli()})
			if _, err := fmt.Fprintf(pw, "id: %d\nevent: tick\ndata: %s\n\n", i, data); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	return &events.APIGatewayProxyStreamingResponse{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"Content-Type":  "text/event-stream",
			"Cache-Control": "no-cache",
		},
		Body: pr,
	}, nil
}

// download streams n deterministic bytes; the client regenerates the same
// Pattern to verify them.
func download(q map[string]string) (*events.APIGatewayProxyStreamingResponse, error) {
	n, err := intParam(q, "bytes", 1024*1024, 0, maxDownload)
	if err != nil {
		return badRequest(err), nil
	}
	seed := q["seed"]
	if seed == "" {
		seed = "download"
	}
	return &events.APIGatewayProxyStreamingResponse{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"Content-Type":           "application/octet-stream",
			"Content-Length":         strconv.Itoa(n),
			"Content-Disposition":    `attachment; filename="spike.bin"`,
			"X-Content-Type-Options": "nosniff",
			"Cache-Control":          "no-store",
		},
		Body: io.LimitReader(Pattern(seed), int64(n)),
	}, nil
}

// UploadResult is the /upload response body.
type UploadResult struct {
	Size     int    `json:"size"`
	SHA256   string `json:"sha256,omitempty"`
	Base64   bool   `json:"base64"`
	Error    string `json:"error,omitempty"`
	MaxBytes int    `json:"maxBytes"`
	// Authorized reports that the authorizer's context (sub, clientId,
	// scope) reached the integration.
	Authorized bool `json:"authorized"`
}

// upload measures the request body API Gateway delivered and enforces
// MaxUpload on the decoded bytes.
func upload(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyStreamingResponse, error) {
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return badRequest(fmt.Errorf("invalid base64 body: %w", err)), nil
		}
		body = decoded
	}
	res := UploadResult{Size: len(body), Base64: req.IsBase64Encoded, MaxBytes: MaxUpload,
		Authorized: req.RequestContext.Authorizer["sub"] != nil && req.RequestContext.Authorizer["scope"] != nil}
	if len(body) > MaxUpload {
		res.Error = "tooLarge"
		return jsonResponse(http.StatusRequestEntityTooLarge, res), nil
	}
	sum := sha256.Sum256(body)
	res.SHA256 = hex.EncodeToString(sum[:])
	return jsonResponse(http.StatusOK, res), nil
}

// Pattern returns an endless deterministic byte stream:
// SHA-256(seed || counter) blocks for counter = 0, 1, 2, ...
func Pattern(seed string) io.Reader {
	return &pattern{seed: []byte(seed)}
}

type pattern struct {
	seed    []byte
	counter uint64
	buf     []byte
}

func (p *pattern) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		if len(p.buf) == 0 {
			var ctr [8]byte
			binary.BigEndian.PutUint64(ctr[:], p.counter)
			p.counter++
			sum := sha256.Sum256(append(append([]byte{}, p.seed...), ctr[:]...))
			p.buf = sum[:]
		}
		c := copy(b[n:], p.buf)
		p.buf = p.buf[c:]
		n += c
	}
	return n, nil
}

func intParam(q map[string]string, name string, def, lo, hi int) (int, error) {
	s, ok := q[name]
	if !ok || s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("%s must be an integer in [%d, %d]", name, lo, hi)
	}
	return v, nil
}

func badRequest(err error) *events.APIGatewayProxyStreamingResponse {
	return jsonResponse(http.StatusBadRequest, map[string]any{"error": "badRequest", "detail": err.Error()})
}

func jsonResponse(status int, v any) *events.APIGatewayProxyStreamingResponse {
	b, _ := json.Marshal(v)
	return &events.APIGatewayProxyStreamingResponse{
		StatusCode: status,
		Headers: map[string]string{
			"Content-Type":   "application/json",
			"Content-Length": strconv.Itoa(len(b)),
		},
		Body: strings.NewReader(string(b)),
	}
}
