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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGWSTokenSource verifies the service-account JWT-bearer flow end to end:
// the assertion is a well-formed RS256 JWT that verifies against the key, the
// access token is returned, and a second call is served from cache (no re-mint).
func TestGWSTokenSource(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assertion := r.FormValue("assertion")
		parts := strings.Split(assertion, ".")
		if len(parts) != 3 {
			t.Errorf("assertion has %d parts, want 3", len(parts))
		}
		// Verify the RS256 signature over header.payload.
		signingInput := parts[0] + "." + parts[1]
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Errorf("decode sig: %v", err)
		}
		digest := sha256.Sum256([]byte(signingInput))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("signature does not verify: %v", err)
		}
		// Check key claims.
		claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		json.Unmarshal(claimsJSON, &claims)
		if claims["iss"] != "svc@proj.iam.gserviceaccount.com" {
			t.Errorf("iss = %v", claims["iss"])
		}
		if claims["sub"] != "user@wego.com" {
			t.Errorf("sub = %v (domain-wide delegation not applied)", claims["sub"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"ya29.test-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	saJSON, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "svc@proj.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    srv.URL,
	})
	keyPath := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(keyPath, saJSON, 0600); err != nil {
		t.Fatal(err)
	}

	ts := newGWSTokenSource(keyPath, "user@wego.com", srv.Client())

	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ya29.test-token" {
		t.Fatalf("token = %q", tok)
	}

	// Second call must come from cache (token still valid) — no new exchange.
	if _, err := ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("token exchanged %d times, want 1 (second call should be cached)", got)
	}
}
