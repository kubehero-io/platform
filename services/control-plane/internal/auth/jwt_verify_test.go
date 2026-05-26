// SPDX-License-Identifier: BUSL-1.1
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signRS256 signs `signingInput` with `priv` and returns the
// base64url-encoded signature ready to splice as the third JWT segment.
func signRS256(t *testing.T, priv *rsa.PrivateKey, signingInput string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

// jwksFromRSA emits a minimal JWKS document for `priv.Public()` under
// the given kid. Mimics what an IdP's `jwks_uri` returns. The standard
// public exponent 65537 encodes as the literal "AQAB" (0x010001 in
// base64url) — every real-world RSA key uses this so we hardcode it.
func jwksFromRSA(priv *rsa.PrivateKey, kid string) string {
	pub := priv.Public().(*rsa.PublicKey)
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := "AQAB" // exponent 65537
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","alg":"RS256","kid":%q,"n":%q,"e":%q}]}`, kid, n, e)
}

func TestVerifyJWTHappyPath(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const kid = "test-key-1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			fmt.Fprintf(w, `{"jwks_uri":"%s/jwks"}`, "http://"+r.Host)
		case "/jwks":
			fmt.Fprint(w, jwksFromRSA(priv, kid))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	header := map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"}
	payload := map[string]any{
		"iss":      srv.URL,
		"aud":      "kubehero",
		"sub":      "user-42",
		"email":    "alice@example.com",
		"exp":      time.Now().Add(5 * time.Minute).Unix(),
		"kh_roles": []string{"admin"},
	}
	hdrJSON, _ := json.Marshal(header)
	plJSON, _ := json.Marshal(payload)
	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	plB64 := base64.RawURLEncoding.EncodeToString(plJSON)
	signingInput := hdrB64 + "." + plB64
	sigB64 := signRS256(t, priv, signingInput)
	token := signingInput + "." + sigB64

	cache := NewJWKSCache(srv.URL)
	p, err := verifyJWT(context.Background(), token, cache, srv.URL, "kubehero", nil)
	if err != nil {
		t.Fatalf("verifyJWT: %v", err)
	}
	if p.Sub != "user-42" {
		t.Errorf("sub = %q, want user-42", p.Sub)
	}
	if p.Email != "alice@example.com" {
		t.Errorf("email = %q", p.Email)
	}
	if p.Role != RoleAdmin {
		t.Errorf("role = %q, want admin", p.Role)
	}
}

func TestVerifyJWTRejectsTamperedPayload(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	const kid = "k"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			fmt.Fprintf(w, `{"jwks_uri":"%s/jwks"}`, "http://"+r.Host)
		case "/jwks":
			fmt.Fprint(w, jwksFromRSA(priv, kid))
		}
	}))
	defer srv.Close()

	header := map[string]any{"alg": "RS256", "kid": kid}
	payload := map[string]any{"iss": srv.URL, "aud": "k", "exp": time.Now().Add(time.Minute).Unix()}
	hdrJSON, _ := json.Marshal(header)
	plJSON, _ := json.Marshal(payload)
	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	plB64 := base64.RawURLEncoding.EncodeToString(plJSON)
	signingInput := hdrB64 + "." + plB64
	sigB64 := signRS256(t, priv, signingInput)

	// Swap the payload AFTER signing — the signature should now fail.
	tampered := map[string]any{"iss": srv.URL, "aud": "k", "exp": 9999999999, "kh_roles": []string{"admin"}}
	tJSON, _ := json.Marshal(tampered)
	plB64Tampered := base64.RawURLEncoding.EncodeToString(tJSON)
	bad := hdrB64 + "." + plB64Tampered + "." + sigB64

	cache := NewJWKSCache(srv.URL)
	_, err := verifyJWT(context.Background(), bad, cache, srv.URL, "k", nil)
	if err == nil {
		t.Fatal("expected verification to fail on tampered payload")
	}
}

func TestVerifyJWTRejectsExpired(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	const kid = "k"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			fmt.Fprintf(w, `{"jwks_uri":"%s/jwks"}`, "http://"+r.Host)
		case "/jwks":
			fmt.Fprint(w, jwksFromRSA(priv, kid))
		}
	}))
	defer srv.Close()

	header := map[string]any{"alg": "RS256", "kid": kid}
	payload := map[string]any{
		"iss": srv.URL, "aud": "k",
		"exp": time.Now().Add(-time.Minute).Unix(),
	}
	hdrJSON, _ := json.Marshal(header)
	plJSON, _ := json.Marshal(payload)
	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	plB64 := base64.RawURLEncoding.EncodeToString(plJSON)
	signingInput := hdrB64 + "." + plB64
	sigB64 := signRS256(t, priv, signingInput)
	token := signingInput + "." + sigB64

	cache := NewJWKSCache(srv.URL)
	_, err := verifyJWT(context.Background(), token, cache, srv.URL, "k", nil)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiration error, got %v", err)
	}
}

func TestECDSAVerifyRoundTrip(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := []byte("a.b")
	hashed := sha256.Sum256(signingInput)
	r, s, err := ecdsa.Sign(rand.Reader, priv, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	// Pack into raw R||S.
	sig := make([]byte, 64)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	if err := verifySignature("ES256", signingInput, sig, &priv.PublicKey); err != nil {
		t.Fatalf("ES256 verify failed: %v", err)
	}
}
