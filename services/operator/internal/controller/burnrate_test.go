// SPDX-License-Identifier: BUSL-1.1
package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseCeilingUSD(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"$100000/mo", 100000},
		{"$100,000 / mo", 100000},
		{"100000", 100000},
		{"$300/hr", 300 * 24 * 30},
		{"$1.5k", 0}, // unsupported shorthand
		{"abc", 0},
		{"", 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := ParseCeilingUSD(c.in); got != c.want {
				t.Fatalf("ParseCeilingUSD(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// fixedCeiling is a CeilingResolver that always resolves to v.
func fixedCeiling(v float64) CeilingResolver {
	return func(_ context.Context, _, _ string) (float64, error) { return v, nil }
}

func TestControlPlaneBurnRateRoundTrip(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kubehero.v1.ControlPlaneService/GetBurnRate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			ClusterID         string  `json:"clusterId"`
			Namespace         string  `json:"namespace"`
			Window            string  `json:"window"`
			MonthlyCeilingUsd float64 `json:"monthlyCeilingUsd"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.ClusterID != "c1" || req.Window != "5m" ||
			req.MonthlyCeilingUsd != 10000 || req.Namespace != "ml-inference" {
			t.Errorf("unexpected body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"burnRateMilli":1500,"available":true,"source":"clickhouse"}`))
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "s3cret", "c1", fixedCeiling(10000))
	got, err := p.BurnRateMilli(context.Background(), "ml-inference", "prod-monthly-ceiling", "5m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1500 {
		t.Fatalf("got %d, want 1500", got)
	}
	if gotAuth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
}

func TestControlPlaneBurnRateErrorReturnsZeroAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "clickhouse exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(1000))
	got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
	if got != 0 {
		t.Fatalf("got %d, want 0 on RPC error (never trip on missing data)", got)
	}
	if err == nil {
		t.Fatal("want error surfaced so the reconciler flips Tripped to Unknown, got nil")
	}
}

func TestControlPlaneBurnRateUnreachableReturnsZeroAndError(t *testing.T) {
	// Closed server → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(1000))
	got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
	if got != 0 || err == nil {
		t.Fatalf("got (%d, %v), want (0, error) when control-plane is unreachable", got, err)
	}
}

func TestControlPlaneBurnRateAvailableFalseReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"burnRateMilli":0,"available":false,"source":"stub"}`))
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(1000))
	got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("got %d, want 0 when available=false", got)
	}
}

func TestControlPlaneBurnRateZeroCeilingSkipsRPC(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(0))
	got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
	if got != 0 || err != nil {
		t.Fatalf("got (%d, %v), want (0, nil) for unresolvable ceiling", got, err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("control-plane called %d times, want 0 (no ceiling → no RPC)", n)
	}
}

func TestControlPlaneBurnRateCacheTTL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"burnRateMilli":1200,"available":true,"source":"clickhouse"}`))
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(1000))
	fakeNow := time.Now()
	p.now = func() time.Time { return fakeNow }

	// Two reads inside the TTL → one RPC.
	for i := 0; i < 2; i++ {
		got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
		if err != nil || got != 1200 {
			t.Fatalf("read %d: got (%d, %v), want (1200, nil)", i, got, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("control-plane called %d times within TTL, want 1", n)
	}

	// A different scope must not share the cache entry.
	if _, err := p.BurnRateMilli(context.Background(), "other-ns", "br", "5m"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("control-plane called %d times for a second scope, want 2", n)
	}

	// Past the TTL → refreshed.
	fakeNow = fakeNow.Add(burnRateCacheTTL + time.Second)
	if _, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("control-plane called %d times after TTL expiry, want 3", n)
	}
}

func TestControlPlaneBurnRateCachesFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewControlPlaneBurnRate(srv.URL, "", "c1", fixedCeiling(1000))
	fakeNow := time.Now()
	p.now = func() time.Time { return fakeNow }

	for i := 0; i < 3; i++ {
		got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
		if got != 0 || err == nil {
			t.Fatalf("read %d: got (%d, %v), want (0, error)", i, got, err)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("a failing control-plane was called %d times within TTL, want 1 (rate-limited)", n)
	}
}

func TestSelectBurnRateProvider(t *testing.T) {
	p, live := SelectBurnRateProvider("", "tok", "c1", nil)
	if live {
		t.Fatal("empty endpoint must select the stub")
	}
	if _, ok := p.(StubBurnRate); !ok {
		t.Fatalf("got %T, want StubBurnRate", p)
	}

	p, live = SelectBurnRateProvider("   ", "", "", nil)
	if live {
		t.Fatal("blank endpoint must select the stub")
	}
	if _, ok := p.(StubBurnRate); !ok {
		t.Fatalf("got %T, want StubBurnRate", p)
	}

	p, live = SelectBurnRateProvider("http://cp.kubehero.svc:8080", "tok", "c1", fixedCeiling(1)) //nolint:lll
	if !live {
		t.Fatal("non-empty endpoint must select the control-plane provider")
	}
	if _, ok := p.(*ControlPlaneBurnRate); !ok {
		t.Fatalf("got %T, want *ControlPlaneBurnRate", p)
	}
}
