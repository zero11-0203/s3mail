// Package agentcli is the owner's side of the mail agent
// (docs/specs/agent.md#identity-and-authorization): it invokes the
// AgentCore runtime with the owner's AWS credentials (SigV4) and the owner
// user id, and completes the token vault's session binding after consent.
// It never sees a JMAP token.
package agentcli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore/types"
)

// OwnerUserID binds tokens and checkpoints to the single owner; the
// runtime uses the same value (agent/src/s3mail_agent/config.py).
const OwnerUserID = "owner"

// sessionURI is the form of the vault's session id on the return URL.
var sessionURI = regexp.MustCompile(`^urn:ietf:params:oauth:request_uri:[A-Za-z0-9_.~=-]{1,512}$`)

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
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	Answer    string   `json:"answer,omitempty"`
	Citations []string `json:"citations,omitempty"`
	// Evidence kinds: email, mailboxes, search, or none for an answer that
	// consulted no mail.
	Evidence         []string `json:"evidence,omitempty"`
	AuthorizationURL string   `json:"authorization_url,omitempty"`
	// ReturnURL is where the vault redirects after consent: the loopback
	// address login listens on, as configured on the runtime.
	ReturnURL string `json:"return_url,omitempty"`
}

// NoMail reports whether the answer says it consulted no mail.
func (r Result) NoMail() bool {
	return len(r.Evidence) == 1 && r.Evidence[0] == "none"
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

// NewLoginState returns 256 random bits, URL-safe: the CSRF state login
// sends through the vault and expects back on its callback.
func NewLoginState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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
// URL with a fresh random state, listens on the runtime's return URL, hands
// the consent URL to open, and completes the session binding for the owner
// user id only for a callback that carries that state. It reports whether
// consent was needed.
func (c *Client) Login(ctx context.Context, open func(string)) (bool, error) {
	state := NewLoginState()
	res, err := c.Invoke(ctx, map[string]string{"mode": "login", "state": state}, NewSessionID())
	if err != nil {
		return false, err
	}
	switch {
	case res.Status == "ok":
		return false, nil
	case res.Status != "authorization_required" || res.AuthorizationURL == "" || res.ReturnURL == "":
		return false, fmt.Errorf("login failed: %s", orUnknown(res.Error, res.Status))
	}
	// The redirect follows consent in the browser, so listening before
	// opening the URL is early enough.
	ln, callback, err := Listen(res.ReturnURL)
	if err != nil {
		return false, err
	}
	defer func() { _ = ln.Close() }()
	open(res.AuthorizationURL)

	session, err := AwaitSession(ctx, ln, callback, state)
	if err != nil {
		return true, err
	}
	if _, err := c.API.CompleteResourceTokenAuth(ctx, &bedrockagentcore.CompleteResourceTokenAuthInput{
		SessionUri:     aws.String(session),
		UserIdentifier: &types.UserIdentifierMemberUserId{Value: OwnerUserID},
	}); err != nil {
		return true, fmt.Errorf("complete session binding: %w", err)
	}

	confirmed, err := c.Invoke(ctx, map[string]string{"mode": "login", "state": NewLoginState()}, NewSessionID())
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

// AwaitSession serves the vault's redirect and returns its session URI.
// Only a GET on the callback path that carries state as a query value and a
// well-formed session URI is accepted; anything else is rejected without
// ending the wait, so a stray or forged request cannot consume the login.
func AwaitSession(ctx context.Context, ln net.Listener, callback *url.URL, state string) (string, error) {
	done := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callback.Path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		query := r.URL.Query()
		session := query.Get("session_id")
		if !carriesState(query, state) || !sessionURI.MatchString(session) {
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

// carriesState reports whether some query value other than the session id
// is exactly state. The vault returns the login's custom state on the
// return URL; matching any value keeps the check independent of the
// parameter's name.
func carriesState(query url.Values, state string) bool {
	for key, values := range query {
		if key == "session_id" {
			continue
		}
		for _, v := range values {
			if subtle.ConstantTimeCompare([]byte(v), []byte(state)) == 1 {
				return true
			}
		}
	}
	return false
}

// PlainText makes runtime output safe to print: answers can quote mail, so
// terminal controls (C0 except newline and tab, DEL, C1, and bidirectional
// overrides) are dropped and invalid UTF-8 becomes U+FFFD
// (docs/specs/clients.md#design).
func PlainText(s string) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

func orUnknown(values ...string) string {
	for _, v := range values {
		if v != "" {
			return PlainText(v)
		}
	}
	return "unknown"
}
