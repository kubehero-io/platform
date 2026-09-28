// SPDX-License-Identifier: BUSL-1.1
package pricing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countingSource counts Quote calls and returns a scripted result. The
// result can be swapped mid-test to simulate a provider going down.
type countingSource struct {
	cloud Cloud
	calls atomic.Int64

	mu  sync.Mutex
	q   Quote
	err error
}

func (s *countingSource) Name() Cloud { return s.cloud }

func (s *countingSource) Quote(_ context.Context, _, _, _ string) (Quote, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.q, s.err
}

func (s *countingSource) set(q Quote, err error) {
	s.mu.Lock()
	s.q, s.err = q, err
	s.mu.Unlock()
}

func TestCatalogCacheHit(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand", PricePerHour: 0.101, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	for i := 0; i < 3; i++ {
		q, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand")
		if err != nil {
			t.Fatalf("Quote #%d: %v", i, err)
		}
		if q.PricePerHour != 0.101 {
			t.Fatalf("Quote #%d: price %v want 0.101 (live, not static)", i, q.PricePerHour)
		}
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("provider called %d times, want 1 (cache should serve repeats)", got)
	}
}

func TestCatalogMissCallsProvider(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{Cloud: CloudAWS, SKU: "c7g.xlarge", Region: "eu-west-1", Lifecycle: "on-demand", PricePerHour: 0.161, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	q, err := c.Quote(context.Background(), CloudAWS, "c7g.xlarge", "eu-west-1", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.161 {
		t.Fatalf("price %v want live 0.161", q.PricePerHour)
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("provider called %d times, want 1", got)
	}
}

func TestCatalogFallsBackToStaticOnProviderFailure(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{}, errors.New("connection refused"))
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	q, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand")
	if err != nil {
		t.Fatalf("provider outage must not fail a static-answerable quote: %v", err)
	}
	if q.PricePerHour != 0.096 {
		t.Fatalf("price %v want static 0.096", q.PricePerHour)
	}
}

func TestCatalogSurfacesProviderErrorWhenStaticMisses(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{}, errors.New("connection refused"))
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	_, err := c.Quote(context.Background(), CloudAWS, "z9.mega", "ap-south-3", "on-demand")
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected the provider error to surface, got %v", err)
	}
}

func TestCatalogUnimplementedLifecycleFallsBackThenErrors(t *testing.T) {
	c := NewCatalog(time.Hour, discardLog()).WithLive(NewAWS())

	// Static knows m5.large spot — the catalog must answer.
	q, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "spot")
	if err != nil {
		t.Fatalf("spot with static coverage: %v", err)
	}
	if q.PricePerHour != 0.029 {
		t.Fatalf("price %v want static spot 0.029", q.PricePerHour)
	}

	// Unknown spot SKU — surface ErrUnimplemented with a message that
	// names the supported lifecycle.
	_, err = c.Quote(context.Background(), CloudAWS, "z9.mega", "us-east-1", "spot")
	if !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("expected ErrUnimplemented, got %v", err)
	}
	if !strings.Contains(err.Error(), "on-demand") {
		t.Fatalf("error should name the supported lifecycle, got %q", err.Error())
	}
}

func TestCatalogTTLExpiry(t *testing.T) {
	src := &countingSource{cloud: CloudGCP}
	src.set(Quote{Cloud: CloudGCP, SKU: "n2-standard-4", Region: "us-central1", Lifecycle: "on-demand", PricePerHour: 0.2, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	now := time.Now()
	c.now = func() time.Time { return now }

	if _, err := c.Quote(context.Background(), CloudGCP, "n2-standard-4", "us-central1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	// Still fresh: 59m later.
	now = now.Add(59 * time.Minute)
	if _, err := c.Quote(context.Background(), CloudGCP, "n2-standard-4", "us-central1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("provider called %d times before TTL expiry, want 1", got)
	}
	// Expired: 61m past the refresh.
	now = now.Add(2 * time.Minute)
	if _, err := c.Quote(context.Background(), CloudGCP, "n2-standard-4", "us-central1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("provider called %d times after TTL expiry, want 2", got)
	}
}

func TestCatalogServesStaleCacheWhenProviderFails(t *testing.T) {
	src := &countingSource{cloud: CloudAzure}
	src.set(Quote{Cloud: CloudAzure, SKU: "Standard_D4s_v5", Region: "westeurope", Lifecycle: "on-demand", PricePerHour: 0.2077, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	now := time.Now()
	c.now = func() time.Time { return now }

	if _, err := c.Quote(context.Background(), CloudAzure, "Standard_D4s_v5", "westeurope", "on-demand"); err != nil {
		t.Fatal(err)
	}
	// Expire the entry, then break the provider. The stale live price
	// (0.2077) must win over the static one (0.192).
	now = now.Add(2 * time.Hour)
	src.set(Quote{}, errors.New("503 backend unavailable"))
	q, err := c.Quote(context.Background(), CloudAzure, "Standard_D4s_v5", "westeurope", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.2077 {
		t.Fatalf("price %v want stale live 0.2077 (not static 0.192)", q.PricePerHour)
	}
}

// blockingSource parks every Quote call until release is closed —
// drives the single-flight assertion.
type blockingSource struct {
	cloud   Cloud
	release chan struct{}
	calls   atomic.Int64
}

func (s *blockingSource) Name() Cloud { return s.cloud }

func (s *blockingSource) Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return Quote{}, ctx.Err()
	}
	return Quote{Cloud: s.cloud, SKU: sku, Region: region, Lifecycle: lifecycle, PricePerHour: 4.2, Currency: "USD"}, nil
}

func TestCatalogSingleFlight(t *testing.T) {
	src := &blockingSource{cloud: CloudAWS, release: make(chan struct{})}
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	const n = 16
	var wg sync.WaitGroup
	results := make([]Quote, n)
	errs := make([]error, n)
	started := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			results[i], errs[i] = c.Quote(context.Background(), CloudAWS, "p5.48xlarge", "us-east-1", "on-demand")
		}(i)
	}
	for i := 0; i < n; i++ {
		<-started
	}
	// Give the goroutines a beat to reach the flight before releasing.
	time.Sleep(50 * time.Millisecond)
	close(src.release)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i].PricePerHour != 4.2 {
			t.Fatalf("goroutine %d: price %v want 4.2", i, results[i].PricePerHour)
		}
	}
	if got := src.calls.Load(); got != 1 {
		t.Fatalf("provider called %d times for %d concurrent misses, want 1 (single-flight)", got, n)
	}
}

func TestCatalogRefreshBypassesFreshEntry(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand", PricePerHour: 0.1, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	key := QuoteKey{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand"}
	if _, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	src.set(Quote{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand", PricePerHour: 0.11, Currency: "USD"}, nil)
	if _, err := c.Refresh(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := src.calls.Load(); got != 2 {
		t.Fatalf("provider called %d times, want 2 (Refresh must bypass freshness)", got)
	}
	q, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.11 {
		t.Fatalf("price %v want refreshed 0.11", q.PricePerHour)
	}
}

func TestRefresherPrefetchesAndStops(t *testing.T) {
	src := &countingSource{cloud: CloudAWS}
	src.set(Quote{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand", PricePerHour: 0.1, Currency: "USD"}, nil)
	c := NewCatalog(time.Hour, discardLog()).WithLive(src)

	ctx, cancel := context.WithCancel(context.Background())
	r := &Refresher{
		Catalog:  c,
		Interval: time.Hour, // only the startup prefetch runs in this test
		Targets: []QuoteKey{
			{Cloud: CloudAWS, SKU: "m5.large", Region: "us-east-1", Lifecycle: "on-demand"},
		},
		Log: discardLog(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	deadline := time.After(5 * time.Second)
	for src.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("refresher never prefetched the target")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher did not stop on context cancellation")
	}
	// The prefetch warmed the cache — a Quote now must not touch the
	// provider again.
	before := src.calls.Load()
	if _, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	if src.calls.Load() != before {
		t.Fatal("Quote after prefetch hit the provider instead of the cache")
	}
}

// --- httptest fakes for the three real provider clients -------------

const awsIndexFixture = `{
  "products": {
    "ABC123": {
      "sku": "ABC123",
      "attributes": {
        "instanceType": "m5.large",
        "tenancy": "Shared",
        "operatingSystem": "Linux",
        "preInstalledSw": "NA",
        "capacitystatus": "Used"
      }
    }
  },
  "terms": {
    "OnDemand": {
      "ABC123": {
        "ABC123.JRTCKXETXF": {
          "priceDimensions": {
            "ABC123.JRTCKXETXF.6YS6EN2CT7": {
              "unit": "Hrs",
              "pricePerUnit": {"USD": "0.0990000000"}
            }
          }
        }
      }
    }
  }
}`

func TestCatalogWithFakeAWS(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if want := "/offers/v1.0/aws/AmazonEC2/current/us-east-1/index.json"; r.URL.Path != want {
			t.Errorf("path %q want %q", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(awsIndexFixture))
	}))
	defer srv.Close()

	c := NewCatalog(time.Hour, discardLog()).WithLive(&AWS{Endpoint: srv.URL})
	q, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.099 {
		t.Fatalf("price %v want live 0.099 (static is 0.096)", q.PricePerHour)
	}
	// Second quote: cache, not another offer-file download.
	if _, err := c.Quote(context.Background(), CloudAWS, "m5.large", "us-east-1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("aws endpoint hit %d times, want 1", got)
	}
}

const gcpSkusFixture = `{
  "skus": [
    {
      "description": "N2 Predefined Instance Core running in Americas",
      "category": {"resourceFamily": "Compute", "resourceGroup": "N2Standard", "usageType": "OnDemand"},
      "serviceRegions": ["us-central1"],
      "pricingInfo": [
        {"pricingExpression": {"tieredRates": [
          {"unitPrice": {"currencyCode": "USD", "units": "0", "nanos": 199000000}}
        ]}}
      ]
    }
  ],
  "nextPageToken": ""
}`

func TestCatalogWithFakeGCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.URL.Query().Get("key"); key != "test-key" {
			t.Errorf("api key %q want %q", key, "test-key")
		}
		_, _ = w.Write([]byte(gcpSkusFixture))
	}))
	defer srv.Close()

	c := NewCatalog(time.Hour, discardLog()).WithLive(&GCP{APIKey: "test-key", Endpoint: srv.URL})
	q, err := c.Quote(context.Background(), CloudGCP, "n2-standard-4", "us-central1", "on-demand")
	if err != nil {
		t.Fatal(err)
	}
	if q.PricePerHour != 0.199 {
		t.Fatalf("price %v want live 0.199 (static is 0.194)", q.PricePerHour)
	}
}

func TestCatalogWithFakeAzureOutageFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewCatalog(time.Hour, discardLog()).WithLive(&Azure{Endpoint: srv.URL})
	q, err := c.Quote(context.Background(), CloudAzure, "Standard_D4s_v5", "westeurope", "on-demand")
	if err != nil {
		t.Fatalf("provider 503 must not fail a static-answerable quote: %v", err)
	}
	if q.PricePerHour != 0.192 {
		t.Fatalf("price %v want static 0.192", q.PricePerHour)
	}
}
