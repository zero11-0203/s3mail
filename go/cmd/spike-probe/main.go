// Command spike-probe measures the deployed streaming spike API and prints
// a pass/fail report. Requests carry a Cognito access token from -token-cmd
// (default "mail-auth token"), because the spike methods use the s3mail
// Lambda authorizer.
//
//	spike-probe -url https://<api-id>.execute-api.<region>.amazonaws.com/spike [-long]
//
// Every request is tagged with ?probe=<run>-<label>; the auth test prints
// the tags of rejected requests so CloudWatch Logs can confirm that none of
// them reached the spike function.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/spike"
)

type probe struct {
	base     string
	tokenCmd []string
	run      string
	client   *http.Client
	failed   bool
}

func main() {
	rawURL := flag.String("url", "", "stage invoke URL (terraform output spike_stream_invoke_url)")
	long := flag.Bool("long", false, "run the long stream test: 13 events 30s apart (6 minutes)")
	downloadMiB := flag.Int("download-mib", 25, "download test size in MiB")
	only := flag.String("only", "", "run one test: auth, stream, download or upload")
	tokenCmd := flag.String("token-cmd", "mail-auth token", "command that prints a valid access token")
	streamCount := flag.Int("count", 0, "stream test event count (overrides -long)")
	streamInterval := flag.Duration("interval", 10*time.Second, "stream test interval, with -count")
	flag.Parse()
	if *rawURL == "" {
		fmt.Fprintln(os.Stderr, "-url is required")
		os.Exit(2)
	}
	u, err := url.Parse(strings.TrimSuffix(*rawURL, "/"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad -url:", err)
		os.Exit(2)
	}
	if !regexp.MustCompile(`\.execute-api\.[a-z0-9-]+\.amazonaws\.com$`).MatchString(u.Host) {
		fmt.Fprintln(os.Stderr, "-url is not an execute-api URL")
		os.Exit(2)
	}
	if strings.TrimSpace(*tokenCmd) == "" {
		fmt.Fprintln(os.Stderr, "-token-cmd is required")
		os.Exit(2)
	}

	ctx := context.Background()
	p := &probe{
		base: u.String(), tokenCmd: strings.Fields(*tokenCmd), run: rand.Text()[:8],
		client: &http.Client{Timeout: 16 * time.Minute},
	}
	p.info("run", "probe tag prefix %s", p.run)

	if *only == "" || *only == "auth" {
		p.auth(ctx)
	}

	if *only == "" || *only == "stream" {
		switch {
		case *streamCount > 0:
			p.stream(ctx, *streamCount, *streamInterval)
		case *long:
			p.stream(ctx, 13, 30*time.Second)
		default:
			p.stream(ctx, 4, 10*time.Second)
		}
	}
	if *only == "" || *only == "download" {
		p.download(ctx, *downloadMiB*1024*1024)
	}
	if *only == "" || *only == "upload" {
		p.uploads(ctx)
	}

	if p.failed {
		fmt.Println("\nRESULT: FAIL")
		os.Exit(1)
	}
	fmt.Println("\nRESULT: PASS")
}

func (p *probe) report(ok bool, name, format string, args ...any) {
	mark := "PASS"
	if !ok {
		mark = "FAIL"
		p.failed = true
	}
	fmt.Printf("%-4s %-28s %s\n", mark, name, fmt.Sprintf(format, args...))
}

func (p *probe) info(name, format string, args ...any) {
	fmt.Printf("%-4s %-28s %s\n", "INFO", name, fmt.Sprintf(format, args...))
}

// token runs -token-cmd for each request, so long runs pick up refreshed
// tokens. The token is never printed.
func (p *probe) token(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, p.tokenCmd[0], p.tokenCmd[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(p.tokenCmd, " "), err)
	}
	t := strings.TrimSpace(string(out))
	if strings.Count(t, ".") != 2 {
		return "", errors.New("token command did not print a JWT")
	}
	return t, nil
}

// tag names a request in the spike function's logs.
func (p *probe) tag(label string) string { return p.run + "-" + label }

// do sends a request tagged with label and authorized with a valid token.
func (p *probe) do(ctx context.Context, method, path, label string, body []byte, contentType string) (*http.Response, error) {
	t, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	return p.send(ctx, method, path, label, "Bearer "+t, body, contentType)
}

// send sends a request with an explicit Authorization value ("" = none).
func (p *probe) send(ctx context.Context, method, path, label, authorization string, body []byte, contentType string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path+sep+"probe="+p.tag(label), r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	return p.client.Do(req)
}

// auth checks the shared bearer-token contract at the gateway: requests
// without a valid access token get 401 with a Bearer challenge, and a valid
// token's authorizer context reaches the integration.
func (p *probe) auth(ctx context.Context) {
	valid, err := p.token(ctx)
	if err != nil {
		p.report(false, "auth token", "%v", err)
		return
	}
	// Flip one character of the signature: well-formed, wrongly signed.
	sig := valid[strings.LastIndex(valid, ".")+1:]
	flipped := "A"
	if sig[0] == 'A' {
		flipped = "B"
	}
	tampered := valid[:strings.LastIndex(valid, ".")+1] + flipped + sig[1:]
	header, payload, _ := strings.Cut(valid, ".")
	payload, _, _ = strings.Cut(payload, ".")
	unsigned := header + "." + payload + "."

	var rejected []string
	for _, c := range []struct{ label, authorization string }{
		{"none", ""},
		{"empty-bearer", "Bearer "},
		{"basic", "Basic b3duZXI6cGFzc3dvcmQ="},
		{"garbage", "Bearer not-a-jwt"},
		{"unsigned", "Bearer " + unsigned},
		{"tampered", "Bearer " + tampered},
		{"no-scheme", valid},
	} {
		name := "reject " + c.label
		resp, err := p.send(ctx, http.MethodGet, "/stream?count=1&interval=0", c.label, c.authorization, nil, "")
		if err != nil {
			p.report(false, name, "request: %v", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		challenge := resp.Header.Get("WWW-Authenticate")
		ok := resp.StatusCode == http.StatusUnauthorized && strings.HasPrefix(challenge, "Bearer ") &&
			strings.Contains(challenge, "resource_metadata=")
		p.report(ok, name, "status %d, WWW-Authenticate=%q, body %.80s", resp.StatusCode, challenge, bytes.TrimSpace(b))
		rejected = append(rejected, p.tag(c.label))
	}
	p.info("rejected tags", "%s (none may appear in the spike function's logs)", strings.Join(rejected, " "))

	resp, err := p.do(ctx, http.MethodPost, "/upload", "valid", []byte("hello"), "application/octet-stream")
	if err != nil {
		p.report(false, "accept valid token", "request: %v", err)
		return
	}
	var res spike.UploadResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	_ = resp.Body.Close()
	p.report(resp.StatusCode == http.StatusOK && res.Authorized, "accept valid token",
		"status %d, authorizer context reached the integration=%v (tag %s must appear)", resp.StatusCode, res.Authorized, p.tag("valid"))

	p.metadata(ctx)
}

// metadata fetches the RFC 9728 document named by the 401 challenge.
func (p *probe) metadata(ctx context.Context) {
	resp, err := p.send(ctx, http.MethodGet, "/stream", "challenge", "", nil, "")
	if err != nil {
		p.report(false, "resource metadata", "request: %v", err)
		return
	}
	_ = resp.Body.Close()
	m := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(resp.Header.Get("WWW-Authenticate"))
	if m == nil {
		p.report(false, "resource metadata", "no resource_metadata in challenge")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m[1], nil)
	if err != nil {
		p.report(false, "resource metadata", "%v", err)
		return
	}
	resp, err = p.client.Do(req)
	if err != nil {
		p.report(false, "resource metadata", "request: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	err = json.NewDecoder(resp.Body).Decode(&doc)
	ok := err == nil && resp.StatusCode == http.StatusOK && doc.Resource == p.base &&
		len(doc.AuthorizationServers) == 1 && strings.HasPrefix(doc.AuthorizationServers[0], "https://cognito-idp.")
	p.report(ok, "resource metadata", "status %d, Content-Type=%q, resource=%q, authorization_servers=%v, scopes=%v",
		resp.StatusCode, resp.Header.Get("Content-Type"), doc.Resource, doc.AuthorizationServers, doc.ScopesSupported)
}

// stream checks that events arrive as they are sent, not at the end.
func (p *probe) stream(ctx context.Context, count int, interval time.Duration) {
	name := fmt.Sprintf("stream %dx%s", count, interval)
	start := time.Now()
	resp, err := p.do(ctx, http.MethodGet, fmt.Sprintf("/stream?count=%d&interval=%d", count, int(interval.Seconds())), "stream", nil, "")
	if err != nil {
		p.report(false, name, "request: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		p.report(false, name, "status %d: %s", resp.StatusCode, b)
		return
	}
	p.info("stream headers", "Content-Type=%q Transfer-Encoding=%v", resp.Header.Get("Content-Type"), resp.TransferEncoding)

	var opened time.Duration
	var maxLag time.Duration
	var recv []time.Time
	var sent []time.Time
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == ": open" && opened == 0 {
			opened = time.Since(start)
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		now := time.Now()
		var ev spike.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			p.report(false, name, "bad event %q: %v", data, err)
			return
		}
		s := time.UnixMilli(ev.SentMs)
		if lag := now.Sub(s); lag > maxLag {
			maxLag = lag
		}
		recv = append(recv, now)
		sent = append(sent, s)
	}
	if err := sc.Err(); err != nil {
		p.report(false, name, "after %d events, %s: %v", len(recv), time.Since(start).Round(time.Second), err)
		return
	}
	total := time.Since(start)
	if len(recv) != count {
		p.report(false, name, "got %d of %d events in %s", len(recv), count, total.Round(time.Second))
		return
	}
	// Incremental delivery: the first event must arrive well before the
	// last one is even sent. Lag includes laptop/Lambda clock skew.
	// A single event cannot show incremental delivery, so it is not judged.
	incremental := count < 2 || recv[0].Before(sent[count-1].Add(-interval/2))
	ok := incremental && maxLag < 3*time.Second
	p.report(ok, name, "first byte %s, max event lag %s, total %s, incremental=%v",
		opened.Round(time.Millisecond), maxLag.Round(time.Millisecond), total.Round(time.Second), incremental)
}

// download checks a large streamed body byte-for-byte and its headers.
func (p *probe) download(ctx context.Context, n int) {
	name := fmt.Sprintf("download %d MiB", n/1024/1024)
	start := time.Now()
	resp, err := p.do(ctx, http.MethodGet, fmt.Sprintf("/download?bytes=%d&seed=probe", n), "download", nil, "")
	if err != nil {
		p.report(false, name, "request: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		p.report(false, name, "status %d: %s", resp.StatusCode, b)
		return
	}
	got := sha256.New()
	size, err := io.Copy(got, resp.Body)
	elapsed := time.Since(start)
	if err != nil {
		p.report(false, name, "after %d bytes, %s: %v", size, elapsed.Round(time.Millisecond), err)
		return
	}
	want := sha256.New()
	_, _ = io.Copy(want, io.LimitReader(spike.Pattern("probe"), int64(n)))
	match := bytes.Equal(got.Sum(nil), want.Sum(nil))
	p.report(match && size == int64(n), name, "%d bytes in %s (%.1f MB/s), sha256 match=%v",
		size, elapsed.Round(time.Millisecond), float64(size)/1e6/elapsed.Seconds(), match)

	for h, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Disposition":    `attachment; filename="spike.bin"`,
		"X-Content-Type-Options": "nosniff",
	} {
		got := resp.Header.Get(h)
		p.report(got == want, "header "+h, "%q", got)
	}
}

// uploads checks the 4 MiB boundary, then records where the
// infrastructure itself starts rejecting larger bodies.
func (p *probe) uploads(ctx context.Context) {
	cases := []struct {
		size   int
		expect int // 0 = informational only
	}{
		{spike.MaxUpload, http.StatusOK},
		{spike.MaxUpload + 1, http.StatusRequestEntityTooLarge},
		{spike.MaxUpload + 512*1024, 0},
		{5 * 1024 * 1024, 0},
		{6 * 1024 * 1024, 0},
	}
	for _, c := range cases {
		name := fmt.Sprintf("upload %d bytes", c.size)
		body, _ := io.ReadAll(io.LimitReader(spike.Pattern(fmt.Sprint("up", c.size)), int64(c.size)))
		resp, err := p.do(ctx, http.MethodPost, "/upload", fmt.Sprint("upload", c.size), body, "application/octet-stream")
		if err != nil {
			p.report(false, name, "request: %v", err)
			continue
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var res spike.UploadResult
		_ = json.Unmarshal(out, &res)
		summary := fmt.Sprintf("status %d, server size %d, base64=%v", resp.StatusCode, res.Size, res.Base64)
		if resp.StatusCode != http.StatusOK && res.Size == 0 {
			summary = fmt.Sprintf("status %d: %.120s", resp.StatusCode, out)
		}
		if c.expect == 0 {
			p.info(name, "%s", summary)
			continue
		}
		ok := resp.StatusCode == c.expect
		if c.expect == http.StatusOK {
			sum := sha256.Sum256(body)
			ok = ok && res.Size == c.size && res.SHA256 == hex.EncodeToString(sum[:]) && res.Authorized
			summary += fmt.Sprintf(", sha256 match=%v, authorized=%v", res.SHA256 == hex.EncodeToString(sum[:]), res.Authorized)
		}
		p.report(ok, name, "%s", summary)
	}
}
