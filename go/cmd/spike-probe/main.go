// Command spike-probe measures the deployed streaming spike API and prints
// a pass/fail report. Requests are SigV4-signed with the caller's AWS
// credentials, because the spike methods use AWS_IAM authorization.
//
//	spike-probe -url https://<api-id>.execute-api.<region>.amazonaws.com/spike [-long]
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/zero11-0203/s3mail/go/internal/spike"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type probe struct {
	base   string
	region string
	creds  aws.CredentialsProvider
	signer *v4.Signer
	client *http.Client
	failed bool
}

func main() {
	rawURL := flag.String("url", "", "stage invoke URL (terraform output spike_stream_invoke_url)")
	long := flag.Bool("long", false, "run the long stream test: 13 events 30s apart (6 minutes)")
	downloadMiB := flag.Int("download-mib", 25, "download test size in MiB")
	only := flag.String("only", "", "run one test: stream, download or upload")
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
	m := regexp.MustCompile(`\.execute-api\.([a-z0-9-]+)\.amazonaws\.com$`).FindStringSubmatch(u.Host)
	if m == nil {
		fmt.Fprintln(os.Stderr, "-url is not an execute-api URL")
		os.Exit(2)
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(m[1]))
	if err != nil {
		fmt.Fprintln(os.Stderr, "load AWS config:", err)
		os.Exit(1)
	}
	p := &probe{
		base: u.String(), region: m[1], creds: cfg.Credentials,
		signer: v4.NewSigner(), client: &http.Client{Timeout: 16 * time.Minute},
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

func (p *probe) do(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	var r io.Reader
	hash := emptySHA256
	if body != nil {
		r = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, r)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	creds, err := p.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	if err := p.signer.SignHTTP(ctx, creds, req, hash, "execute-api", p.region, time.Now()); err != nil {
		return nil, err
	}
	return p.client.Do(req)
}

// stream checks that events arrive as they are sent, not at the end.
func (p *probe) stream(ctx context.Context, count int, interval time.Duration) {
	name := fmt.Sprintf("stream %dx%s", count, interval)
	start := time.Now()
	resp, err := p.do(ctx, http.MethodGet, fmt.Sprintf("/stream?count=%d&interval=%d", count, int(interval.Seconds())), nil, "")
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
	resp, err := p.do(ctx, http.MethodGet, fmt.Sprintf("/download?bytes=%d&seed=probe", n), nil, "")
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
		resp, err := p.do(ctx, http.MethodPost, "/upload", body, "application/octet-stream")
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
			ok = ok && res.Size == c.size && res.SHA256 == hex.EncodeToString(sum[:])
			summary += fmt.Sprintf(", sha256 match=%v", res.SHA256 == hex.EncodeToString(sum[:]))
		}
		p.report(ok, name, "%s", summary)
	}
}
