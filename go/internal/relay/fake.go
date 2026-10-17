package relay

import (
	"context"
	"io"
	"sync"
)

// Fake is the in-memory adapter for tests. It runs Handoff like a real
// adapter and records what would have been transmitted.
type Fake struct {
	mu sync.Mutex
	// Results are returned in order; the last one repeats. An Accepted,
	// Unknown or final-stage result counts as transmitted.
	Results []Result
	Sent    []FakeSend
	calls   int
}

// FakeSend is one transmitted message.
type FakeSend struct {
	Envelope Envelope
	Body     []byte
}

// Submit implements Relay.
func (f *Fake) Submit(ctx context.Context, m Message) Result {
	f.mu.Lock()
	r := Result{Outcome: Accepted, Stage: "final", Reply: "250 2.0.0 OK"}
	if len(f.Results) > 0 {
		r = f.Results[min(f.calls, len(f.Results)-1)]
	}
	f.calls++
	f.mu.Unlock()
	if r.Stage == "rcpt" || r.Stage == "connect" || r.Stage == "auth" {
		return r
	}
	if m.Handoff != nil {
		if err := m.Handoff(ctx); err != nil {
			return Result{Outcome: NotAccepted, Stage: "handoff", Reply: err.Error()}
		}
	}
	body, err := io.ReadAll(m.Body)
	if err != nil {
		return Result{Outcome: NotAccepted, Stage: "read", Reply: err.Error()}
	}
	f.mu.Lock()
	f.Sent = append(f.Sent, FakeSend{Envelope: m.Envelope, Body: body})
	f.mu.Unlock()
	return r
}

// Calls is the number of attempts made.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
