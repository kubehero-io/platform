// SPDX-License-Identifier: BUSL-1.1
package pricing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubSource lets us drive the registry's fallback logic without
// making real HTTP calls.
type stubSource struct {
	cloud Cloud
	err   error
	q     Quote
}

func (s stubSource) Name() Cloud { return s.cloud }
func (s stubSource) Quote(_ context.Context, _, _, _ string) (Quote, error) {
	if s.err != nil {
		return Quote{}, s.err
	}
	return s.q, nil
}

func TestRegistryFallsBackOnUnimplemented(t *testing.T) {
	r := NewDefaultRegistry()
	r.WithLive(stubSource{cloud: CloudAWS, err: ErrUnimplemented})
	q, err := r.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.PricePerHour != 0.096 {
		t.Errorf("expected static fallback 0.096, got %v", q.PricePerHour)
	}
}

func TestRegistryFallsBackOnNotFound(t *testing.T) {
	r := NewDefaultRegistry()
	r.WithLive(stubSource{cloud: CloudGCP, err: ErrNotFound})
	q, err := r.Quote(context.Background(), CloudGCP, "n2-standard-4", "us-central1", "on-demand")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Cloud != CloudGCP || q.PricePerHour <= 0 {
		t.Errorf("static fallback returned %v", q)
	}
}

func TestRegistrySurfacesNetworkError(t *testing.T) {
	r := NewDefaultRegistry()
	r.WithLive(stubSource{cloud: CloudAzure, err: errors.New("network is unreachable")})
	_, err := r.Quote(context.Background(), CloudAzure, "Standard_D4s_v5", "westeurope", "on-demand")
	if err == nil {
		t.Fatal("expected error to surface, not be swallowed by static fallback")
	}
}

func TestRegistryUsesLiveOnSuccess(t *testing.T) {
	want := Quote{Cloud: CloudAWS, SKU: "x", Region: "y", Lifecycle: "on-demand", PricePerHour: 1.23, Currency: "USD"}
	r := NewDefaultRegistry()
	r.WithLive(stubSource{cloud: CloudAWS, q: want})
	got, err := r.Quote(context.Background(), CloudAWS, "x", "y", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if got.PricePerHour != 1.23 {
		t.Errorf("expected live result 1.23, got %v (registry didn't honour live)", got.PricePerHour)
	}
}

func TestAWSReturnsUnimplementedForSpot(t *testing.T) {
	a := &AWS{}
	_, err := a.Quote(context.Background(), "m5.large", "us-east-1", "spot")
	if !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("expected ErrUnimplemented for spot, got %v", err)
	}
}

func TestAzureFiltersByRegionAndSku(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("$filter")
		if !contains(filter, "armSkuName eq 'Standard_D4s_v5'") || !contains(filter, "armRegionName eq 'westeurope'") {
			t.Errorf("filter doesn't carry SKU + region: %q", filter)
		}
		_, _ = w.Write([]byte(`{"Items":[{"currencyCode":"USD","unitPrice":0.192,"armSkuName":"Standard_D4s_v5","productName":"Virtual Machines DSv5 Series"}]}`))
	}))
	defer srv.Close()

	a := &Azure{Endpoint: srv.URL}
	q, err := a.Quote(context.Background(), "Standard_D4s_v5", "westeurope", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.192 {
		t.Fatalf("expected 0.192, got %v", q.PricePerHour)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > len(sub) && (find(s, sub) >= 0)))
}

func find(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
