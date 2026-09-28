// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package httpauth authenticates plain-HTTP compatibility endpoints
// (Loki, OTLP, Pyroscope) and Connect server streams exactly like the
// Connect unary handlers are authenticated.
//
// The control plane's auth lives in a Connect interceptor configured in
// main (API keys, cluster enrollment tokens, OIDC/JWKS, anonymous
// policy). Rather than re-deriving that configuration here — and
// risking a plain-HTTP door that is more permissive than the RPCs —
// the Authenticator runs every request's credentials through a tiny
// in-process Connect handler built with the very same handler options.
// Whatever principal (or error) the real interceptor produces is the
// answer.
//
// Streams need this too: a connect.UnaryInterceptorFunc does not wrap
// streaming handlers, so without StreamInterceptor a server stream
// would run with no principal at all.
package httpauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

const probeProcedure = "/kubehero.internal.v1.AuthProbe/Check"

type slotKey struct{}

type slot struct{ principal *auth.Principal }

// Authenticator resolves callers with the Connect interceptor chain.
type Authenticator struct {
	probe http.Handler
}

// New builds an authenticator from the handler options every Connect
// service is mounted with (deps.Handler in main).
func New(opts ...connect.HandlerOption) *Authenticator {
	h := connect.NewUnaryHandler(probeProcedure,
		func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			if s, ok := ctx.Value(slotKey{}).(*slot); ok {
				p := auth.PrincipalFromContext(ctx)
				s.principal = &p
			}
			return connect.NewResponse(&emptypb.Empty{}), nil
		}, opts...)
	return &Authenticator{probe: h}
}

// Error is an authentication / authorisation failure with the HTTP
// status and Connect code to answer with.
type Error struct {
	Status int
	Code   connect.Code
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// bearer extracts the credential. Basic auth is accepted with the token
// as the password (what Grafana datasources and log shippers that only
// speak basic auth can send); the username is ignored.
func bearer(h string) string {
	h = strings.TrimSpace(h)
	if rest, ok := strings.CutPrefix(h, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return "Basic invalid" // the interceptor rejects non-Bearer schemes
		}
		_, pass, found := strings.Cut(string(raw), ":")
		if !found || pass == "" {
			return "Basic invalid"
		}
		return "Bearer " + pass
	}
	return h
}

// Authenticate returns ctx carrying the caller's principal.
func (a *Authenticator) Authenticate(ctx context.Context, header http.Header) (context.Context, *Error) {
	s := &slot{}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, slotKey{}, s), http.MethodPost, probeProcedure, strings.NewReader("{}"))
	if err != nil {
		return nil, &Error{Status: http.StatusInternalServerError, Code: connect.CodeInternal, Msg: "auth probe: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if h := header.Get("Authorization"); h != "" {
		req.Header.Set("Authorization", bearer(h))
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	a.probe.ServeHTTP(rec, req)
	if rec.status == http.StatusOK && s.principal != nil {
		// Carry the principal the interceptor resolved onto the real
		// request's context (the probe's own context ends with it).
		return auth.WithPrincipal(ctx, *s.principal), nil
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(rec.body.String()), &body)
	e := &Error{Status: rec.status, Code: connect.CodeUnauthenticated, Msg: body.Message}
	switch body.Code {
	case "permission_denied":
		e.Status, e.Code = http.StatusForbidden, connect.CodePermissionDenied
	case "unavailable":
		e.Status, e.Code = http.StatusServiceUnavailable, connect.CodeUnavailable
	case "unauthenticated", "":
		e.Status, e.Code = http.StatusUnauthorized, connect.CodeUnauthenticated
	default:
		e.Status, e.Code = http.StatusInternalServerError, connect.CodeInternal
	}
	if e.Msg == "" {
		e.Msg = "authentication failed"
	}
	return nil, e
}

// Require authenticates and checks a minimum role.
func (a *Authenticator) Require(ctx context.Context, header http.Header, role auth.Role) (context.Context, *Error) {
	ctx, aerr := a.Authenticate(ctx, header)
	if aerr != nil {
		return nil, aerr
	}
	if err := auth.Require(ctx, role); err != nil {
		var ce *connect.Error
		msg := err.Error()
		if errors.As(err, &ce) {
			msg = ce.Message()
		}
		return nil, &Error{Status: http.StatusForbidden, Code: connect.CodePermissionDenied, Msg: msg}
	}
	return ctx, nil
}

// WriteError answers an HTTP request with an auth failure.
func WriteError(w http.ResponseWriter, e *Error) {
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kubehero"`)
	}
	http.Error(w, e.Msg, e.Status)
}

// StreamInterceptor authenticates Connect server streams with the same
// probe (unary interceptors do not wrap streams). Unary calls pass
// through untouched — the regular interceptor handles them.
func (a *Authenticator) StreamInterceptor() connect.Interceptor { return streamAuth{a} }

type streamAuth struct{ a *Authenticator }

func (s streamAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (s streamAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (s streamAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		pctx, aerr := s.a.Authenticate(ctx, conn.RequestHeader())
		if aerr != nil {
			return connect.NewError(aerr.Code, errors.New(aerr.Msg))
		}
		return next(pctx, conn)
	}
}

// recorder is a minimal in-memory ResponseWriter for the probe.
type recorder struct {
	header http.Header
	status int
	wrote  bool
	body   strings.Builder
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if r.body.Len() < 4096 { // an error body; nothing large comes back
		r.body.Write(p)
	}
	return len(p), nil
}
