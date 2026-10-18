// Package agentcli is the owner's side of the mail agent
// (docs/specs/agent.md#identity-and-authorization): it invokes the
// AgentCore runtime with the owner's AWS credentials (SigV4) and the owner
// user id, and completes the token vault's session binding after consent.
// It never sees a JMAP token.
package agentcli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// OwnerUserID binds tokens and checkpoints to the single owner; the
// runtime uses the same value (agent/src/s3mail_agent/config.py).
const OwnerUserID = "owner"

// DefaultReturnURL is where the token vault returns after consent. It must
// be allowed on the runtime's workload identity
// (docs/operations/deployment.md#agent-deployment).
const DefaultReturnURL = "http://localhost:8765/callback"

// maxResponse bounds what the CLI reads from one invocation.
const maxResponse = 1 << 20

// ErrNotAuthorized means the vault holds no usable token: run login.
var ErrNotAuthorized = errors.New("not authorized: run `agent login`")

// API is the part of the AgentCore data plane the CLI uses.
type API interface {
	InvokeAgentRuntime(context.Context, *bedrockagentcore.InvokeAgentRuntimeInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
	CompleteResourceTokenAuth(context.Context, *bedrockagentcore.CompleteResourceTokenAuthInput, ...func(*bedrockagentcore.Options)) (*bedrockagentcore.CompleteResourceTokenAuthOutput, error)
}

// Result is the runtime's content-free status plus, for ask, the answer.
type Result struct {
	Status           string   `json:"status"`
	Error            string   `json:"error,omitempty"`
	Answer           string   `json:"answer,omitempty"`
	Citations        []string `json:"citations,omitempty"`
	AuthorizationURL string   `json:"authorization_url,omitempty"`
}

// Client invokes one runtime endpoint.
type Client struct {
	API        API
	RuntimeARN string
	Qualifier  string
}

// NewSessionID returns a fresh runtime session id, which is also the
// conversation's thread id. AgentCore requires at least 33 characters.
func NewSessionID() string {
	return "cli-" + rand.Text() + rand.Text()[:8]
}

// Invoke sends one payload on a session.
func (c *Client) Invoke(ctx context.Context, payload any, session string) (Result, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{}, err
	}
	out, err := c.API.InvokeAgentRuntime(ctx, &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  aws.String(c.RuntimeARN),
		Qualifier:        aws.String(c.Qualifier),
		RuntimeSessionId: aws.String(session),
		RuntimeUserId:    aws.String(OwnerUserID),
		ContentType:      aws.String("application/json"),
		Accept:           aws.String("application/json"),
		Payload:          body,
	})
	if err != nil {
		return Result{}, fmt.Errorf("invoke runtime: %w", err)
	}
	defer func() { _ = out.Response.Close() }()
	raw, err := io.ReadAll(io.LimitReader(out.Response, maxResponse))
	if err != nil {
		return Result{}, fmt.Errorf("read runtime response: %w", err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, fmt.Errorf("decode runtime response: %w", err)
	}
	return res, nil
}

// Ask asks one question on thread, or on a new thread when it is empty,
// and returns the answer and the thread id to continue with.
func (c *Client) Ask(ctx context.Context, question, thread string) (Result, string, error) {
	if thread == "" {
		thread = NewSessionID()
	}
	res, err := c.Invoke(ctx, map[string]string{"mode": "ask", "question": question}, thread)
	if err != nil {
		return Result{}, thread, err
	}
	switch res.Status {
	case "ok":
		return res, thread, nil
	case "authorization_required":
		return res, thread, ErrNotAuthorized
	default:
		return res, thread, fmt.Errorf("agent error: %s", orUnknown(res.Error, res.Status))
	}
}

// Login gets the owner's consent once. It asks the runtime for a consent
// URL, hands it to open, waits for the vault's redirect to returnURL and
// completes the session binding for the owner user id. It reports whether
// consent was needed.
func (c *Client) Login(ctx context.Context, returnURL string, open func(string)) (bool, error) {
	// Listen before invoking, so the redirect cannot arrive first.
	ln, callback, err := Listen(returnURL)
	if err != nil {
		return false, err
	}
	defer func() { _ = ln.Close() }()

	res, err := c.Invoke(ctx, map[string]string{"mode": "login"}, NewSessionID())
	if err != nil {
		return false, err
	}
	switch {
	case res.Status == "ok":
		return false, nil
	case res.Status != "authorization_required" || res.AuthorizationURL == "":
		return false, fmt.Errorf("login failed: %s", orUnknown(res.Error, res.Status))
	}
	open(res.AuthorizationURL)

	session, err := AwaitSession(ctx, ln, callback)
	if err != nil {
		return true, err
	}
	if _, err := c.API.CompleteResourceTokenAuth(ctx, &bedrockagentcore.CompleteResourceTokenAuthInput{
		SessionUri:     aws.String(session),
		UserIdentifier: &types.UserIdentifierMemberUserId{Value: OwnerUserID},
	}); err != nil {
		return true, fmt.Errorf("complete session binding: %w", err)
	}

	confirmed, err := c.Invoke(ctx, map[string]string{"mode": "login"}, NewSessionID())
	if err != nil {
		return true, err
	}
	if confirmed.Status != "ok" {
		return true, fmt.Errorf("login not confirmed: %s", orUnknown(confirmed.Error, confirmed.Status))
	}
	return true, nil
}

// Listen binds the loopback address of the return URL, never all interfaces.
func Listen(returnURL string) (net.Listener, *url.URL, error) {
	u, err := url.Parse(returnURL)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") || u.Port() == "" || u.Port() == "0" {
		return nil, nil, fmt.Errorf("return URL %q must be http://localhost:<port>/<path>", returnURL)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		return nil, nil, fmt.Errorf("listen on return port %s: %w", u.Port(), err)
	}
	return ln, u, nil
}

// AwaitSession serves the vault's redirect and returns its session id.
// Other paths get 404, and a request without a session id is rejected
// without ending the wait.
func AwaitSession(ctx context.Context, ln net.Listener, callback *url.URL) (string, error) {
	done := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callback.Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		session := r.URL.Query().Get("session_id")
		if session == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintln(w, "agent: unexpected request ignored.")
			return
		}
		_, _ = fmt.Fprintln(w, "agent: login received. Return to the terminal.")
		select {
		case done <- session:
		default:
		}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Let the browser receive its response before closing.
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case session := <-done:
		return session, nil
	}
}

func orUnknown(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "unknown"
}
