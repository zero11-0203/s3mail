package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestDaemonAddress(t *testing.T) {
	for in, want := range map[string]string{
		"169.254.79.129:2000":                   "169.254.79.129:2000",
		"tcp:127.0.0.1:2000 udp:127.0.0.2:2000": "127.0.0.2:2000",
		"":                                      "",
		"tcp:127.0.0.1:2000":                    "",
	} {
		if got := daemonAddress(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// daemon is a local UDP listener standing in for the X-Ray daemon.
func daemon(t *testing.T) (string, func() []*coltracepb.ExportTraceServiceRequest) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc.LocalAddr().String(), func() []*coltracepb.ExportTraceServiceRequest {
		var out []*coltracepb.ExportTraceServiceRequest
		buf := make([]byte, 1<<16)
		for {
			_ = pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return out
			}
			if n >= 1<<16 {
				t.Errorf("datagram of %d bytes", n)
			}
			msg := string(buf[:n])
			rest, ok := strings.CutPrefix(msg, udpHeader+sampledPrefix)
			if !ok {
				t.Fatalf("datagram %q", msg[:min(len(msg), 60)])
			}
			raw, err := base64.StdEncoding.DecodeString(rest)
			if err != nil {
				t.Fatal(err)
			}
			req := &coltracepb.ExportTraceServiceRequest{}
			if err := proto.Unmarshal(raw, req); err != nil {
				t.Fatal(err)
			}
			out = append(out, req)
		}
	}
}

func spanNames(reqs []*coltracepb.ExportTraceServiceRequest) []string {
	var names []string
	for _, r := range reqs {
		for _, rs := range r.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					names = append(names, s.Name)
				}
			}
		}
	}
	return names
}

func TestUDPExport(t *testing.T) {
	addr, read := daemon(t)
	// macOS limits datagrams to 9 KiB; Lambda's Linux allows 64 KiB.
	exp, err := otlptrace.New(context.Background(), &udpClient{addr: addr, max: 6 << 10})
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithIDGenerator(xray.NewIDGenerator()))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	tr := tp.Tracer("test")
	_, small := tr.Start(context.Background(), "small")
	small.End()
	// Enough large spans to need several datagrams.
	pad := strings.Repeat("x", 1500)
	for range 40 {
		_, s := tr.Start(context.Background(), "large", trace.WithAttributes(attribute.String("pad", pad)))
		s.End()
	}
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := read()
	names := spanNames(reqs)
	if len(names) != 41 || len(reqs) < 3 {
		t.Fatalf("%d spans in %d datagrams", len(names), len(reqs))
	}
	if reqs[0].ResourceSpans[0].Resource == nil {
		t.Error("resource lost in a split batch")
	}
}

// recorder installs an in-memory tracer provider for one test.
func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(xray.Propagator{})
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestHandlerContinuesLambdaTrace(t *testing.T) {
	rec := recorder(t)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "s3mail-test")
	t.Setenv("_X_AMZN_TRACE_ID", "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1")
	h := Handler(func(ctx context.Context, in string) (string, error) { return in + "!", nil })
	if out, err := h(context.Background(), "x"); out != "x!" || err != nil {
		t.Fatal(out, err)
	}
	s := rec.Ended()
	if len(s) != 1 || s[0].Name() != "handler" {
		t.Fatalf("spans %v", s)
	}
	if s[0].SpanContext().TraceID().String() != "5759e988bd862e3fe1be46a994272793" || s[0].Parent().SpanID().String() != "53995c3f42cd8ad8" {
		t.Errorf("not a child of the Lambda segment: %v %v", s[0].SpanContext().TraceID(), s[0].Parent().SpanID())
	}
}

type body struct {
	io.Reader
	closed bool
}

func (b *body) Close() error { b.closed = true; return nil }

type resp struct{ Body io.Reader }

func (r *resp) Read(p []byte) (int, error) { return r.Body.Read(p) }
func (r *resp) Close() error {
	if c, ok := r.Body.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func TestStreamingSpanCoversTheBody(t *testing.T) {
	rec := recorder(t)
	b := &body{Reader: strings.NewReader("event: state\n")}
	h := Streaming(func(context.Context, int) (*resp, error) { return &resp{Body: b}, nil },
		func(r *resp) *io.Reader { return &r.Body })
	r, err := h(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Ended()) != 0 {
		t.Fatal("span ended before the stream")
	}
	if got, _ := io.ReadAll(r); string(got) != "event: state\n" {
		t.Errorf("body %q", got)
	}
	if len(rec.Ended()) != 1 {
		t.Fatal("span not ended at EOF")
	}
	_ = r.Close()
	if !b.closed || len(rec.Ended()) != 1 {
		t.Error("close not passed on, or span ended twice")
	}
}

// fakeAWS answers every AWS request with an empty success and records
// the trace header.
type fakeAWS struct{ headers []string }

func (f *fakeAWS) Do(r *http.Request) (*http.Response, error) {
	f.headers = append(f.headers, r.Header.Get("X-Amzn-Trace-Id"))
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/x-amz-json-1.0"}},
		Body: io.NopCloser(bytes.NewReader([]byte("{}"))), Request: r}, nil
}

// TestAWSSpansCarryNoMailData checks the privacy rule
// (docs/operations/monitoring.md#design): SDK spans name the service,
// operation and table or queue, never item values or message bodies.
func TestAWSSpansCarryNoMailData(t *testing.T) {
	rec := recorder(t)
	fake := &fakeAWS{}
	cfg := aws.Config{Region: "us-east-1", HTTPClient: fake, Credentials: aws.AnonymousCredentials{}}
	AWS(&cfg)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	secret := "alice@example.com Subject: private"
	_, _ = dynamodb.NewFromConfig(cfg).PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("s3mail-mail"),
		Item:      map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: secret}},
	})
	_, _ = sqs.NewFromConfig(cfg).SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String("https://sqs.us-east-1.amazonaws.com/111111111111/s3mail-submit"), MessageBody: aws.String(secret),
	})
	parent.End()
	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("%d spans", len(spans))
	}
	for _, s := range spans {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "alice") || strings.Contains(kv.Value.String(), "private") {
				t.Errorf("%s: %s=%s", s.Name(), kv.Key, kv.Value.String())
			}
		}
	}
	// The trace header reaches the request, so SQS records it.
	for _, h := range fake.headers {
		if !strings.Contains(h, "Root=1-") {
			t.Errorf("trace header %q", h)
		}
	}
}
