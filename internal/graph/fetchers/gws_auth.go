package fetchers

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
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// gwsScopes are the read-only scopes the service account requests.
const gwsScopes = "https://www.googleapis.com/auth/drive.readonly https://www.googleapis.com/auth/documents.readonly"

// gwsTokenProvider yields a valid Google access token, refreshing as needed.
// Implemented by both the service-account (JWT-bearer) and the OAuth user
// (refresh-token) sources.
type gwsTokenProvider interface {
	Token(ctx context.Context) (string, error)
}

const gwsOAuthTokenURI = "https://oauth2.googleapis.com/token"

// gwsOAuthTokenSource mints and caches Google access tokens from an installed-app
// OAuth client + a long-lived refresh token (the credentials the `gws` CLI already
// holds for the logged-in user). Durable and reuses the user's existing doc access,
// so no service account or domain-wide delegation is needed. Safe for concurrent use.
type gwsOAuthTokenSource struct {
	clientID     string
	clientSecret string
	refreshToken string
	client       *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

func newGWSOAuthTokenSource(clientID, clientSecret, refreshToken string, client *http.Client) *gwsOAuthTokenSource {
	if client == nil {
		client = http.DefaultClient
	}
	return &gwsOAuthTokenSource{clientID: clientID, clientSecret: clientSecret, refreshToken: refreshToken, client: client}
}

func (ts *gwsOAuthTokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.token != "" && time.Now().Before(ts.exp.Add(-60*time.Second)) {
		return ts.token, nil
	}

	form := url.Values{}
	form.Set("client_id", ts.clientID)
	form.Set("client_secret", ts.clientSecret)
	form.Set("refresh_token", ts.refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gwsOAuthTokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("gws oauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ts.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gws oauth: refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gws oauth: refresh status %d: %s", resp.StatusCode, string(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("gws oauth: decode: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("gws oauth: empty access_token in response")
	}
	ts.token = tok.AccessToken
	ts.exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return ts.token, nil
}

// gwsServiceAccount is the relevant slice of a Google service-account key JSON.
type gwsServiceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// gwsTokenSource mints and caches Google access tokens from a service-account key
// via the JWT-bearer grant (RS256-signed assertion exchanged at the token URI).
// Tokens are cached until shortly before expiry and refreshed on demand; safe for
// concurrent use.
type gwsTokenSource struct {
	keyPath string
	subject string // optional user to impersonate (domain-wide delegation)
	client  *http.Client

	mu    sync.Mutex
	key   *rsa.PrivateKey
	email string
	tURI  string
	token string
	exp   time.Time
}

func newGWSTokenSource(keyPath, subject string, client *http.Client) *gwsTokenSource {
	if client == nil {
		client = http.DefaultClient
	}
	return &gwsTokenSource{keyPath: keyPath, subject: subject, client: client}
}

// Token returns a valid access token, minting a new one if the cache is empty or
// within 60s of expiry.
func (ts *gwsTokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.token != "" && time.Now().Before(ts.exp.Add(-60*time.Second)) {
		return ts.token, nil
	}
	if ts.key == nil {
		if err := ts.loadKey(); err != nil {
			return "", err
		}
	}

	now := time.Now()
	assertion, err := ts.signAssertion(now)
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.tURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("gws token: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ts.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gws token: exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("gws token: exchange status %d: %s", resp.StatusCode, string(body))
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("gws token: decode: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("gws token: empty access_token in response")
	}
	ts.token = tok.AccessToken
	ts.exp = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	return ts.token, nil
}

// loadKey reads and parses the service-account key file into a usable RSA key.
func (ts *gwsTokenSource) loadKey() error {
	raw, err := os.ReadFile(ts.keyPath)
	if err != nil {
		return fmt.Errorf("gws token: read key file %q: %w", ts.keyPath, err)
	}
	var sa gwsServiceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return fmt.Errorf("gws token: parse key JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return fmt.Errorf("gws token: key JSON missing client_email or private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return fmt.Errorf("gws token: private_key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("gws token: parse PKCS8 key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return fmt.Errorf("gws token: key is not an RSA private key")
	}
	ts.key = rsaKey
	ts.email = sa.ClientEmail
	ts.tURI = sa.TokenURI
	if ts.tURI == "" {
		ts.tURI = "https://oauth2.googleapis.com/token"
	}
	return nil
}

// signAssertion builds and RS256-signs the JWT assertion for the token exchange.
func (ts *gwsTokenSource) signAssertion(now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))

	claims := map[string]any{
		"iss":   ts.email,
		"scope": gwsScopes,
		"aud":   ts.tURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	if ts.subject != "" {
		claims["sub"] = ts.subject // domain-wide delegation: act as this user
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("gws token: marshal claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)

	signingInput := header + "." + payload
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, ts.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("gws token: sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
