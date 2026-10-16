package auth

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testIssuer = "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_EXAMPLE"
	testClient = "terminalclient"
)

var (
	now     = time.Unix(1_800_000_000, 0)
	keyA    = mustKey()
	keyB    = mustKey() // a key the issuer never published
	baseExp = now.Add(5 * time.Minute).Unix()
)

func mustKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func sign(key *rsa.PrivateKey, hdr, claims map[string]any) string {
	signing := b64(hdr) + "." + b64(claims)
	d := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func accessClaims() map[string]any {
	return map[string]any{
		"iss": testIssuer, "sub": "owner-sub", "token_use": "access",
		"client_id": testClient, "scope": "s3mail/mail.read s3mail/mail.write s3mail/mail.submit",
		"exp": baseExp, "iat": now.Unix(),
	}
}

func with(m map[string]any, k string, v any) map[string]any {
	c := map[string]any{}
	for kk, vv := range m {
		c[kk] = vv
	}
	if v == nil {
		delete(c, k)
	} else {
		c[k] = v
	}
	return c
}

func jwksServer(t *testing.T, fetches *atomic.Int32, keys map[string]*rsa.PublicKey) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		var set []map[string]string
		for kid, k := range keys {
			set = append(set, map[string]string{
				"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": set})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newVerifier(t *testing.T, fetches *atomic.Int32) *Verifier {
	srv := jwksServer(t, fetches, map[string]*rsa.PublicKey{"a": &keyA.PublicKey})
	clock := func() time.Time { return now }
	return &Verifier{
		Issuer: testIssuer, ClientIDs: []string{testClient},
		Keys: &JWKS{URL: srv.URL, Client: srv.Client(), Now: clock}, Now: clock,
	}
}

func TestVerify(t *testing.T) {
	rs := map[string]any{"alg": "RS256", "kid": "a"}
	hmacToken := func() string {
		// alg confusion: HS256 "signed" with the public modulus as secret.
		signing := b64(map[string]any{"alg": "HS256", "kid": "a"}) + "." + b64(accessClaims())
		m := hmac.New(sha256.New, keyA.N.Bytes())
		m.Write([]byte(signing))
		return signing + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}

	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{"valid access token", sign(keyA, rs, accessClaims()), true},
		{"ID token", sign(keyA, rs, with(with(accessClaims(), "token_use", "id"), "aud", testClient)), false},
		{"wrong issuer", sign(keyA, rs, with(accessClaims(), "iss", "https://evil.example.com")), false},
		{"issuer suffix", sign(keyA, rs, with(accessClaims(), "iss", testIssuer+"x")), false},
		{"wrong client", sign(keyA, rs, with(accessClaims(), "client_id", "otherclient")), false},
		{"missing client", sign(keyA, rs, with(accessClaims(), "client_id", nil)), false},
		{"expired", sign(keyA, rs, with(accessClaims(), "exp", now.Unix())), false},
		{"missing exp", sign(keyA, rs, with(accessClaims(), "exp", nil)), false},
		{"not yet valid", sign(keyA, rs, with(accessClaims(), "nbf", now.Add(time.Minute).Unix())), false},
		{"missing sub", sign(keyA, rs, with(accessClaims(), "sub", nil)), false},
		{"unpublished key", sign(keyB, map[string]any{"alg": "RS256", "kid": "b"}, accessClaims()), false},
		{"right kid, wrong key", sign(keyB, rs, accessClaims()), false},
		{"alg none", b64(map[string]any{"alg": "none", "kid": "a"}) + "." + b64(accessClaims()) + ".", false},
		{"alg HS256", hmacToken(), false},
		{"missing kid", sign(keyA, map[string]any{"alg": "RS256"}, accessClaims()), false},
		{"tampered claims", func() string {
			p := strings.Split(sign(keyA, rs, accessClaims()), ".")
			p[1] = b64(with(accessClaims(), "sub", "attacker"))
			return strings.Join(p, ".")
		}(), false},
		{"two segments", "a.b", false},
		{"garbage", "not-a-token", false},
		{"oversized", strings.Repeat("a", maxTokenBytes+1), false},
	}

	var fetches atomic.Int32
	v := newVerifier(t, &fetches)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := v.Verify(context.Background(), tt.token)
			if tt.ok {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if c.Subject != "owner-sub" || c.ClientID != testClient || !c.HasScope("s3mail/mail.read") {
					t.Fatalf("claims = %+v", c)
				}
				return
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestHasScopeIsExact(t *testing.T) {
	c := Claims{Scopes: []string{"s3mail/mail.read", "other/mail.write"}}
	for scope, want := range map[string]bool{
		"s3mail/mail.read":  true,
		"s3mail/mail.write": false, // only granted under another resource server
		"mail.read":         false, // suffix match must not count
		"s3mail/mail":       false,
	} {
		if got := c.HasScope(scope); got != want {
			t.Errorf("HasScope(%q) = %v, want %v", scope, got, want)
		}
	}
}

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	var fetches atomic.Int32
	v := newVerifier(t, &fetches)
	ctx := context.Background()

	if _, err := v.Verify(ctx, sign(keyA, map[string]any{"alg": "RS256", "kid": "a"}, accessClaims())); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		_, _ = v.Verify(ctx, sign(keyB, map[string]any{"alg": "RS256", "kid": "random"}, accessClaims()))
	}
	if n := fetches.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 within the rate-limit window", n)
	}
}

func TestBearerToken(t *testing.T) {
	for in, ok := range map[string]bool{
		"Bearer abc.def.ghi":  true,
		"bearer abc.def.ghi":  true,
		"Basic dXNlcjpwYXNz":  false,
		"Bearer":              false,
		"Bearer ":             false,
		"abc.def.ghi":         false,
		"Bearer a b":          false,
		"Token abc.def.ghi":   false,
		"Bearer\tabc.def.ghi": false,
	} {
		_, err := BearerToken(in)
		if (err == nil) != ok {
			t.Errorf("BearerToken(%q) err = %v, want ok=%v", in, err, ok)
		}
	}
}
