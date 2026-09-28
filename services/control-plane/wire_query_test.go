// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

func serveQuery(t *testing.T, deps wireDeps, cfg auth.Config) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Logger = log
	deps.Log = log
	deps.Handler = []connect.HandlerOption{connect.WithInterceptors(auth.NewInterceptor(cfg))}
	deps.Alerts = alerter.NewRouter()
	mux := http.NewServeMux()
	wireQuery(ctx, mux, deps, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Without stores every query service answers with labelled demo data.
func TestWireQueryDemoMode(t *testing.T) {
	srv := serveQuery(t, wireDeps{}, auth.Config{AllowAnonymous: true})
	ctx := context.Background()

	cost := kuberov1connect.NewCostServiceClient(srv.Client(), srv.URL)
	a, err := cost.GetAllocation(ctx, connect.NewRequest(&kuberov1.GetAllocationRequest{Window: "yesterday", IncludeIdle: true}))
	if err != nil || a.Msg.GetSource() != "demo" || len(a.Msg.GetAllocations()) == 0 {
		t.Fatalf("GetAllocation: %v %v", err, a)
	}
	if _, err := cost.ListRightsizing(ctx, connect.NewRequest(&kuberov1.ListRightsizingRequest{})); err != nil {
		t.Fatalf("ListRightsizing: %v", err)
	}
	prof := kuberov1connect.NewProfilesServiceClient(srv.Client(), srv.URL)
	if r, err := prof.ListProfileTargets(ctx, connect.NewRequest(&kuberov1.ListProfileTargetsRequest{})); err != nil || len(r.Msg.GetTargets()) == 0 {
		t.Fatalf("ListProfileTargets: %v", err)
	}
	netc := kuberov1connect.NewNetworkServiceClient(srv.Client(), srv.URL)
	if r, err := netc.GetServiceMap(ctx, connect.NewRequest(&kuberov1.GetServiceMapRequest{})); err != nil || r.Msg.GetSource() != "demo" {
		t.Fatalf("GetServiceMap: %v", err)
	}

	// The evaluator seeds the default rules in the background.
	alerts := kuberov1connect.NewAlertsServiceClient(srv.Client(), srv.URL)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := alerts.ListAlertRules(ctx, connect.NewRequest(&kuberov1.ListAlertRulesRequest{}))
		if err == nil && len(r.Msg.GetRules()) == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("default rules not seeded: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r, err := alerts.ListAlerts(ctx, connect.NewRequest(&kuberov1.ListAlertsRequest{})); err != nil || r.Msg.GetFiring() == 0 {
		t.Fatalf("demo alerts: %v", err)
	}

	resp, err := http.Get(srv.URL + "/allocation/compute?window=1d&aggregate=namespace&accumulate=true")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if resp.StatusCode != 200 || env.Code != 200 || len(env.Data) != 1 {
		t.Fatalf("OpenCost route: %d %+v", resp.StatusCode, env)
	}

	resp, err = http.Get(srv.URL + "/api/v1/export/focus?window=1d")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || len(rows) < 2 || rows[0][0] != "BilledCost" {
		t.Fatalf("FOCUS route: %d %v", resp.StatusCode, err)
	}
}

// KUBEHERO_DEMO_MODE=false without stores: RPCs fail loudly, HTTP 503.
func TestWireQueryDemoDisabled(t *testing.T) {
	srv := serveQuery(t, wireDeps{DemoFixturesDisabled: true}, auth.Config{AllowAnonymous: true})
	cost := kuberov1connect.NewCostServiceClient(srv.Client(), srv.URL)
	_, err := cost.GetAllocation(context.Background(), connect.NewRequest(&kuberov1.GetAllocationRequest{}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("GetAllocation: %v", err)
	}
	resp, err := http.Get(srv.URL + "/allocation?window=1d")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("OpenCost route with demo disabled: %d", resp.StatusCode)
	}
}

// The HTTP faces enforce the same credentials as the Connect handlers.
func TestWireQueryHTTPAuth(t *testing.T) {
	srv := serveQuery(t, wireDeps{}, auth.Config{APIKeys: []string{"s3cret:member"}})
	for _, path := range []string{"/allocation/compute?window=1d", "/api/v1/export/focus?window=1d"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token: %d", path, resp.StatusCode)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(body), `"status":"error"`) {
			t.Fatalf("%s with a token: %d %s", path, resp.StatusCode, body)
		}
	}
	// Connect handlers share the interceptor: no token, no data.
	cost := kuberov1connect.NewCostServiceClient(srv.Client(), srv.URL)
	_, err := cost.GetAllocation(context.Background(), connect.NewRequest(&kuberov1.GetAllocationRequest{}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnauthenticated {
		t.Fatalf("anonymous Connect call: %v", err)
	}
}
