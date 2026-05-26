// SPDX-License-Identifier: BUSL-1.1
package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestRPCBurnRateRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kubehero.v1.ControlPlaneService/GetBurnRate" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var req struct {
			Window            string  `json:"window"`
			MonthlyCeilingUsd float64 `json:"monthlyCeilingUsd"`
			Namespace         string  `json:"namespace"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Window != "5m" || req.MonthlyCeilingUsd != 10000 || req.Namespace != "ml-inference" {
			t.Fatalf("unexpected body: %+v", req)
		}
		_, _ = w.Write([]byte(`{"burnRateMilli":1500,"available":true,"source":"clickhouse"}`))
	}))
	defer srv.Close()

	p := &RPCBurnRate{
		Endpoint: srv.URL,
		CeilingResolver: func(_ context.Context, _, _ string) (float64, error) {
			return 10000, nil
		},
	}
	got, err := p.BurnRateMilli(context.Background(), "ml-inference", "prod-monthly-ceiling", "5m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1500 {
		t.Fatalf("got %d, want 1500", got)
	}
}

func TestRPCBurnRateAvailableFalseReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"burnRateMilli":0,"available":false,"source":"stub"}`))
	}))
	defer srv.Close()

	p := &RPCBurnRate{
		Endpoint:        srv.URL,
		CeilingResolver: func(_ context.Context, _, _ string) (float64, error) { return 1000, nil },
	}
	got, err := p.BurnRateMilli(context.Background(), "ns", "br", "5m")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("got %d, want 0 when available=false", got)
	}
}
