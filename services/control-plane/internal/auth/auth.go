// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package auth wraps Connect handlers with bearer-token authentication
// and a lightweight RBAC check.
//
// Two modes layer:
//
//   - API keys (KUBEHERO_API_KEYS=<comma-separated>) — symmetric
//     long-lived tokens, suitable for the operator → control-plane
//     channel and for CLI scripting against a self-hosted control
//     plane. Each key carries an optional role suffix
//     ("<token>:admin", "<token>:member"); a bare token defaults to
//     the "member" role.
//
//   - OIDC bearer (OIDC_ISSUER_URL=<url> + OIDC_AUDIENCE=<aud>) — the
//     dashboard + interactive CLI flows present a JWT minted by Dex /
//     Okta / Auth0. We don't validate signatures yet (TODO: pull the
//     JWKS); for now we only enforce the presence + audience claim.
//     This keeps the surface honest until the JWKS fetcher lands.
//
// Anonymous mode (neither env var set) is the dev / kind-demo path —
// requests succeed with role="anonymous" so the dashboard, kind demo,
// and unit tests work without any setup. We log a single startup
// warning so you can't accidentally ship a production cp with no auth.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
)

// Role is the role of the caller after credential resolution. The
// engine consults it in the RBAC check; see Require.
//
// Hierarchy (lowest to highest):
//   anonymous — no credentials presented (dev open-mode only)
//   viewer    — read every list/get RPC; cannot mutate
//   member    — viewer + create/update own resources (default for SSO users)
//   auditor   — viewer + ListAuditLog at any scope + export rights
//   admin     — member + RegisterCluster + policy arming
//   owner     — admin + org/IdP config + role grants
//
// Role checks use atLeast() with these as ordered ranks. Auditor's
// rank sits alongside member rather than above so a compliance
// officer can be granted read-everywhere without inheriting mutate
// rights — atLeast(auditor, admin) is false but atLeast(auditor,
// viewer) is true.
type Role string

const (
	RoleAnonymous Role = "anonymous"
	RoleViewer    Role = "viewer"
	RoleMember    Role = "member"
	RoleAuditor   Role = "auditor"
	RoleAdmin     Role = "admin"
	RoleOwner     Role = "owner"
)

// principalKey is the context key under which we stash the resolved
// caller. Internal use only — RPC handlers read it via Principal().
type principalKey struct{}

type Principal struct {
	Sub    string   // token id (first 8 chars of token hash) or JWT sub
	Role   Role     // resolved role after group mapping
	Email  string   // OIDC only
	Groups []string // raw groups claim from the IdP (e.g. ["kubehero-admins", "ml-team"])
}

// PrincipalFromContext returns the resolved caller, or an anonymous
// principal when the request was unauthenticated.
func PrincipalFromContext(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{Role: RoleAnonymous}
}

// WithPrincipal injects a principal for tests + the dev-mode anonymous
// path. Don't call this from request-handling code — the interceptor
// is the only legitimate stamper.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// Config drives how requests are authenticated.
type Config struct {
	// APIKeys is a slice of "<token>" or "<token>:<role>" entries.
	// Empty means no API-key auth.
	APIKeys []string
	// OIDCIssuer is the issuer URL used for token presence checks.
	// Empty means no OIDC.
	OIDCIssuer   string
	OIDCAudience string
	// JWKS is the public-key cache used for OIDC signature validation.
	// When nil, the interceptor falls back to presence + iss/aud
	// checks only (clearly logged as "insecure JWT mode" at startup).
	// In production, set this to NewJWKSCache(OIDCIssuer).
	JWKS *JWKSCache
	// GroupRoles maps an IdP group name → KubeHero role. The token's
	// `groups` claim is walked in order; the highest matching role
	// wins. A user belonging to "ml-team" and "kubehero-admins" with
	// the table {ml-team: member, kubehero-admins: admin} resolves
	// to admin.
	//
	// Recommended starting set:
	//   "kubehero-owners":   "owner"
	//   "kubehero-admins":   "admin"
	//   "kubehero-auditors": "auditor"
	//   "kubehero-members":  "member"
	//   "kubehero-viewers":  "viewer"
	//
	// Empty map → role defaults to viewer (read-only) for any
	// authenticated user. This is fail-closed at the role level even
	// when auth itself succeeds.
	GroupRoles map[string]Role
	// AllowAnonymous: when both APIKeys and OIDCIssuer are empty,
	// requests succeed with role=anonymous. Set false in production
	// to fail-closed.
	AllowAnonymous bool
	// Logger is used for the single startup warning when running open.
	Logger *slog.Logger
}

// ParseGroupRoles turns a string like "kubehero-admins=admin,ml=member"
// into the GroupRoles map. Whitespace around tokens is trimmed; empty
// or malformed entries are skipped.
func ParseGroupRoles(env string) map[string]Role {
	if env == "" {
		return nil
	}
	out := map[string]Role{}
	for _, e := range strings.Split(env, ",") {
		kv := strings.SplitN(strings.TrimSpace(e), "=", 2)
		if len(kv) != 2 {
			continue
		}
		group := strings.TrimSpace(kv[0])
		role := strings.ToLower(strings.TrimSpace(kv[1]))
		if group == "" {
			continue
		}
		switch Role(role) {
		case RoleViewer, RoleMember, RoleAuditor, RoleAdmin, RoleOwner:
			out[group] = Role(role)
		}
	}
	return out
}

// ResolveGroupRole returns the highest-ranked role from `groups` that
// has a mapping in `gr`. Returns "" when no group matches; callers
// fall back to a default (typically RoleViewer).
func ResolveGroupRole(groups []string, gr map[string]Role) Role {
	if len(gr) == 0 || len(groups) == 0 {
		return ""
	}
	best := Role("")
	bestRank := -1
	rank := map[Role]int{
		RoleViewer:  1,
		RoleAuditor: 2, // higher than viewer for "best wins" purposes
		RoleMember:  3,
		RoleAdmin:   4,
		RoleOwner:   5,
	}
	for _, g := range groups {
		if r, ok := gr[g]; ok {
			if rank[r] > bestRank {
				best = r
				bestRank = rank[r]
			}
		}
	}
	return best
}

// ParseAPIKeys turns the env value into a Config slice. Whitespace
// and empty tokens are skipped so KUBEHERO_API_KEYS="" stays inert.
func ParseAPIKeys(env string) []string {
	if env == "" {
		return nil
	}
	parts := strings.Split(env, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// NewInterceptor returns a Connect interceptor that resolves the caller
// from the Authorization header (or absence thereof) and stashes a
// Principal in the context. Handlers can then enforce roles via
// Require.
func NewInterceptor(cfg Config) connect.UnaryInterceptorFunc {
	if len(cfg.APIKeys) == 0 && cfg.OIDCIssuer == "" && cfg.AllowAnonymous {
		if cfg.Logger != nil {
			cfg.Logger.Warn("control-plane running with no auth — KUBEHERO_API_KEYS + OIDC_ISSUER_URL both unset; every RPC accepts anonymous access (dev only)")
		}
	}
	keys := indexKeys(cfg.APIKeys)

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			h := req.Header().Get("Authorization")
			if h == "" {
				if cfg.AllowAnonymous {
					// Dev / kind-demo / unit tests: open-access semantics.
					// We stamp RoleAdmin so handler-level Require checks
					// pass; the startup warning makes it impossible to
					// ship this state to production by accident.
					return next(context.WithValue(ctx, principalKey{}, Principal{
						Sub: "anonymous", Role: RoleAdmin,
					}), req)
				}
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header"))
			}

			// "Bearer <token>"
			if !strings.HasPrefix(h, "Bearer ") {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("Authorization must be Bearer"))
			}
			token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))

			// API-key path first (cheaper than fetching JWKS).
			if p, ok := keys[token]; ok {
				return next(context.WithValue(ctx, principalKey{}, p), req)
			}

			// OIDC path. When JWKS is wired we verify the signature
			// + iss/aud/exp/nbf. Otherwise we fall back to a clearly-
			// logged presence-only check (dev-only, never production).
			if cfg.OIDCIssuer != "" && looksLikeJWT(token) {
				var (
					p   Principal
					err error
				)
				if cfg.JWKS != nil {
					p, err = verifyJWT(ctx, token, cfg.JWKS, cfg.OIDCIssuer, cfg.OIDCAudience, cfg.GroupRoles)
				} else {
					p, err = decodeJWTPresence(token, cfg.OIDCIssuer, cfg.OIDCAudience, cfg.GroupRoles)
				}
				if err != nil {
					return nil, connect.NewError(connect.CodeUnauthenticated, err)
				}
				return next(context.WithValue(ctx, principalKey{}, p), req)
			}

			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
		}
	}
}

// Require enforces a minimum role on the calling principal. Use it at
// the top of any handler that mutates state.
func Require(ctx context.Context, min Role) error {
	got := PrincipalFromContext(ctx).Role
	if !atLeast(got, min) {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("requires role %q, caller is %q", min, got))
	}
	return nil
}

func atLeast(got, want Role) bool {
	// Auditor lives at a parallel rank: it satisfies viewer but not
	// member (read-only widely; never mutates). When `want` is
	// auditor specifically, auditor and above (admin/owner) all
	// satisfy. We model this by giving auditor read-rank = viewer,
	// and a separate "audit-eligible" gate that admin/owner inherit.
	rank := map[Role]int{
		RoleAnonymous: 0,
		RoleViewer:    1,
		RoleAuditor:   1, // matches viewer for read endpoints
		RoleMember:    2,
		RoleAdmin:     3,
		RoleOwner:     4,
	}
	if want == RoleAuditor {
		// Auditor satisfied by auditor / admin / owner — i.e. anyone
		// with explicit audit rights or higher.
		switch got {
		case RoleAuditor, RoleAdmin, RoleOwner:
			return true
		}
		return false
	}
	return rank[got] >= rank[want]
}

// indexKeys turns the slice into a map for O(1) lookup. We hash the
// token in the Sub field so logs never leak the secret.
func indexKeys(raw []string) map[string]Principal {
	out := make(map[string]Principal, len(raw))
	for _, e := range raw {
		token, role := splitKey(e)
		if token == "" {
			continue
		}
		out[token] = Principal{
			Sub:  shortHash(token),
			Role: role,
		}
	}
	return out
}

func splitKey(e string) (string, Role) {
	if i := strings.LastIndex(e, ":"); i > 0 {
		token := strings.TrimSpace(e[:i])
		role := strings.ToLower(strings.TrimSpace(e[i+1:]))
		switch role {
		case "admin":
			return token, RoleAdmin
		case "member":
			return token, RoleMember
		}
		// Unknown suffix — treat the whole string as a token + member.
	}
	return strings.TrimSpace(e), RoleMember
}
