package telemetry

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The X-Ray daemon's OTLP datagram: a JSON header line, then the sampled
// traces prefix and a base64 OTLP ExportTraceServiceRequest, as sent by
// the AWS distros' OTLP UDP span exporters.
const (
	udpHeader     = `{"format":"json","version":1}` + "\n"
	sampledPrefix = "T1S"
	// maxPayload keeps a datagram, after base64, under the 64 KiB limit
	// of the daemon and of UDP on Linux.
	maxPayload = 45 << 10
)

// daemonAddress returns the UDP address from AWS_XRAY_DAEMON_ADDRESS,
// which is "host:port" or "tcp:host:port udp:host:port".
func daemonAddress(v string) string {
	for _, f := range strings.Fields(v) {
		if a, ok := strings.CutPrefix(f, "udp:"); ok {
			return a
		}
	}
	if f := strings.Fields(v); len(f) == 1 && !strings.Contains(f[0], "tcp:") {
		return f[0]
	}
	return ""
}

// udpClient is the otlptrace client that writes datagrams to the daemon.
type udpClient struct {
	addr string
	// max is the largest payload per datagram (maxPayload when zero).
	max  int
	mu   sync.Mutex
	conn net.Conn
}

func (c *udpClient) Start(context.Context) error {
	conn, err := net.Dial("udp", c.addr)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	return nil
}

func (c *udpClient) Stop(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *udpClient) UploadTraces(_ context.Context, spans []*tracepb.ResourceSpans) error {
	limit := c.max
	if limit == 0 {
		limit = maxPayload
	}
	payloads, err := encode(spans, limit)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("telemetry: daemon connection closed")
	}
	for _, p := range payloads {
		if _, err := c.conn.Write(datagram(p)); err != nil {
			return err
		}
	}
	return nil
}

func datagram(payload []byte) []byte {
	return []byte(udpHeader + sampledPrefix + base64.StdEncoding.EncodeToString(payload))
}

// encode serializes the spans into requests of at most limit bytes,
// splitting by span; a single span that is still too large is dropped.
func encode(spans []*tracepb.ResourceSpans, limit int) ([][]byte, error) {
	b, err := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: spans})
	if err != nil || len(b) <= limit {
		return [][]byte{b}, err
	}
	var out [][]byte
	for _, part := range split(spans) {
		p, err := encode(part, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, p...)
	}
	return out, nil
}

// split halves a batch by span, keeping each span's resource and scope;
// a batch of one span cannot be split and is dropped.
func split(rs []*tracepb.ResourceSpans) [][]*tracepb.ResourceSpans {
	type one struct {
		r *tracepb.ResourceSpans
		s *tracepb.ScopeSpans
		x *tracepb.Span
	}
	var all []one
	for _, r := range rs {
		for _, s := range r.ScopeSpans {
			for _, x := range s.Spans {
				all = append(all, one{r, s, x})
			}
		}
	}
	if len(all) <= 1 {
		return nil
	}
	group := func(items []one) []*tracepb.ResourceSpans {
		var out []*tracepb.ResourceSpans
		for _, it := range items {
			if len(out) == 0 || out[len(out)-1].Resource != it.r.Resource {
				out = append(out, &tracepb.ResourceSpans{Resource: it.r.Resource, SchemaUrl: it.r.SchemaUrl})
			}
			last := out[len(out)-1]
			if n := len(last.ScopeSpans); n == 0 || last.ScopeSpans[n-1].Scope != it.s.Scope {
				last.ScopeSpans = append(last.ScopeSpans, &tracepb.ScopeSpans{Scope: it.s.Scope, SchemaUrl: it.s.SchemaUrl})
			}
			ss := last.ScopeSpans[len(last.ScopeSpans)-1]
			ss.Spans = append(ss.Spans, it.x)
		}
		return out
	}
	mid := len(all) / 2
	return [][]*tracepb.ResourceSpans{group(all[:mid]), group(all[mid:])}
}
