// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// looksLikeJWT does a cheap structural check (three base64url segments
// separated by dots). It avoids the full JWKS fetch when an API-key
// token is also presented.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	return len(parts) == 3 && len(parts[0]) > 0 && len(parts[1]) > 0
}

// decodeJWTPresence performs an unsigned decode and validates `iss` +
// `aud`. Used only as the dev fallback when the JWKS cache is nil.
// **Strictly** a presence + audience check — NEVER a substitute for
// the cryptographic verification in verifyJWT.
func decodeJWTPresence(token, issuer, audience string, groupRoles map[string]Role) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, errors.New("malformed JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, errors.New("JWT payload not base64url")
	}
	var claims struct {
		Iss    string   `json:"iss"`
		Sub    string   `json:"sub"`
		Email  string   `json:"email"`
		Aud    any      `json:"aud"`
		Roles  []string `json:"kh_roles,omitempty"`
		Groups []string `json:"groups,omitempty"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return Principal{}, errors.New("JWT payload not JSON")
	}
	if claims.Iss != issuer {
		return Principal{}, errors.New("issuer mismatch")
	}
	if audience != "" && !audMatches(claims.Aud, audience) {
		return Principal{}, errors.New("audience mismatch")
	}

	role := ResolveGroupRole(claims.Groups, groupRoles)
	if role == "" {
		for _, r := range claims.Roles {
			if strings.EqualFold(r, "admin") {
				role = RoleAdmin
			}
		}
	}
	if role == "" {
		role = RoleViewer
	}
	return Principal{
		Sub:    firstNonEmpty(claims.Sub, shortHash(token)),
		Role:   role,
		Email:  claims.Email,
		Groups: claims.Groups,
	}, nil
}

func audMatches(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shortHash hides the raw token from logs. We never log full tokens;
// the principal's Sub field is at most 16 hex chars.
func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}
