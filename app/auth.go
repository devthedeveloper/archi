package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// appAuth signs GitHub App JWTs and caches installation tokens. The private
// key and every JWT stay inside this file — nothing here is ever logged.
type appAuth struct {
	appID string // numeric App ID as a string (JWT iss)
	key   *rsa.PrivateKey
	base  string // "https://api.github.com"; httptest server in tests
	http  *http.Client
	now   func() time.Time
	mu    sync.Mutex
	cache map[int64]instToken // installation id → token
}

// instToken is one cached installation token.
type instToken struct {
	token   string
	expires time.Time // from the API response (≈1h out)
}

// loadPrivateKey decodes the PEM block and parses PKCS#1, falling back to
// PKCS#8 — GitHub issues PKCS#1 today; PKCS#8 covers re-exported keys.
// Non-RSA PKCS#8 keys are an error.
func loadPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: not PKCS#1 or PKCS#8: %v", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key: PKCS#8 key is %T, want RSA", parsed)
	}
	return key, nil
}

// appJWT builds a 9-minute App JWT:
//
//	header = {"alg":"RS256","typ":"JWT"}
//	claims = {"iat":<unix(now-60s)>,"exp":<unix(now+540s)>,"iss":"<appID>"}
//	jwt    = b64url(header) + "." + b64url(claims) + "." + b64url(RS256 sig)
//
// iat is backdated 60s to absorb clock skew; exp is 9m (GitHub max 10m).
func (a *appAuth) appJWT() (string, error) {
	now := a.now()
	header := `{"alg":"RS256","typ":"JWT"}`
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Add(-60*time.Second).Unix(), now.Add(540*time.Second).Unix(), a.appID)
	input := b64url([]byte(header)) + "." + b64url([]byte(claims))
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing app JWT: %v", err)
	}
	return input + "." + b64url(sig), nil
}

// b64url is unpadded URL-alphabet base64, the JWT segment encoding.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// installationToken returns a cached token for the installation, minting a
// new one when absent or when less than 5 minutes of validity remain.
// Single-flight per process under mu — coarse, but minting is rare.
func (a *appAuth) installationToken(ctx context.Context, installID int64) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.cache[installID]; ok && a.now().Add(5*time.Minute).Before(t.expires) {
		return t.token, nil
	}
	jwt, err := a.appJWT()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", a.base, installID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("minting installation token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("minting installation token: status %d", resp.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("minting installation token: %v", err)
	}
	expires, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("minting installation token: bad expires_at %q", body.ExpiresAt)
	}
	if a.cache == nil {
		a.cache = map[int64]instToken{}
	}
	a.cache[installID] = instToken{token: body.Token, expires: expires}
	return body.Token, nil
}

// dropInstallation forgets a cached token (installation deleted events).
func (a *appAuth) dropInstallation(installID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cache, installID)
}
