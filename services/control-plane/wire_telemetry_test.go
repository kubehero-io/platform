// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

// bearer adds an Authorization header to every Connect call.
type bearer string

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Without ClickHouse, with auth required: every telemetry surface is
// mounted, logs serve demo data behind the same auth as the RPCs
// (streams included), ingest accepts-and-drops, and no metric engine
// is handed to the alert evaluator.
func TestWireTelemetryDemoMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	interceptor := connect.WithInterceptors(auth.NewInterceptor(auth.Config{
		APIKeys: []string{"viewer-key:viewer", "member-key:member"}, AllowAnonymous: false,
	}))
	mux := http.NewServeMux()
	engine := wireTelemetry(ctx, mux, wireDeps{
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler: []connect.HandlerOption{interceptor},
	})
	if engine != nil {
		t.Fatal("demo mode must not hand a metric engine to the alert evaluator")
	}
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	defer srv.Close()

	anon := kuberov1connect.NewLogsServiceClient(srv.Client(), srv.URL)
	logsClient := kuberov1connect.NewLogsServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer("viewer-key")))
	tel := kuberov1connect.NewTelemetryServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer("member-key")))

	if _, err := anon.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{namespace="shop"}`})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous QueryLogs: %v", err)
	}
	res, err := logsClient.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{namespace="shop"} |= "GET"`, Limit: 5}))
	if err != nil || len(res.Msg.GetLines()) != 5 || res.Msg.GetLines()[0].GetLabels()["source"] != "demo" {
		t.Fatalf("QueryLogs: %v %v", res, err)
	}

	// Server stream: unary interceptors do not wrap streams, so the
	// stream interceptor must authenticate it.
	stream, err := anon.TailLogs(ctx, connect.NewRequest(&kuberov1.TailLogsRequest{Query: `{namespace="shop"}`}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous TailLogs: %v", err)
	}
	tailCtx, stopTail := context.WithTimeout(ctx, 10*time.Second)
	defer stopTail()
	stream, err = logsClient.TailLogs(tailCtx, connect.NewRequest(&kuberov1.TailLogsRequest{Query: `{namespace="shop"}`}))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for got == 0 && stream.Receive() {
		got += len(stream.Msg().GetLines())
	}
	if got == 0 {
		t.Fatalf("TailLogs delivered nothing: %v", stream.Err())
	}
	stopTail()
	_ = stream.Close()

	ing, err := tel.IngestLogs(ctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: []*kuberov1.LogEntry{{Body: "x"}}}))
	if err != nil || ing.Msg.GetDropped() != 1 {
		t.Fatalf("IngestLogs without ClickHouse: %v %v", ing, err)
	}
	viewerTel := kuberov1connect.NewTelemetryServiceClient(srv.Client(), srv.URL, connect.WithInterceptors(bearer("viewer-key")))
	if _, err := viewerTel.IngestLogs(ctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer IngestLogs: %v", err)
	}

	// Plain-HTTP shims share the same auth.
	for path, want := range map[string]int{
		"/loki/api/v1/labels":           http.StatusUnauthorized,
		"/loki/api/v1/status/buildinfo": http.StatusOK,
		"/ready":                        http.StatusOK,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/loki/api/v1/label/namespace/values", nil)
	req.Header.Set("Authorization", "Bearer viewer-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"shop"`) {
		t.Fatalf("label values: %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/loki/api/v1/push", "/v1/logs", "/ingest?name=app.cpu"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer viewer-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d, want 403 (pushing needs member)", path, resp.StatusCode)
		}
	}
}

// KUBEHERO_DEMO_MODE=false without ClickHouse: logs fail loudly.
func TestWireTelemetryNoDemo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := http.NewServeMux()
	wireTelemetry(ctx, mux, wireDeps{
		Log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Handler:              []connect.HandlerOption{connect.WithInterceptors(auth.NewInterceptor(auth.Config{AllowAnonymous: true}))},
		DemoFixturesDisabled: true,
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := kuberov1connect.NewLogsServiceClient(srv.Client(), srv.URL)
	_, err := c.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{a="b"}`}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("QueryLogs = %v, want FailedPrecondition", err)
	}
	resp, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/ready = %d, want 503 without a log store", resp.StatusCode)
	}
}
