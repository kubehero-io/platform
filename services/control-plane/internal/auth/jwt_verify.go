// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// verifyJWT decodes + cryptographically verifies a JWT against the
// issuer's JWKS. Validates iss, aud, exp, nbf, plus the signature.
//
// Returns a Principal stamped with sub + email + role on success.
// Role resolution priority (highest wins):
//   1. groups claim mapped via Config.GroupRoles
//   2. legacy `kh_roles` claim (admin only)
//   3. fail-closed default = viewer
//
// Replaces decodeJWTPresence as the production path; presence-only
// fallback is used only when JWKS is nil (dev mode).
func verifyJWT(
	ctx context.Context,
	token string,
	jwks *JWKSCache,
	issuer, audience string,
	groupRoles map[string]Role,
) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("malformed JWT")
	}
	signingInput := parts[0] + "." + parts[1]

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerRaw, &hdr); err != nil {
		return Principal{}, fmt.Errorf("jwt header decode: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt signature: %w", err)
	}

	pub, err := jwks.Get(ctx, hdr.Kid)
	if err != nil {
		return Principal{}, err
	}

	if err := verifySignature(hdr.Alg, []byte(signingInput), sig, pub); err != nil {
		return Principal{}, err
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, fmt.Errorf("jwt payload: %w", err)
	}
	var c struct {
		Iss    string   `json:"iss"`
		Sub    string   `json:"sub"`
		Email  string   `json:"email"`
		Aud    any      `json:"aud"`
		Exp    int64    `json:"exp"`
		Nbf    int64    `json:"nbf"`
		Roles  []string `json:"kh_roles,omitempty"` // legacy: explicit role claim
		Groups []string `json:"groups,omitempty"`   // standard OIDC group claim (Okta, Azure AD, Auth0, Keycloak, Google Workspace via custom claim)
	}
	if err := json.Unmarshal(payloadRaw, &c); err != nil {
		return Principal{}, fmt.Errorf("jwt payload decode: %w", err)
	}
	if c.Iss != issuer {
		return Principal{}, errors.New("issuer mismatch")
	}
	if audience != "" && !audMatches(c.Aud, audience) {
		return Principal{}, errors.New("audience mismatch")
	}
	now := time.Now().Unix()
	if c.Exp > 0 && now > c.Exp {
		return Principal{}, errors.New("token expired")
	}
	if c.Nbf > 0 && now < c.Nbf {
		return Principal{}, errors.New("token not yet valid")
	}

	// Group → role mapping is the production path. We fall back to
	// the legacy `kh_roles` claim only when the IdP doesn't emit
	// groups, and to the fail-closed viewer default beyond that.
	role := ResolveGroupRole(c.Groups, groupRoles)
	if role == "" {
		for _, r := range c.Roles {
			if strings.EqualFold(r, "admin") {
				role = RoleAdmin
			}
		}
	}
	if role == "" {
		role = RoleViewer
	}
	return Principal{
		Sub:    firstNonEmpty(c.Sub, shortHash(token)),
		Role:   role,
		Email:  c.Email,
		Groups: c.Groups,
	}, nil
}

func verifySignature(alg string, signingInput, sig []byte, pub any) error {
	switch alg {
	case "RS256":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS256 expects RSA key")
		}
		hashed := sha256.Sum256(signingInput)
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, hashed[:], sig)
	case "RS384":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS384 expects RSA key")
		}
		hashed := sha512.Sum384(signingInput)
		return rsa.VerifyPKCS1v15(k, crypto.SHA384, hashed[:], sig)
	case "RS512":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS512 expects RSA key")
		}
		hashed := sha512.Sum512(signingInput)
		return rsa.VerifyPKCS1v15(k, crypto.SHA512, hashed[:], sig)
	case "ES256":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("ES256 expects ECDSA key")
		}
		hashed := sha256.Sum256(signingInput)
		return verifyECDSA(k, hashed[:], sig, 32)
	case "ES384":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("ES384 expects ECDSA key")
		}
		hashed := sha512.Sum384(signingInput)
		return verifyECDSA(k, hashed[:], sig, 48)
	}
	return fmt.Errorf("unsupported alg %q", alg)
}

// verifyECDSA expects a "raw" R||S signature (the JWS shape). Go's
// stdlib ecdsa.Verify takes (r, s) big.Int pairs.
func verifyECDSA(k *ecdsa.PublicKey, hash, sig []byte, sz int) error {
	if len(sig) != 2*sz {
		return fmt.Errorf("ecdsa signature length %d, want %d", len(sig), 2*sz)
	}
	r := new(big.Int).SetBytes(sig[:sz])
	s := new(big.Int).SetBytes(sig[sz:])
	if !ecdsa.Verify(k, hash, r, s) {
		return errors.New("ecdsa signature invalid")
	}
	return nil
}
