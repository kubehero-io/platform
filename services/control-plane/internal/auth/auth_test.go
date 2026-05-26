// SPDX-License-Identifier: BUSL-1.1
package auth

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
)

// fakeReq satisfies enough of connect.AnyRequest for our interceptor.
type fakeReq struct {
	connect.AnyRequest
	hdr http.Header
}

func (f *fakeReq) Header() http.Header { return f.hdr }
func (f *fakeReq) Spec() connect.Spec  { return connect.Spec{Procedure: "/test/Method"} }

func newReq(authHeader string) *fakeReq {
	h := http.Header{}
	if authHeader != "" {
		h.Set("Authorization", authHeader)
	}
	return &fakeReq{hdr: h}
}

// fakeNext captures the principal the interceptor stamped.
func fakeNext(captured *Principal) connect.UnaryFunc {
	return func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		*captured = PrincipalFromContext(ctx)
		return nil, nil
	}
}

func TestParseAPIKeysSplitsAndTrims(t *testing.T) {
	got := ParseAPIKeys("a, b:admin , ,  c:member")
	want := []string{"a", "b:admin", "c:member"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestInterceptorAnonymousMode(t *testing.T) {
	cfg := Config{AllowAnonymous: true}
	var p Principal
	_, _ = NewInterceptor(cfg)(fakeNext(&p))(context.Background(), newReq(""))
	if p.Role != RoleAdmin {
		t.Fatalf("anonymous mode should stamp admin, got %q", p.Role)
	}
}

func TestInterceptorRejectsMissingHeaderInClosedMode(t *testing.T) {
	cfg := Config{AllowAnonymous: false, APIKeys: []string{"key1"}}
	var p Principal
	_, err := NewInterceptor(cfg)(fakeNext(&p))(context.Background(), newReq(""))
	var ce *connect.Error
	if err == nil {
		t.Fatal("want error")
	}
	if !errorsAs(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestInterceptorAcceptsAPIKey(t *testing.T) {
	cfg := Config{APIKeys: []string{"abc:admin", "xyz"}}
	var p Principal
	_, err := NewInterceptor(cfg)(fakeNext(&p))(context.Background(), newReq("Bearer abc"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Role != RoleAdmin {
		t.Errorf("want admin, got %q", p.Role)
	}
	if p.Sub == "" || p.Sub == "abc" {
		t.Errorf("Sub should be a hash, not the raw token: %q", p.Sub)
	}

	// "xyz" defaults to member when no role suffix was given.
	_, _ = NewInterceptor(cfg)(fakeNext(&p))(context.Background(), newReq("Bearer xyz"))
	if p.Role != RoleMember {
		t.Errorf("want member default, got %q", p.Role)
	}
}

func TestInterceptorRejectsUnknownToken(t *testing.T) {
	cfg := Config{APIKeys: []string{"abc:admin"}}
	var p Principal
	_, err := NewInterceptor(cfg)(fakeNext(&p))(context.Background(), newReq("Bearer wrong"))
	var ce *connect.Error
	if err == nil || !errorsAs(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestRequireEnforcesRole(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Role: RoleMember})
	if err := Require(ctx, RoleAdmin); err == nil {
		t.Fatal("member should fail admin Require")
	}
	if err := Require(ctx, RoleMember); err != nil {
		t.Fatal("member should pass member Require")
	}
}

// errorsAs duplicated locally so this test file stays tight.
func errorsAs(err error, target **connect.Error) bool {
	for err != nil {
		if ce, ok := err.(*connect.Error); ok {
			*target = ce
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
