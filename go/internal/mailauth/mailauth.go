// Package mailauth implements the terminal OAuth helper
// (docs/specs/security.md#terminal-login): authorization code + PKCE through
// the system browser and a loopback redirect, refresh credentials in the OS
// keychain, and a fresh access token on demand for clients such as aerc.
package mailauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config holds the public values from the Terraform "auth" output.
type Config struct {
	Issuer           string   `json:"issuer"`
	TerminalClientID string   `json:"terminal_client_id"`
	LoginBaseURL     string   `json:"login_base_url"`
	Scopes           []string `json:"scopes"`
	CallbackURL      string   `json:"callback_url"`
}

// LoadConfig reads a JSON file produced by `terraform output -json auth`.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Issuer == "" || c.TerminalClientID == "" || c.LoginBaseURL == "" || c.CallbackURL == "" || len(c.Scopes) == 0 {
		return Config{}, fmt.Errorf("%s: missing issuer, terminal_client_id, login_base_url, callback_url or scopes", path)
	}
	return c, nil
}

// Region is parsed from the Cognito issuer URL.
func (c Config) Region() (string, error) {
	m := regexp.MustCompile(`^https://cognito-idp\.([a-z0-9-]+)\.amazonaws\.com/`).FindStringSubmatch(c.Issuer)
	if m == nil {
		return "", fmt.Errorf("issuer %q is not a Cognito user pool URL", c.Issuer)
	}
	return m[1], nil
}

// Session is what the keychain stores.
type Session struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid reports whether the cached access token lasts at least margin.
func (s Session) Valid(now time.Time, margin time.Duration) bool {
	return s.AccessToken != "" && now.Add(margin).Before(s.ExpiresAt)
}

// PKCE is an RFC 7636 S256 verifier/challenge pair.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE returns a 43-character verifier and its S256 challenge.
func NewPKCE() (PKCE, error) {
	v, err := randomString(32)
	if err != nil {
		return PKCE{}, err
	}
	return PKCE{Verifier: v, Challenge: S256(v)}, nil
}

// S256 is BASE64URL(SHA256(verifier)) without padding.
func S256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AuthorizeURL builds the managed login authorization request.
func (c Config) AuthorizeURL(state string, p PKCE) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.TerminalClientID},
		"redirect_uri":          {c.CallbackURL},
		"scope":                 {strings.Join(c.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {"S256"},
	}
	return strings.TrimSuffix(c.LoginBaseURL, "/") + "/oauth2/authorize?" + q.Encode()
}

// Client talks to the Cognito token endpoints.
type Client struct {
	Config Config
	HTTP   *http.Client     // nil means a 15 s timeout client
	Now    func() time.Time // nil means time.Now
	// IDPEndpoint overrides https://cognito-idp.<region>.amazonaws.com/ in tests.
	IDPEndpoint string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Exchange trades an authorization code for tokens.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (Session, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.Config.TerminalClientID},
		"code":          {code},
		"redirect_uri":  {c.Config.CallbackURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.Config.LoginBaseURL, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	status, err := c.doJSON(req, &out)
	if err != nil {
		return Session{}, err
	}
	if status != http.StatusOK || out.AccessToken == "" || out.RefreshToken == "" {
		return Session{}, fmt.Errorf("token endpoint: status %d %s", status, out.Error)
	}
	return Session{
		RefreshToken: out.RefreshToken,
		AccessToken:  out.AccessToken,
		ExpiresAt:    c.now().Add(time.Duration(out.ExpiresIn) * time.Second),
	}, nil
}

// ErrReauthenticate means the refresh token is no longer usable; run login.
var ErrReauthenticate = errors.New("session expired or revoked; run mail-auth login")

// Refresh uses GetTokensFromRefreshToken, which the client must use
// because refresh token rotation is enabled. The returned session carries
// the rotated refresh token.
func (c *Client) Refresh(ctx context.Context, s Session) (Session, error) {
	endpoint := c.IDPEndpoint
	if endpoint == "" {
		region, err := c.Config.Region()
		if err != nil {
			return Session{}, err
		}
		endpoint = "https://cognito-idp." + region + ".amazonaws.com/"
	}
	body, _ := json.Marshal(map[string]string{
		"RefreshToken": s.RefreshToken,
		"ClientId":     c.Config.TerminalClientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService.GetTokensFromRefreshToken")
	var out struct {
		AuthenticationResult struct {
			AccessToken  string `json:"AccessToken"`
			RefreshToken string `json:"RefreshToken"`
			ExpiresIn    int    `json:"ExpiresIn"`
		} `json:"AuthenticationResult"`
		Type string `json:"__type"`
	}
	status, err := c.doJSON(req, &out)
	if err != nil {
		return Session{}, err
	}
	if status != http.StatusOK {
		if strings.Contains(out.Type, "NotAuthorizedException") {
			return Session{}, ErrReauthenticate
		}
		return Session{}, fmt.Errorf("refresh: status %d %s", status, out.Type)
	}
	r := out.AuthenticationResult
	if r.AccessToken == "" {
		return Session{}, errors.New("refresh: no access token in response")
	}
	next := Session{
		RefreshToken: s.RefreshToken,
		AccessToken:  r.AccessToken,
		ExpiresAt:    c.now().Add(time.Duration(r.ExpiresIn) * time.Second),
	}
	if r.RefreshToken != "" {
		next.RefreshToken = r.RefreshToken
	}
	return next, nil
}

// Revoke invalidates the refresh token and the access tokens issued from it.
func (c *Client) Revoke(ctx context.Context, refreshToken string) error {
	form := url.Values{"token": {refreshToken}, "client_id": {c.Config.TerminalClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.Config.LoginBaseURL, "/")+"/oauth2/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoke: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) doJSON(req *http.Request, out any) (int, error) {
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, fmt.Errorf("status %d: response is not JSON", resp.StatusCode)
		}
	}
	return resp.StatusCode, nil
}
