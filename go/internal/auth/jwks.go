package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	maxJWKSBytes = 64 * 1024
	// minRefresh rate-limits JWKS fetches triggered by unknown key IDs, so
	// tokens with random kids cannot make every request call Cognito.
	minRefresh = time.Minute
)

// JWKS fetches and caches the issuer's signing keys
// (<issuer>/.well-known/jwks.json for Cognito).
type JWKS struct {
	URL    string
	Client *http.Client     // nil means a client with a 5 s timeout
	Now    func() time.Time // nil means time.Now

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// Key returns the cached key for kid, refreshing the set at most once per
// minRefresh when kid is unknown.
func (j *JWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	now := time.Now
	if j.Now != nil {
		now = j.Now
	}
	if !j.fetched.IsZero() && now().Sub(j.fetched) < minRefresh {
		return nil, errors.New("unknown key ID")
	}
	j.fetched = now()
	keys, err := j.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	j.keys = keys
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown key ID")
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j *JWKS) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	client := j.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJWKSBytes {
		return nil, errors.New("JWKS too large")
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable RS256 keys")
	}
	return keys, nil
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31 {
		return nil, errors.New("bad exponent")
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exp.Int64())}
	if pub.N.BitLen() < 2048 {
		return nil, errors.New("key too small")
	}
	return pub, nil
}
