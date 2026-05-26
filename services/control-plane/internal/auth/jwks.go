// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JWKSCache fetches an OIDC issuer's public keys and caches them with
// TTL refresh. Production-grade interceptor uses this to verify the
// signature on incoming JWTs.
//
// Discovery: we GET /.well-known/openid-configuration on the issuer,
// read jwks_uri, then GET that. Cache TTL defaults to 1h; the cache
// also lazy-refreshes on a `kid` miss so a rotated key doesn't lock
// users out.
type JWKSCache struct {
	Issuer     string
	HTTPClient *http.Client
	TTL        time.Duration

	mu        sync.RWMutex
	keys      map[string]any   // kid → *rsa.PublicKey or *ecdsa.PublicKey
	loadedAt  time.Time
	jwksURL   string
}

// NewJWKSCache constructs an empty cache. Call Get(ctx, kid) to fetch
// + cache lazily.
func NewJWKSCache(issuer string) *JWKSCache {
	return &JWKSCache{
		Issuer:     issuer,
		HTTPClient: &http.Client{Timeout: 8 * time.Second},
		TTL:        time.Hour,
		keys:       map[string]any{},
	}
}

// Get returns the public key for `kid`. On cache miss or expiry, the
// cache refreshes against the issuer's jwks_uri.
func (c *JWKSCache) Get(ctx context.Context, kid string) (any, error) {
	c.mu.RLock()
	if k, ok := c.keys[kid]; ok && time.Since(c.loadedAt) < c.TTL {
		c.mu.RUnlock()
		return k, nil
	}
	c.mu.RUnlock()

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("kid %q not in JWKS for %s", kid, c.Issuer)
}

func (c *JWKSCache) refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.jwksURL == "" {
		url, err := c.discoverJWKSURL(ctx)
		if err != nil {
			return fmt.Errorf("discover jwks_uri: %w", err)
		}
		c.jwksURL = url
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks http %d", resp.StatusCode)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks decode: %w", err)
	}
	keys := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		pub, err := k.publicKey()
		if err != nil {
			continue // skip unsupported key types (oct, etc.)
		}
		if k.Kid != "" {
			keys[k.Kid] = pub
		}
	}
	c.keys = keys
	c.loadedAt = time.Now()
	return nil
}

func (c *JWKSCache) discoverJWKSURL(ctx context.Context) (string, error) {
	u := strings.TrimRight(c.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery http %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("oidc discovery: jwks_uri missing")
	}
	return doc.JWKSURI, nil
}

// jwk is a single key as it appears in a JWKS document. We support
// RSA and EC P-256 — covers Auth0, Okta, Dex, Google, AWS Cognito,
// Azure AD, WorkOS. Symmetric `oct` keys aren't supported because no
// reasonable OIDC IdP emits HMAC-signed tokens for verification by
// public RPs.
type jwk struct {
	Kty string `json:"kty"` // RSA | EC
	Use string `json:"use"` // "sig"
	Alg string `json:"alg"` // RS256, ES256, …
	Kid string `json:"kid"`
	N   string `json:"n,omitempty"` // RSA modulus (base64url)
	E   string `json:"e,omitempty"` // RSA exponent
	Crv string `json:"crv,omitempty"` // EC: P-256 etc.
	X   string `json:"x,omitempty"`   // EC X
	Y   string `json:"y,omitempty"`   // EC Y
	X5c []string `json:"x5c,omitempty"` // optional cert chain (DER base64)
}

func (k jwk) publicKey() (any, error) {
	if len(k.X5c) > 0 {
		der, err := base64.StdEncoding.DecodeString(k.X5c[0])
		if err == nil {
			cert, err := x509.ParseCertificate(der)
			if err == nil {
				return cert.PublicKey, nil
			}
		}
		// fall through to component-based decoding
	}
	switch k.Kty {
	case "RSA":
		n, err := decodeBigInt(k.N)
		if err != nil {
			return nil, fmt.Errorf("rsa.n: %w", err)
		}
		e, err := decodeBigInt(k.E)
		if err != nil {
			return nil, fmt.Errorf("rsa.e: %w", err)
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		curve, err := ecCurve(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := decodeBigInt(k.X)
		if err != nil {
			return nil, fmt.Errorf("ec.x: %w", err)
		}
		y, err := decodeBigInt(k.Y)
		if err != nil {
			return nil, fmt.Errorf("ec.y: %w", err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	}
	return nil, fmt.Errorf("unsupported kty %q", k.Kty)
}

func decodeBigInt(b64 string) (*big.Int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		// Try standard base64 too (some IdPs pad).
		raw, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, err
		}
	}
	return new(big.Int).SetBytes(raw), nil
}

func ecCurve(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	}
	return nil, fmt.Errorf("unsupported EC curve %q", crv)
}

// pemEncode is here so callers can dump the cached key for debugging
// (`kubehero auth jwks --kid <id>` will use this once we wire it).
func pemEncode(pub any) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
