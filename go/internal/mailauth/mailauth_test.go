package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testConfig(base string) Config {
	return Config{
		Issuer:           "https://cognito-idp.us-east-1.amazonaws.com/us-east-1_EXAMPLE",
		TerminalClientID: "terminalclient",
		LoginBaseURL:     base,
		Scopes:           []string{"s3mail/mail.read", "s3mail/mail.write", "s3mail/mail.submit"},
		CallbackURL:      "http://localhost:53789/callback",
	}
}

func TestS256Vector(t *testing.T) {
	// RFC 7636 Appendix B.
	if got := S256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256 = %s", got)
	}
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Verifier) != 43 || p.Challenge != S256(p.Verifier) {
		t.Fatalf("PKCE = %+v", p)
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := testConfig("https://s3mail-x.auth.us-east-1.amazoncognito.com")
	u, err := url.Parse(c.AuthorizeURL("st", PKCE{Verifier: "v", Challenge: "ch"}))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/oauth2/authorize" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != "ch" || q.Get("state") != "st" ||
		q.Get("scope") != "s3mail/mail.read s3mail/mail.write s3mail/mail.submit" ||
		q.Get("redirect_uri") != c.CallbackURL {
		t.Fatalf("authorize URL = %s", u)
	}
}

func TestRegion(t *testing.T) {
	r, err := testConfig("").Region()
	if err != nil || r != "us-east-1" {
		t.Fatalf("Region = %q, %v", r, err)
	}
	if _, err := (Config{Issuer: "https://evil.example.com/x"}).Region(); err == nil {
		t.Fatal("non-Cognito issuer accepted")
	}
}

func TestExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/oauth2/token" || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "abc" || r.Form.Get("code_verifier") != "ver" ||
			r.Form.Get("client_id") != "terminalclient" || r.Form.Get("client_secret") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at","refresh_token":"rt","expires_in":300,"token_type":"Bearer"}`)
	}))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	c := &Client{Config: testConfig(srv.URL), HTTP: srv.Client(), Now: func() time.Time { return now }}

	s, err := c.Exchange(context.Background(), "abc", "ver")
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "at" || s.RefreshToken != "rt" || !s.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("session = %+v", s)
	}
	if _, err := c.Exchange(context.Background(), "wrong", "ver"); err == nil {
		t.Fatal("bad code accepted")
	}
}

func TestRefreshRotatesAndDetectsRevocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.Header.Get("X-Amz-Target") != "AWSCognitoIdentityProviderService.GetTokensFromRefreshToken" ||
			in["ClientId"] != "terminalclient" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if in["RefreshToken"] == "revoked" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"NotAuthorizedException","message":"Refresh Token has been revoked"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"AuthenticationResult":{"AccessToken":"at2","RefreshToken":"rt2","ExpiresIn":300}}`)
	}))
	defer srv.Close()
	c := &Client{Config: testConfig(""), HTTP: srv.Client(), IDPEndpoint: srv.URL}

	s, err := c.Refresh(context.Background(), Session{RefreshToken: "rt1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessToken != "at2" || s.RefreshToken != "rt2" {
		t.Fatalf("session = %+v, want rotated refresh token", s)
	}
	if _, err := c.Refresh(context.Background(), Session{RefreshToken: "revoked"}); !errors.Is(err, ErrReauthenticate) {
		t.Fatalf("revoked refresh err = %v", err)
	}
}

func TestSessionValid(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := Session{AccessToken: "at", ExpiresAt: now.Add(90 * time.Second)}
	if !s.Valid(now, time.Minute) || s.Valid(now, 2*time.Minute) || (Session{ExpiresAt: s.ExpiresAt}).Valid(now, 0) {
		t.Fatal("Valid margin logic wrong")
	}
}

func TestLoopback(t *testing.T) {
	ln, cb, err := Listen("http://localhost:0/callback")
	if err == nil {
		_ = ln.Close()
		t.Fatal("port 0 accepted; Cognito needs a fixed port")
	}
	for _, bad := range []string{"https://localhost:53789/cb", "http://example.com:53789/cb", "http://localhost/cb"} {
		if _, _, err := Listen(bad); err == nil {
			t.Errorf("Listen(%q) accepted", bad)
		}
	}

	ln, cb, err = Listen("http://localhost:53788/callback")
	if err != nil {
		t.Skipf("loopback port unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Fatalf("bound %s, want loopback only", ln.Addr())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type res struct {
		code string
		err  error
	}
	got := make(chan res, 1)
	go func() {
		code, err := AwaitCode(ctx, ln, cb, "good-state")
		got <- res{code, err}
	}()

	base := "http://" + ln.Addr().String()
	get := func(path string) int {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// A forged redirect is rejected and does not end the wait.
	if code := get("/callback?state=forged&code=evil"); code != http.StatusBadRequest {
		t.Fatalf("forged state: status %d", code)
	}
	if code := get("/other?state=good-state&code=x"); code != http.StatusNotFound {
		t.Fatalf("other path: status %d", code)
	}
	select {
	case r := <-got:
		t.Fatalf("login ended on a forged redirect: %+v", r)
	default:
	}
	if code := get("/callback?state=good-state&code=real"); code != http.StatusOK {
		t.Fatalf("real redirect: status %d", code)
	}
	if r := <-got; r.err != nil || r.code != "real" {
		t.Fatalf("real redirect: code=%q err=%v", r.code, r.err)
	}
}
