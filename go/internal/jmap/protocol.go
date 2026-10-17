package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Capability URIs.
const (
	CapCore = "urn:ietf:params:jmap:core"
	CapMail = "urn:ietf:params:jmap:mail"
)

// Advertised limits (docs/specs/jmap.md#limits-and-blobs).
const (
	MaxSizeUpload         = 4 << 20
	MaxConcurrentUpload   = 4
	MaxSizeRequest        = 1 << 20
	MaxConcurrentRequests = 4
	MaxCallsInRequest     = 16
	MaxObjectsInGet       = 64
	MaxObjectsInSet       = 16
	MaxMailboxesPerEmail  = 8
	maxQueryLimit         = 1024
)

// Invocation is a [name, arguments, callId] triple.
type Invocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

func (inv *Invocation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 3 {
		return fmt.Errorf("invocation is not a 3-element array")
	}
	if err := json.Unmarshal(raw[0], &inv.Name); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[2], &inv.CallID); err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw[1])) == 0 || bytes.TrimSpace(raw[1])[0] != '{' {
		return fmt.Errorf("arguments are not an object")
	}
	inv.Args = raw[1]
	return nil
}

func (inv Invocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{inv.Name, inv.Args, inv.CallID})
}

// Request is a JMAP API request (RFC 8620 §3.3).
type Request struct {
	Using       []string          `json:"using"`
	MethodCalls []Invocation      `json:"methodCalls"`
	CreatedIDs  map[string]string `json:"createdIds,omitempty"`
}

// Response is a JMAP API response (RFC 8620 §3.4).
type Response struct {
	MethodResponses []Invocation      `json:"methodResponses"`
	CreatedIDs      map[string]string `json:"createdIds,omitempty"`
	SessionState    string            `json:"sessionState"`
}

// Problem is a request-level error (RFC 8620 §3.6.1, RFC 7807).
type Problem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Limit  string `json:"limit,omitempty"`
}

func problem(kind string, status int, detail string) *Problem {
	return &Problem{Type: "urn:ietf:params:jmap:error:" + kind, Status: status, Detail: detail}
}

// MethodError is a method-level error (RFC 8620 §3.6.2).
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

func (e *MethodError) Error() string { return e.Type + ": " + e.Description }

func methodErr(kind, desc string) *MethodError { return &MethodError{Type: kind, Description: desc} }

// SetError is a per-object /set error (RFC 8620 §5.3).
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
	// NotFound lists missing blob ids for blobNotFound (RFC 8621 §4.6).
	NotFound []string `json:"notFound,omitempty"`
}

type handlerFunc func(ctx context.Context, c *call) (any, *MethodError)

type method struct {
	capability string
	scope      string
	fn         handlerFunc
}

type call struct {
	s    *Server
	acct Account
	args json.RawMessage
	// created maps creation ids to the ids they produced in this request
	// (RFC 8620 §5.3); it also receives the request's createdIds.
	created map[string]string
}

// resolveID maps "#creationId" to the created object id.
func (c *call) resolveID(id string) (string, bool) {
	if ref, ok := strings.CutPrefix(id, "#"); ok {
		v, found := c.created[ref]
		return v, found
	}
	return id, true
}

// decode strictly unmarshals the arguments and checks accountId.
func (c *call) decode(v any, accountID *string) *MethodError {
	dec := json.NewDecoder(bytes.NewReader(c.args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return methodErr("invalidArguments", err.Error())
	}
	if accountID != nil && *accountID != c.s.publicID(c.acct) {
		return methodErr("accountNotFound", "")
	}
	return nil
}

// Validate checks the request envelope before any method runs.
func (s *Server) validate(req *Request) *Problem {
	known := map[string]bool{CapCore: true, CapMail: true}
	for _, u := range req.Using {
		if !known[u] {
			return problem("unknownCapability", 400, "unsupported capability "+strconv.Quote(u))
		}
	}
	if req.MethodCalls == nil {
		return problem("notRequest", 400, "methodCalls is required")
	}
	if len(req.MethodCalls) > MaxCallsInRequest {
		p := problem("limit", 400, "too many method calls")
		p.Limit = "maxCallsInRequest"
		return p
	}
	return nil
}

// Execute runs every method call in order, resolving result references.
func (s *Server) Execute(ctx context.Context, acct Account, req *Request) *Response {
	using := map[string]bool{}
	for _, u := range req.Using {
		using[u] = true
	}
	resp := &Response{MethodResponses: []Invocation{}}
	created := map[string]string{}
	for k, v := range req.CreatedIDs {
		created[k] = v
	}
	var done []resolved
	for _, inv := range req.MethodCalls {
		out := s.invoke(ctx, acct, using, inv, done, created)
		resp.MethodResponses = append(resp.MethodResponses, out)
		var v any
		_ = json.Unmarshal(out.Args, &v)
		done = append(done, resolved{name: out.Name, callID: out.CallID, value: v})
	}
	if req.CreatedIDs != nil {
		resp.CreatedIDs = created
	}
	if st, err := s.Store.State(ctx); err == nil {
		resp.SessionState = s.sessionState(acct, st)
	}
	return resp
}

type resolved struct {
	name, callID string
	value        any
}

func (s *Server) invoke(ctx context.Context, acct Account, using map[string]bool, inv Invocation, done []resolved, created map[string]string) Invocation {
	fail := func(e *MethodError) Invocation {
		b, _ := json.Marshal(e)
		return Invocation{Name: "error", Args: b, CallID: inv.CallID}
	}
	m, ok := s.methods[inv.Name]
	if !ok || !using[m.capability] {
		return fail(methodErr("unknownMethod", inv.Name))
	}
	if !acct.has(m.scope) {
		return fail(methodErr("forbidden", "the token lacks "+m.scope))
	}
	args, merr := resolveReferences(inv.Args, done)
	if merr != nil {
		return fail(merr)
	}
	result, merr := m.fn(ctx, &call{s: s, acct: acct, args: args, created: created})
	if merr != nil {
		return fail(merr)
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fail(methodErr("serverFail", "encode response"))
	}
	return Invocation{Name: inv.Name, Args: b, CallID: inv.CallID}
}

// resolveReferences replaces "#name" arguments with the referenced values
// (RFC 8620 §3.7).
func resolveReferences(args json.RawMessage, done []resolved) (json.RawMessage, *MethodError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, methodErr("invalidArguments", "arguments are not an object")
	}
	changed := false
	for k, v := range m {
		if !strings.HasPrefix(k, "#") {
			continue
		}
		name := k[1:]
		if _, dup := m[name]; dup {
			return nil, methodErr("invalidArguments", "both "+name+" and #"+name+" given")
		}
		var ref struct {
			ResultOf string `json:"resultOf"`
			Name     string `json:"name"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(v, &ref); err != nil || ref.ResultOf == "" || ref.Name == "" {
			return nil, methodErr("invalidResultReference", "malformed reference")
		}
		var src *resolved
		for i := range done {
			if done[i].callID == ref.ResultOf {
				src = &done[i]
				break
			}
		}
		if src == nil || src.name != ref.Name {
			return nil, methodErr("invalidResultReference", "no "+ref.Name+" response for "+ref.ResultOf)
		}
		val, ok := evalPointer(src.value, ref.Path)
		if !ok {
			return nil, methodErr("invalidResultReference", "path "+ref.Path+" does not resolve")
		}
		b, _ := json.Marshal(val)
		delete(m, k)
		m[name] = b
		changed = true
	}
	if !changed {
		return args, nil
	}
	out, _ := json.Marshal(m)
	return out, nil
}

// evalPointer evaluates a JSON Pointer with the JMAP "*" extension: "*"
// maps the rest of the path over an array, flattening array results.
func evalPointer(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	tokens := strings.Split(path[1:], "/")
	return walk(v, tokens)
}

func walk(v any, tokens []string) (any, bool) {
	if len(tokens) == 0 {
		return v, true
	}
	tok := strings.ReplaceAll(strings.ReplaceAll(tokens[0], "~1", "/"), "~0", "~")
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[tok]
		if !ok {
			return nil, false
		}
		return walk(child, tokens[1:])
	case []any:
		if tok == "*" {
			out := []any{}
			for _, item := range x {
				r, ok := walk(item, tokens[1:])
				if !ok {
					return nil, false
				}
				if arr, isArr := r.([]any); isArr {
					out = append(out, arr...)
				} else {
					out = append(out, r)
				}
			}
			return out, true
		}
		i, err := strconv.Atoi(tok)
		if err != nil || i < 0 || i >= len(x) {
			return nil, false
		}
		return walk(x[i], tokens[1:])
	}
	return nil, false
}
