// Package webhook receives relay delivery notifications
// (docs/specs/mail-flow.md#outbound-relay-interface): it checks the shared
// bearer secret and persists the raw body on the delivery queue before
// acknowledging. Parsing and correlation happen in the delivery worker,
// so an unparseable or not-yet-correlated event is never lost here.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// MaxBody bounds one notification body.
const MaxBody = 64 << 10

// Receiver handles POST /webhooks/<relay>.
type Receiver struct {
	// Secrets returns the accepted bearer secrets: the current one and,
	// during rotation, the previous one.
	Secrets func(ctx context.Context) ([]string, error)
	// Persist stores the raw body durably (the delivery queue).
	Persist func(ctx context.Context, body string) error
	Log     *slog.Logger
}

func respond(code int) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: code,
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       "{}",
	}
}

func (r *Receiver) authorized(ctx context.Context, header string) bool {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return false
	}
	secrets, err := r.Secrets(ctx)
	if err != nil {
		r.Log.Error("read webhook secret", "error", err)
		return false
	}
	match := 0
	for _, s := range secrets {
		if s != "" {
			match |= subtle.ConstantTimeCompare([]byte(token), []byte(s))
		}
	}
	return match == 1
}

func header(req events.APIGatewayProxyRequest, name string) string {
	for k, v := range req.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// Handle checks, persists and acknowledges one notification. Any non-2xx
// makes the relay retry later.
func (r *Receiver) Handle(ctx context.Context, req events.APIGatewayProxyRequest) events.APIGatewayProxyResponse {
	if req.HTTPMethod != "POST" {
		return respond(405)
	}
	if !r.authorized(ctx, header(req, "Authorization")) {
		r.Log.Warn("webhook rejected", "reason", "authorization")
		return respond(401)
	}
	body := req.Body
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return respond(400)
		}
		body = string(b)
	}
	if len(body) > MaxBody {
		return respond(413)
	}
	trimmed := strings.TrimSpace(body)
	if !json.Valid([]byte(trimmed)) || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
		r.Log.Warn("webhook rejected", "reason", "not a JSON object")
		return respond(400)
	}
	if err := r.Persist(ctx, trimmed); err != nil {
		r.Log.Error("persist webhook", "error", err)
		return respond(503)
	}
	return respond(200)
}
