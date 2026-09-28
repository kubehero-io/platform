// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package auth

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
)

type fakeVerifier struct {
	tokens map[string]string
	calls  int
	err    error
}

func (f *fakeVerifier) VerifyClusterToken(_ context.Context, token string) (string, bool, error) {
	f.calls++
	if f.err != nil {
		return "", false, f.err
	}
	id, ok := f.tokens[token]
	return id, ok, nil
}

func callWith(t *testing.T, cfg Config, header string) (Principal, error) {
	t.Helper()
	var got Principal
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		got = PrincipalFromContext(ctx)
		return nil, nil
	})
	req := connect.NewRequest(&struct{}{})
	if header != "" {
		req.Header().Set("Authorization", header)
	}
	_, err := NewInterceptor(cfg)(next)(context.Background(), req)
	return got, err
}

func TestClusterTokenAuthenticatesAsMemberOfThatCluster(t *testing.T) {
	v := &fakeVerifier{tokens: map[string]string{"tok-abc": "c-123"}}
	cfg := Config{ClusterTokens: v}
	p, err := callWith(t, cfg, "Bearer tok-abc")
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleMember || p.ClusterID != "c-123" || p.Sub != "cluster:c-123" {
		t.Fatalf("principal = %+v", p)
	}
}

func TestClusterTokenVerdictsAreCached(t *testing.T) {
	v := &fakeVerifier{tokens: map[string]string{"tok-abc": "c-123"}}
	icpt := NewInterceptor(Config{ClusterTokens: v})
	next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, nil })
	for i := 0; i < 5; i++ {
		req := connect.NewRequest(&struct{}{})
		req.Header().Set("Authorization", "Bearer tok-abc")
		if _, err := icpt(next)(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		bad := connect.NewRequest(&struct{}{})
		bad.Header().Set("Authorization", "Bearer nope")
		_, _ = icpt(next)(context.Background(), bad)
	}
	if v.calls != 2 {
		t.Fatalf("verifier called %d times, want 2 (one per distinct token)", v.calls)
	}
}

func TestUnknownClusterTokenIsRejected(t *testing.T) {
	_, err := callWith(t, Config{ClusterTokens: &fakeVerifier{}}, "Bearer nope")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
}

func TestClusterTokenStoreErrorIsUnavailable(t *testing.T) {
	_, err := callWith(t, Config{ClusterTokens: &fakeVerifier{err: errors.New("db down")}}, "Bearer x")
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestStaticAPIKeyStillWinsOverClusterToken(t *testing.T) {
	v := &fakeVerifier{tokens: map[string]string{"shared": "c-1"}}
	p, err := callWith(t, Config{APIKeys: []string{"shared:admin"}, ClusterTokens: v}, "Bearer shared")
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleAdmin || v.calls != 0 {
		t.Fatalf("principal = %+v, verifier calls = %d", p, v.calls)
	}
}

func TestAPIKeyRoleSuffixes(t *testing.T) {
	cases := map[string]Role{
		"a:owner": RoleOwner, "b:admin": RoleAdmin, "c:auditor": RoleAuditor,
		"d:member": RoleMember, "e:viewer": RoleViewer, "f": RoleMember,
	}
	for entry, want := range cases {
		token, role := splitKey(entry)
		if role != want {
			t.Errorf("%q → role %q, want %q", entry, role, want)
		}
		if token == "" || token == entry && want != RoleMember {
			t.Errorf("%q → token %q", entry, token)
		}
	}
}
