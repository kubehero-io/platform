// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package httpauth protects plain-HTTP routes (the OpenCost-compatible
// /allocation API, the FOCUS export) with exactly the authentication
// the Connect handlers get.
//
// serve() hands the query plane its interceptors only as opaque
// connect.HandlerOptions, so rather than re-reading auth env vars (and
// drifting from main's config: API keys, OIDC/JWKS, cluster enrollment
// tokens, anonymous dev mode) the middleware runs each request through
// an in-process Connect unary handler built with those same options.
// The interceptor chain resolves the caller, the handler checks the
// role, and the real route runs inside it with the principal in ctx.
// Costs one small allocation per request and no network hop.
package httpauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

// procedure is the synthetic route the bridge dispatches to. It is
// never mounted on the public mux.
const procedure = "/kubehero.internal.v1.HTTPAuthBridge/Check"

type carrierKey struct{}

type carrier struct {
	w      http.ResponseWriter
	r      *http.Request
	next   http.Handler
	served bool
}

// Middleware returns a wrapper that admits callers holding at least
// minRole under the given Connect handler options (normally the
// control plane's auth interceptor).
func Middleware(minRole auth.Role, opts ...connect.HandlerOption) func(http.Handler) http.Handler {
	bridge := connect.NewUnaryHandler(procedure,
		func(ctx context.Context, _ *connect.Request[kuberov1.HealthCheckRequest]) (*connect.Response[kuberov1.HealthCheckResponse], error) {
			c, _ := ctx.Value(carrierKey{}).(*carrier)
			if c == nil {
				return nil, connect.NewError(connect.CodeInternal, errors.New("httpauth: request carrier missing"))
			}
			if err := auth.Require(ctx, minRole); err != nil {
				return nil, err
			}
			c.served = true
			c.next.ServeHTTP(c.w, c.r.WithContext(ctx))
			return connect.NewResponse(&kuberov1.HealthCheckResponse{}), nil
		}, opts...)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := &carrier{w: w, r: r, next: next}
			syn, err := http.NewRequestWithContext(
				context.WithValue(r.Context(), carrierKey{}, c),
				http.MethodPost, procedure, http.NoBody)
			if err != nil {
				WriteError(w, http.StatusInternalServerError, "auth bridge: "+err.Error())
				return
			}
			syn.Header.Set("Content-Type", "application/proto")
			if a := r.Header.Get("Authorization"); a != "" {
				syn.Header.Set("Authorization", a)
			}
			rec := &recorder{header: http.Header{}}
			bridge.ServeHTTP(rec, syn)
			if c.served {
				return
			}
			status := rec.status
			if status == 0 || status == http.StatusOK {
				status = http.StatusInternalServerError
			}
			WriteError(w, status, connectMessage(rec.body.Bytes()))
		})
	}
}

// WriteError writes the JSON error envelope the HTTP query routes use
// ({"code":401,"status":"error","message":"…"} — OpenCost's shape).
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "status": "error", "message": msg})
}

// connectMessage extracts the human message from a Connect error body.
func connectMessage(body []byte) string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "unauthorized"
}

// recorder captures the bridge's (error) response; the real response
// is written by the wrapped route directly to the client.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	// Error bodies are tiny; cap anyway so a misbehaving handler can't
	// balloon memory.
	if room := 64<<10 - r.body.Len(); room > 0 {
		if len(b) > room {
			r.body.Write(b[:room])
		} else {
			r.body.Write(b)
		}
	}
	return len(b), nil
}
