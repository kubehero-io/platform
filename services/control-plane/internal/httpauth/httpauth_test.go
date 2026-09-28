// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package httpauth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func newServer(t *testing.T, min auth.Role, cfg auth.Config) *httptest.Server {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := Middleware(min, connect.WithInterceptors(auth.NewInterceptor(cfg)))
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.PrincipalFromContext(r.Context())
		w.Header().Set("X-Role", string(p.Role))
		w.Header().Set("X-Query", r.URL.Query().Get("window"))
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mw(inner))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestMiddlewareAPIKeys(t *testing.T) {
	srv := newServer(t, auth.RoleViewer, auth.Config{APIKeys: []string{"admintok:admin", "membertok"}})

	resp, body := get(t, srv.URL+"/allocation?window=7d", "admintok")
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("admin: %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Role") != "admin" || resp.Header.Get("X-Query") != "7d" {
		t.Fatalf("principal/query not propagated: %v", resp.Header)
	}

	resp, body = get(t, srv.URL+"/allocation", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: want 401, got %d %q", resp.StatusCode, body)
	}
	var e map[string]any
	if err := json.Unmarshal([]byte(body), &e); err != nil || e["status"] != "error" || e["message"] == "" {
		t.Fatalf("error envelope: %q", body)
	}

	resp, _ = get(t, srv.URL+"/allocation", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: want 401, got %d", resp.StatusCode)
	}
}

func TestMiddlewareRoleCheck(t *testing.T) {
	srv := newServer(t, auth.RoleAdmin, auth.Config{APIKeys: []string{"membertok"}})
	resp, body := get(t, srv.URL+"/x", "membertok")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member on admin route: want 403, got %d %q", resp.StatusCode, body)
	}
}

func TestMiddlewareAnonymousDevMode(t *testing.T) {
	srv := newServer(t, auth.RoleViewer, auth.Config{AllowAnonymous: true})
	resp, body := get(t, srv.URL+"/x", "")
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("anonymous dev mode: %d %q", resp.StatusCode, body)
	}
}

func TestMiddlewareWithoutInterceptorFailsClosed(t *testing.T) {
	// No interceptor → anonymous principal → below viewer.
	mw := Middleware(auth.RoleViewer)
	srv := httptest.NewServer(mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("leak"))
	})))
	defer srv.Close()
	resp, body := get(t, srv.URL, "")
	if resp.StatusCode != http.StatusForbidden || body == "leak" {
		t.Fatalf("want fail-closed 403, got %d %q", resp.StatusCode, body)
	}
}
