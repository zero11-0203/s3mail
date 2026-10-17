package jmap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// Owners maps verified Cognito subjects to internal account ids. The
// authorizer has already verified the token; this is the subject-to-account
// mapping step (docs/specs/security.md#browser-login).
type Owners map[string]string

// AccountFromRequest reads the authorizer context. A subject without a
// mapped account gets no account.
func AccountFromRequest(req events.APIGatewayProxyRequest, owners Owners) (Account, bool) {
	ctx := req.RequestContext.Authorizer
	sub, _ := ctx["sub"].(string)
	internal, ok := owners[sub]
	if sub == "" || !ok {
		return Account{}, false
	}
	scope, _ := ctx["scope"].(string)
	username, _ := ctx["username"].(string)
	if username == "" {
		username = internal
	}
	var exp time.Time
	switch v := ctx["exp"].(type) {
	case float64:
		exp = time.Unix(int64(v), 0)
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			exp = time.Unix(n, 0)
		}
	}
	return Account{InternalID: internal, Username: username, Scopes: strings.Fields(scope), Expires: exp}, true
}

var noStore = map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"}

func jsonResponse(status int, contentType string, v any) *events.APIGatewayProxyStreamingResponse {
	b, _ := json.Marshal(v)
	h := map[string]string{"Content-Type": contentType}
	for k, x := range noStore {
		h[k] = x
	}
	return &events.APIGatewayProxyStreamingResponse{StatusCode: status, Headers: h, Body: bytes.NewReader(b)}
}

func problemResponse(p *Problem) *events.APIGatewayProxyStreamingResponse {
	return jsonResponse(p.Status, "application/problem+json", p)
}

// HandleAPI serves GET /.well-known/jmap, POST /jmap and POST
// /upload/{accountId}.
func (s *Server) HandleAPI(ctx context.Context, req events.APIGatewayProxyRequest, owners Owners) (*events.APIGatewayProxyStreamingResponse, error) {
	if req.Resource == "/upload/{accountId}" {
		return s.HandleUpload(ctx, req, owners)
	}
	acct, ok := AccountFromRequest(req, owners)
	if !ok {
		return problemResponse(problem("forbidden", 403, "no account is mapped to this subject")), nil
	}
	switch {
	case req.HTTPMethod == http.MethodGet && req.Resource == "/.well-known/jmap":
		sess, err := s.Session(ctx, acct)
		if err != nil {
			s.Log.Error("jmap", "outcome", "serverFail", "step", "session", "error", err)
			return problemResponse(problem("serverFail", 500, "")), nil
		}
		return jsonResponse(200, "application/json", sess), nil
	case req.HTTPMethod == http.MethodPost && req.Resource == "/jmap":
		return s.api(ctx, acct, req), nil
	}
	return problemResponse(problem("notFound", 404, "")), nil
}

func (s *Server) api(ctx context.Context, acct Account, req events.APIGatewayProxyRequest) *events.APIGatewayProxyStreamingResponse {
	body := []byte(req.Body)
	if req.IsBase64Encoded {
		b, err := base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return problemResponse(problem("notJSON", 400, "undecodable body"))
		}
		body = b
	}
	if len(body) > MaxSizeRequest {
		p := problem("limit", 400, "request too large")
		p.Limit = "maxSizeRequest"
		return problemResponse(p)
	}
	ct := strings.ToLower(header(req.Headers, "Content-Type"))
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return problemResponse(problem("notJSON", 400, "Content-Type must be application/json"))
	}
	if !json.Valid(body) {
		return problemResponse(problem("notJSON", 400, "body is not valid JSON"))
	}
	var r Request
	if err := json.Unmarshal(body, &r); err != nil {
		return problemResponse(problem("notRequest", 400, err.Error()))
	}
	if p := s.validate(&r); p != nil {
		return problemResponse(p)
	}
	start := time.Now()
	resp := s.Execute(ctx, acct, &r)
	methods := map[string]string{}
	for _, c := range r.MethodCalls {
		methods[c.CallID] = c.Name
	}
	for _, inv := range resp.MethodResponses {
		outcome := "ok"
		if inv.Name == "error" {
			var e MethodError
			_ = json.Unmarshal(inv.Args, &e)
			outcome = e.Type
		}
		s.Log.Info("jmap", "callId", inv.CallID, "method", methods[inv.CallID], "outcome", outcome)
	}
	s.Log.Info("jmap request", "calls", len(r.MethodCalls), "latencyMs", time.Since(start).Milliseconds())
	return jsonResponse(200, "application/json", resp)
}

func header(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// HandleDownload serves GET /download/{accountId}/{blobId}/{name}: the
// blob must belong to a live Email or an unexpired upload of the caller's
// account.
func (s *Server) HandleDownload(ctx context.Context, req events.APIGatewayProxyRequest, owners Owners) (*events.APIGatewayProxyStreamingResponse, error) {
	acct, ok := AccountFromRequest(req, owners)
	if !ok || !acct.has(ScopeRead) {
		return problemResponse(problem("forbidden", 403, "")), nil
	}
	st, err := s.Store.State(ctx)
	if err != nil {
		return problemResponse(problem("serverFail", 500, "")), nil
	}
	s.epoch = st.Epoch
	p := req.PathParameters
	notFound := problemResponse(problem("notFound", 404, "no such blob"))
	if p["accountId"] != s.publicID(acct) {
		return notFound, nil
	}
	b, ok, err := s.readBlob(ctx, p["blobId"])
	if err != nil {
		s.Log.Error("download", "outcome", "serverFail", "error", err)
		return problemResponse(problem("serverFail", 500, "")), nil
	}
	if !ok {
		return notFound, nil
	}
	content := b.data
	ctype := req.QueryStringParameters["type"]
	if ctype == "" || strings.ContainsAny(ctype, "\r\n") {
		ctype = "application/octet-stream"
	}
	s.Log.Info("download", "outcome", "ok", "size", len(content))
	return &events.APIGatewayProxyStreamingResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":   ctype,
			"Content-Length": strconv.Itoa(len(content)),
			// Never render untrusted content under the API origin.
			"Content-Disposition":     contentDisposition(p["name"]),
			"Content-Security-Policy": "sandbox; default-src 'none'",
			"X-Content-Type-Options":  "nosniff",
			"Cache-Control":           "private, no-store",
		},
		Body: bytes.NewReader(content),
	}, nil
}

// contentDisposition always forces a download, with an ASCII fallback and
// an RFC 8187 UTF-8 filename.
func contentDisposition(name string) string {
	if name == "" {
		name = "download"
	}
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '/' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(r)
		}
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii.String(), pathEscape(name))
}

func pathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
