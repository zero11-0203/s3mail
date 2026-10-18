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

	// Later phases: the digest text, the draft saved, the approval to show
	// and what an approved action did, preferences, and the triage result.
	Digest      string       `json:"digest,omitempty"`
	EmailID     string       `json:"email_id,omitempty"`
	DraftID     string       `json:"draft_id,omitempty"`
	Draft       *Draft       `json:"draft,omitempty"`
	Approval    *Approval    `json:"approval,omitempty"`
	Outcome     *Outcome     `json:"outcome,omitempty"`
	Preference  *Preference  `json:"preference,omitempty"`
	Preferences []Preference `json:"preferences,omitempty"`
	Forgotten   int          `json:"forgotten,omitempty"`
	Triage      string       `json:"triage,omitempty"`
}

// Draft is a reply the agent saved in Drafts.
type Draft struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Cc      []string `json:"cc"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
}

// Approval is a proposed action waiting for the owner: its exact effect
// as text and the hash an approval must carry.
type Approval struct {
	Hash      string `json:"hash"`
	Kind      string `json:"kind"`
	Summary   string `json:"summary"`
	ExpiresAt string `json:"expires_at"`
}

// Outcome is what the executor did with an approval.
type Outcome struct {
	Outcome      string   `json:"outcome"`
	Error        string   `json:"error,omitempty"`
	Updated      []string `json:"updated,omitempty"`
	Failed       []string `json:"failed,omitempty"`
	SubmissionID string   `json:"submission_id,omitempty"`
	Submissions  []string `json:"submissions,omitempty"`
}

// Preference is one owner-stated preference.
type Preference struct {
	ID   string `json:"id"`
	Text string `json:"text"`
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
	return c.Call(ctx, map[string]any{"mode": "ask", "question": question}, thread)
}

// Call sends one payload on thread, or on a new thread when it is empty.
// An answer and a proposal waiting for approval are both successes.
func (c *Client) Call(ctx context.Context, payload map[string]any, thread string) (Result, string, error) {
	if thread == "" {
		thread = NewSessionID()
	}
	res, err := c.Invoke(ctx, payload, thread)
	if err != nil {
		return Result{}, thread, err
	}
	return res, thread, Check(res)
}

// Check turns a runtime status into an error.
func Check(res Result) error {
	switch res.Status {
	case "ok", "approval_required":
		return nil
	case "authorization_required":
		return ErrNotAuthorized
	default:
		return fmt.Errorf("agent error: %s", orUnknown(res.Error, res.Status))
	}
}

// approvalHash is a proposal's SHA-256, as the runtime renders it.
var approvalHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ApprovalID joins the thread and the hash the owner approves; approve and
// reject resume exactly that thread with exactly that hash.
func ApprovalID(thread, hash string) string {
	return thread + ":" + hash
}

// ParseApprovalID splits an approval id from ApprovalID.
func ParseApprovalID(id string) (thread, hash string, err error) {
	thread, hash, ok := strings.Cut(strings.TrimSpace(id), ":")
	if !ok || len(thread) < 33 || !approvalHash.MatchString(hash) {
		return "", "", errors.New("an approval id is THREAD:HASH, as printed by agent act")
	}
	return thread, hash, nil
}

// Decide approves or rejects a pending proposal.
func (c *Client) Decide(ctx context.Context, id string, approve bool) (Result, error) {
	thread, hash, err := ParseApprovalID(id)
	if err != nil {
		return Result{}, err
	}
	res, _, err := c.Call(ctx, map[string]any{"mode": "approve", "hash": hash, "approve": approve}, thread)
	return res, err
}

// Login gets the owner's consent once. It asks the runtime for a consent
// URL with a fresh random state, listens on the runtime's return URL, hands
// the consent URL to open, and completes the session binding for the owner
// user id only for a callback that carries that state. It reports whether
// consent was needed. With force, the vault asks for consent even when it
// holds a token, as a newly granted scope needs.
func (c *Client) Login(ctx context.Context, open func(string), force bool) (bool, error) {
	state := NewLoginState()
	payload := map[string]any{"mode": "login", "state": state}
	if force {
		payload["force"] = true
	}
	res, err := c.Invoke(ctx, payload, NewSessionID())
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
