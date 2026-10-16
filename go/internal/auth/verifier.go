// Package auth verifies Cognito access tokens for the shared endpoint
// authorization contract (docs/specs/security.md#shared-endpoint-authorization).
//
// Only RS256-signed access tokens from the configured issuer and an
// allowlisted app client are accepted. ID tokens, other algorithms and
// expired or not-yet-valid tokens are rejected.
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxTokenBytes bounds the work done on an untrusted Authorization header.
const maxTokenBytes = 8 * 1024

// ErrInvalidToken is returned for every rejected token. Detail is kept in
// the wrapped error for logs; callers must not echo it to clients.
var ErrInvalidToken = errors.New("invalid token")

// Claims are the verified fields handlers use for authorization.
type Claims struct {
	Subject  string
	Username string
	ClientID string
	Scopes   []string
	Expires  time.Time
}

// HasScope reports whether the exact wire scope (e.g. "s3mail/mail.read")
// was granted. Suffix or prefix matches never count.
func (c Claims) HasScope(scope string) bool {
	return slices.Contains(c.Scopes, scope)
}

// KeySource returns the RSA public key for a key ID.
type KeySource interface {
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Verifier checks access tokens against one issuer.
type Verifier struct {
	Issuer    string   // exact iss value, e.g. https://cognito-idp.<region>.amazonaws.com/<pool-id>
	ClientIDs []string // allowlisted Cognito app client IDs
	Keys      KeySource
	Now       func() time.Time // nil means time.Now
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type claims struct {
	Iss      string `json:"iss"`
	Sub      string `json:"sub"`
	Username string `json:"username"`
	TokenUse string `json:"token_use"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
	Exp      *int64 `json:"exp"`
	Nbf      *int64 `json:"nbf"`
}

// Verify validates a compact JWS access token and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	if len(token) > maxTokenBytes {
		return Claims{}, reject("token too large")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, reject("not a compact JWS")
	}

	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Claims{}, reject("header: %v", err)
	}
	// Only RS256: rejects "none", HMAC confusion and other algorithms.
	if h.Alg != "RS256" {
		return Claims{}, reject("algorithm %q not allowed", h.Alg)
	}
	if h.Kid == "" {
		return Claims{}, reject("missing kid")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, reject("signature encoding: %v", err)
	}
	key, err := v.Keys.Key(ctx, h.Kid)
	if err != nil {
		return Claims{}, reject("key %q: %v", h.Kid, err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, reject("bad signature")
	}

	// Claims are only parsed after the signature is verified.
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, reject("claims: %v", err)
	}
	if c.Iss != v.Issuer {
		return Claims{}, reject("issuer %q not trusted", c.Iss)
	}
	if c.TokenUse != "access" {
		return Claims{}, reject("token_use %q is not access", c.TokenUse)
	}
	if !slices.Contains(v.ClientIDs, c.ClientID) {
		return Claims{}, reject("client_id %q not allowed", c.ClientID)
	}
	if c.Sub == "" {
		return Claims{}, reject("missing sub")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	t := now().Unix()
	if c.Exp == nil || t >= *c.Exp {
		return Claims{}, reject("expired or missing exp")
	}
	if c.Nbf != nil && t < *c.Nbf {
		return Claims{}, reject("not yet valid")
	}

	return Claims{
		Subject:  c.Sub,
		Username: c.Username,
		ClientID: c.ClientID,
		Scopes:   strings.Fields(c.Scope),
		Expires:  time.Unix(*c.Exp, 0),
	}, nil
}

// BearerToken extracts the token from an Authorization header value.
// Only the "Bearer" scheme is accepted; Basic and others are rejected.
func BearerToken(authorization string) (string, error) {
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", reject("not a bearer credential")
	}
	return token, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func reject(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidToken, fmt.Sprintf(format, args...))
}
