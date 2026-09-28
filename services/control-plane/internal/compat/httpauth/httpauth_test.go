// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package httpauth

import (
	"context"
	"encoding/base64"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

type fakeTokens map[string]string

func (f fakeTokens) VerifyClusterToken(_ context.Context, token string) (string, bool, error) {
	id, ok := f[token]
	return id, ok, nil
}

func newAuth(cfg auth.Config) *Authenticator {
	return New(connect.WithInterceptors(auth.NewInterceptor(cfg)))
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestAuthenticateMirrorsInterceptor(t *testing.T) {
	strict := newAuth(auth.Config{
		APIKeys:       []string{"adm-key:admin", "view-key:viewer"},
		ClusterTokens: fakeTokens{"cluster-tok": "uuid-1"},
	})
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("grafana:view-key"))
	tests := []struct {
		name    string
		header  http.Header
		role    auth.Role
		status  int
		cluster string
	}{
		{"no credentials", hdr(), auth.RoleViewer, http.StatusUnauthorized, ""},
		{"bad token", hdr("Authorization", "Bearer nope"), auth.RoleViewer, http.StatusUnauthorized, ""},
		{"not bearer", hdr("Authorization", "Token x"), auth.RoleViewer, http.StatusUnauthorized, ""},
		{"api key", hdr("Authorization", "Bearer adm-key"), auth.RoleAdmin, http.StatusOK, ""},
		{"viewer may read", hdr("Authorization", "Bearer view-key"), auth.RoleViewer, http.StatusOK, ""},
		{"viewer may not push", hdr("Authorization", "Bearer view-key"), auth.RoleMember, http.StatusForbidden, ""},
		{"basic auth password is the token", hdr("Authorization", basic), auth.RoleViewer, http.StatusOK, ""},
		{"broken basic auth", hdr("Authorization", "Basic !!!"), auth.RoleViewer, http.StatusUnauthorized, ""},
		{"cluster enrollment token", hdr("Authorization", "Bearer cluster-tok"), auth.RoleMember, http.StatusOK, "uuid-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := strict.Require(context.Background(), tc.header, tc.role)
			if tc.status == http.StatusOK {
				if err != nil {
					t.Fatalf("err = %+v", err)
				}
				if got := auth.PrincipalFromContext(ctx).ClusterID; got != tc.cluster {
					t.Fatalf("cluster = %q, want %q", got, tc.cluster)
				}
				return
			}
			if err == nil || err.Status != tc.status {
				t.Fatalf("err = %+v, want status %d", err, tc.status)
			}
		})
	}

	// Anonymous access follows the interceptor's policy.
	open := newAuth(auth.Config{AllowAnonymous: true})
	if _, err := open.Require(context.Background(), hdr(), auth.RoleMember); err != nil {
		t.Fatalf("open mode rejected anonymous: %+v", err)
	}
	closed := newAuth(auth.Config{AllowAnonymous: false})
	if _, err := closed.Require(context.Background(), hdr(), auth.RoleViewer); err == nil || err.Status != http.StatusUnauthorized {
		t.Fatalf("closed mode: %+v", err)
	}
}
