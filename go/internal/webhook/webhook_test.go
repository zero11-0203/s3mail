package webhook

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestReceiver(t *testing.T) {
	var stored []string
	var persistErr error
	r := &Receiver{
		Secrets: func(context.Context) ([]string, error) { return []string{"current-secret", "previous-secret"}, nil },
		Persist: func(_ context.Context, b string) error {
			if persistErr != nil {
				return persistErr
			}
			stored = append(stored, b)
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body := `{"event":"delivered","email_id":"1q","rcpt":"a@example.net"}`
	req := func(auth, b string, b64 bool) events.APIGatewayProxyRequest {
		return events.APIGatewayProxyRequest{HTTPMethod: "POST", Headers: map[string]string{"authorization": auth}, Body: b, IsBase64Encoded: b64}
	}
	cases := []struct {
		name string
		req  events.APIGatewayProxyRequest
		code int
	}{
		{"current", req("Bearer current-secret", body, false), 200},
		{"previous during rotation", req("Bearer previous-secret", base64.StdEncoding.EncodeToString([]byte(body)), true), 200},
		{"wrong secret", req("Bearer nope", body, false), 401},
		{"basic auth", req("Basic Y3VycmVudC1zZWNyZXQ=", body, false), 401},
		{"missing", req("", body, false), 401},
		{"not json", req("Bearer current-secret", "event=delivered", false), 400},
		{"too large", req("Bearer current-secret", `{"x":"`+string(make([]byte, MaxBody))+`"}`, false), 413},
		{"get", events.APIGatewayProxyRequest{HTTPMethod: "GET"}, 405},
	}
	for _, c := range cases {
		if got := r.Handle(context.Background(), c.req).StatusCode; got != c.code {
			t.Errorf("%s: %d, want %d", c.name, got, c.code)
		}
	}
	if len(stored) != 2 || stored[1] != body {
		t.Errorf("stored %q", stored)
	}
	persistErr = errors.New("queue down")
	if got := r.Handle(context.Background(), req("Bearer current-secret", body, false)).StatusCode; got != 503 {
		t.Errorf("persist failure: %d", got)
	}
}
