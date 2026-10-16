package spike

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// readPrelude splits a streaming response into its JSON prelude and body,
// checking the 8-null-byte delimiter API Gateway requires.
func readPrelude(t *testing.T, r io.Reader) (prelude map[string]any, body []byte) {
	t.Helper()
	all, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	i := bytes.Index(all, make([]byte, 8))
	if i < 0 {
		t.Fatalf("no 8-null-byte delimiter in %q", all)
	}
	if err := json.Unmarshal(all[:i], &prelude); err != nil {
		t.Fatalf("prelude is not JSON: %v", err)
	}
	return prelude, all[i+8:]
}

func call(t *testing.T, req events.APIGatewayProxyRequest) (int, map[string]any, []byte) {
	t.Helper()
	resp, err := Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	prelude, body := readPrelude(t, resp)
	return int(prelude["statusCode"].(float64)), prelude["headers"].(map[string]any), body
}

func TestDownloadMatchesPattern(t *testing.T) {
	const n = 100_000
	status, headers, body := call(t, events.APIGatewayProxyRequest{
		HTTPMethod: http.MethodGet, Resource: "/download",
		QueryStringParameters: map[string]string{"bytes": "100000", "seed": "s"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	want, _ := io.ReadAll(io.LimitReader(Pattern("s"), n))
	if !bytes.Equal(body, want) {
		t.Fatalf("body differs from Pattern (len %d)", len(body))
	}
	for k, v := range map[string]string{
		"Content-Length":         "100000",
		"X-Content-Type-Options": "nosniff",
		"Content-Disposition":    `attachment; filename="spike.bin"`,
	} {
		if headers[k] != v {
			t.Errorf("header %s = %v, want %q", k, headers[k], v)
		}
	}
}

func TestUploadBoundary(t *testing.T) {
	tests := []struct {
		size   int
		b64    bool
		status int
	}{
		{MaxUpload, true, http.StatusOK},
		{MaxUpload + 1, true, http.StatusRequestEntityTooLarge},
		{10, false, http.StatusOK},
	}
	for _, tt := range tests {
		raw, _ := io.ReadAll(io.LimitReader(Pattern("u"), int64(tt.size)))
		body := string(raw)
		if tt.b64 {
			body = base64.StdEncoding.EncodeToString(raw)
		}
		status, _, out := call(t, events.APIGatewayProxyRequest{
			HTTPMethod: http.MethodPost, Resource: "/upload", Body: body, IsBase64Encoded: tt.b64,
		})
		if status != tt.status {
			t.Errorf("size %d: status = %d, want %d", tt.size, status, tt.status)
			continue
		}
		var res UploadResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("size %d: %v", tt.size, err)
		}
		if res.Size != tt.size {
			t.Errorf("size %d: reported %d", tt.size, res.Size)
		}
		if tt.status == http.StatusOK {
			sum := sha256.Sum256(raw)
			if res.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("size %d: sha256 mismatch", tt.size)
			}
		}
	}
}

func TestStreamIsIncremental(t *testing.T) {
	resp, err := Handle(context.Background(), events.APIGatewayProxyRequest{
		HTTPMethod: http.MethodGet, Resource: "/stream",
		QueryStringParameters: map[string]string{"count": "2", "interval": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(resp)
	if _, err := r.ReadBytes(0); err != nil { // start of the delimiter
		t.Fatal(err)
	}
	if _, err := r.Discard(7); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	var first time.Duration
	var seqs []int
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatal(err)
			}
			if len(seqs) == 0 {
				first = time.Since(start)
			}
			seqs = append(seqs, ev.Seq)
		}
	}
	if len(seqs) != 2 || seqs[0] != 0 || seqs[1] != 1 {
		t.Fatalf("events = %v", seqs)
	}
	if first > 500*time.Millisecond {
		t.Errorf("first event after %s; stream is not incremental", first)
	}
	if total := time.Since(start); total < time.Second {
		t.Errorf("stream finished after %s, want >= interval", total)
	}
}

func TestRejectsBadParams(t *testing.T) {
	for _, q := range []map[string]string{
		{"count": "0"}, {"count": "x"}, {"interval": "61"}, {"count": "60", "interval": "60"},
	} {
		status, _, _ := call(t, events.APIGatewayProxyRequest{HTTPMethod: http.MethodGet, Resource: "/stream", QueryStringParameters: q})
		if status != http.StatusBadRequest {
			t.Errorf("%v: status = %d, want 400", q, status)
		}
	}
	status, _, _ := call(t, events.APIGatewayProxyRequest{HTTPMethod: http.MethodGet, Resource: "/nope"})
	if status != http.StatusNotFound {
		t.Errorf("unknown route: status = %d", status)
	}
}
