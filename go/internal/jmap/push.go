package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// Push timing (docs/specs/jmap.md#push).
const (
	PollInterval = 3 * time.Second
	// Heartbeats stay well inside the 5-minute Regional idle timeout.
	maxQuiet   = 60 * time.Second
	maxPing    = 300 * time.Second
	streamTime = 14*time.Minute + 30*time.Second
)

var pushTypes = []string{"Email", "Mailbox", "Thread"}

// HandleEventSource serves GET /eventsource (RFC 8620 §7.3) by polling the
// strongly consistent account state. The stream ends at token expiry, on
// closeafter=state after the first change, or before the Lambda deadline.
func (s *Server) HandleEventSource(ctx context.Context, req events.APIGatewayProxyRequest, owners Owners) (*events.APIGatewayProxyStreamingResponse, error) {
	acct, ok := AccountFromRequest(req, owners)
	if !ok || !acct.has(ScopeRead) {
		return problemResponse(problem("forbidden", 403, "")), nil
	}
	q := req.QueryStringParameters
	var types []string
	for _, t := range strings.Split(q["types"], ",") {
		switch {
		case t == "*":
			types = pushTypes
		case slices.Contains(pushTypes, t):
			types = append(types, t)
		}
	}
	closeAfterState := q["closeafter"] == "state"
	if q["closeafter"] != "" && q["closeafter"] != "state" && q["closeafter"] != "no" {
		return problemResponse(problem("invalidArguments", 400, "closeafter is state or no")), nil
	}
	ping := time.Duration(0)
	if v := q["ping"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return problemResponse(problem("invalidArguments", 400, "ping is a non-negative integer")), nil
		}
		ping = min(time.Duration(n)*time.Second, maxPing)
	}
	st, err := s.Store.State(ctx)
	if err != nil {
		return problemResponse(problem("serverFail", 500, "")), nil
	}
	s.epoch = st.Epoch

	end := time.Now().Add(streamTime)
	if dl, ok := ctx.Deadline(); ok && dl.Add(-15*time.Second).Before(end) {
		end = dl.Add(-15 * time.Second)
	}
	if !acct.Expires.IsZero() && acct.Expires.Before(end) {
		end = acct.Expires
	}

	pr, pw := io.Pipe()
	s.Log.Info("push", "outcome", "open", "types", strings.Join(types, ","), "closeAfterState", closeAfterState, "ping", int(ping/time.Second))
	go func() {
		start := time.Now()
		err := s.stream(ctx, pw, acct, st, types, closeAfterState, ping, end)
		outcome := "closed"
		if err != nil {
			outcome = "disconnected"
		}
		s.Log.Info("push", "outcome", outcome, "durationMs", time.Since(start).Milliseconds())
		_ = pw.CloseWithError(err)
	}()
	return &events.APIGatewayProxyStreamingResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":           "text/event-stream",
			"Cache-Control":          "no-store",
			"X-Content-Type-Options": "nosniff",
		},
		Body: pr,
	}, nil
}

func (s *Server) stream(ctx context.Context, w io.Writer, acct Account, last State, types []string, closeAfterState bool, ping time.Duration, end time.Time) error {
	id := s.publicID(acct)
	send := func(event, data string) error {
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		return err
	}
	// stateEvent reports the requested types whose state differs from
	// prev; the first event reports them all.
	stateEvent := func(st State, prev *State) error {
		changed := map[string]string{}
		for _, t := range types {
			if prev == nil || st.Of(t) != prev.Of(t) {
				changed[t] = stateToken(st, t)
			}
		}
		if len(changed) == 0 {
			return nil
		}
		b, _ := json.Marshal(map[string]any{"@type": "StateChange", "changed": map[string]any{id: changed}})
		return send("state", string(b))
	}
	if _, err := io.WriteString(w, ": open\n\n"); err != nil {
		return err
	}
	if len(types) > 0 {
		if err := stateEvent(last, nil); err != nil {
			return err
		}
	}
	lastWrite, lastPing := time.Now(), time.Now()
	poll := PollInterval
	if s.PushPoll > 0 {
		poll = s.PushPoll
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			if now.After(end) {
				return nil
			}
			st, err := s.Store.State(ctx)
			if err != nil {
				return err
			}
			if relevant := slices.ContainsFunc(types, func(t string) bool { return st.Of(t) != last.Of(t) }); st.Epoch == last.Epoch && !relevant {
				last = st
			} else if st != last {
				prev := last
				last = st
				if len(types) > 0 {
					if err := stateEvent(st, &prev); err != nil {
						return err
					}
					lastWrite = now
					if closeAfterState {
						return nil
					}
				}
			}
			switch {
			case ping > 0 && now.Sub(lastPing) >= ping:
				if err := send("ping", fmt.Sprintf(`{"@type":"Ping","interval":%d}`, int(ping/time.Second))); err != nil {
					return err
				}
				lastPing, lastWrite = now, now
			case now.Sub(lastWrite) >= maxQuiet:
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return err
				}
				lastWrite = now
			}
		}
	}
}
