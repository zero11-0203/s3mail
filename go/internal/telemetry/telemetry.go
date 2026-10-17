// Package telemetry traces the Lambda functions with OpenTelemetry and
// sends the spans to AWS X-Ray (docs/operations/monitoring.md#tracing-x-ray).
//
// Spans go over UDP to the X-Ray daemon that Lambda runs for functions
// with active tracing, in the OTLP-over-UDP format AWS's own distros use,
// so no collector layer is needed. Each invocation's span is the child of
// the Lambda segment named in _X_AMZN_TRACE_ID; AWS SDK calls are client
// spans that carry the trace header onward, so SQS messages and Lambda
// invocations continue or link the trace.
//
// Spans hold identifiers only, never mail content, addresses or tokens.
package telemetry

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	// The deprecation is withdrawn upstream (opentelemetry-go-contrib #9881).
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws" //nolint:staticcheck
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/propagators/aws/xray"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// provider is the installed tracer provider, nil outside Lambda.
var provider *sdktrace.TracerProvider

// Init installs the tracer provider and the X-Ray propagator. Outside
// Lambda, or without the daemon address that active tracing sets, it
// installs only the propagator and spans are not recorded.
func Init() {
	otel.SetTextMapPropagator(xray.Propagator{})
	fn := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	addr := daemonAddress(os.Getenv("AWS_XRAY_DAEMON_ADDRESS"))
	if fn == "" || addr == "" {
		return
	}
	exp, err := otlptrace.New(context.Background(), &udpClient{addr: addr})
	if err != nil {
		log.Printf("telemetry disabled: %v", err)
		return
	}
	provider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithIDGenerator(xray.NewIDGenerator()),
		// Lambda decides sampling; the invocation span follows it.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceName(fn),
			semconv.CloudProviderAWS,
			semconv.CloudPlatformAWSLambda,
			semconv.CloudRegion(os.Getenv("AWS_REGION")),
			semconv.FaaSName(fn),
			semconv.FaaSVersion(os.Getenv("AWS_LAMBDA_FUNCTION_VERSION")),
		)),
	)
	otel.SetTracerProvider(provider)
}

// AWS adds a client span to every AWS SDK call made with cfg and injects
// the trace header into its request.
func AWS(cfg *aws.Config) {
	otelaws.AppendMiddlewares(&cfg.APIOptions)
}

// HTTP wraps a transport (nil for the default) with client spans.
func HTTP(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return otelhttp.NewTransport(rt)
}

// Tracer is the instrumentation tracer for one package.
func Tracer(pkg string) trace.Tracer {
	return otel.Tracer("github.com/zero11-0203/s3mail/go/internal/" + pkg)
}

// Fail records err on span.
func Fail(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// Header returns the X-Ray trace header of ctx's span when it is
// recorded, for work that resumes the trace later; otherwise "".
func Header(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsSampled() {
		return ""
	}
	c := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, c)
	return c.Get("X-Amzn-Trace-Id")
}

// Resume starts a span that continues the trace in header, linked to the
// current span in ctx; without a header it is an ordinary child span.
func Resume(ctx context.Context, header, name string) (context.Context, trace.Span) {
	if header == "" {
		return Tracer("telemetry").Start(ctx, name)
	}
	link := trace.LinkFromContext(ctx)
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"X-Amzn-Trace-Id": header})
	return Tracer("telemetry").Start(ctx, name, trace.WithLinks(link))
}

// invocation starts the span of one Lambda invocation, the child of the
// Lambda segment in _X_AMZN_TRACE_ID. It is an internal span, so X-Ray
// nests it in the function's own segment instead of adding a node.
func invocation(ctx context.Context) (context.Context, trace.Span) {
	carrier := propagation.MapCarrier{"X-Amzn-Trace-Id": os.Getenv("_X_AMZN_TRACE_ID")}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	return Tracer("telemetry").Start(ctx, "handler", trace.WithSpanKind(trace.SpanKindInternal))
}

// flush exports the invocation's spans before Lambda freezes the
// environment.
func flush(ctx context.Context) {
	if provider != nil {
		if err := provider.ForceFlush(ctx); err != nil {
			log.Printf("telemetry flush: %v", err)
		}
	}
}

// Handler traces each invocation of a Lambda handler.
func Handler[In, Out any](h func(context.Context, In) (Out, error)) func(context.Context, In) (Out, error) {
	return func(ctx context.Context, in In) (Out, error) {
		ctx, span := invocation(ctx)
		out, err := h(ctx, in)
		if err != nil {
			Fail(span, err)
		}
		span.End()
		flush(ctx)
		return out, err
	}
}

// Func traces a Lambda handler that returns only an error.
func Func[In any](h func(context.Context, In) error) func(context.Context, In) error {
	return func(ctx context.Context, in In) error {
		_, err := Handler(func(ctx context.Context, in In) (struct{}, error) {
			return struct{}{}, h(ctx, in)
		})(ctx, in)
		return err
	}
}

// Streaming traces a handler whose response body Lambda streams after
// the handler returns: the span ends when the body is drained or closed,
// so it covers the whole stream (one span per push connection).
func Streaming[In any, Out io.ReadCloser](h func(context.Context, In) (Out, error), body func(Out) *io.Reader) func(context.Context, In) (Out, error) {
	return func(ctx context.Context, in In) (Out, error) {
		ctx, span := invocation(ctx)
		out, err := h(ctx, in)
		if err != nil {
			Fail(span, err)
		}
		var b *io.Reader
		if err == nil {
			b = body(out)
		}
		if b == nil || *b == nil {
			span.End()
			flush(ctx)
			return out, err
		}
		*b = &endingReader{r: *b, end: func() { span.End(); flush(context.WithoutCancel(ctx)) }}
		return out, nil
	}
}

// endingReader ends the invocation span at the end of the body.
type endingReader struct {
	r    io.Reader
	once sync.Once
	end  func()
}

func (e *endingReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		e.once.Do(e.end)
	}
	return n, err
}

func (e *endingReader) Close() error {
	e.once.Do(e.end)
	if c, ok := e.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
