// SPDX-License-Identifier: BUSL-1.1
package pricing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ec2OfferFile is a trimmed regional offer file in AWS's key order.
// Only the Linux / Shared / NA / Used products count; the Reserved
// terms after OnDemand are deliberately not valid JSON, so a parser
// that reads past OnDemand fails the test.
const ec2OfferFile = `{
  "formatVersion": "v1.0",
  "disclaimer": "fixture",
  "offerCode": "AmazonEC2",
  "publicationDate": "2026-09-01T00:00:00Z",
  "products": {
    "LINUX_M5L":  {"sku": "LINUX_M5L",  "productFamily": "Compute Instance", "attributes": {"instanceType": "m5.large",  "tenancy": "Shared",    "operatingSystem": "Linux",   "preInstalledSw": "NA", "capacitystatus": "Used"}},
    "LINUX_M5L2": {"sku": "LINUX_M5L2", "productFamily": "Compute Instance", "attributes": {"instanceType": "m5.large",  "tenancy": "Shared",    "operatingSystem": "Linux",   "preInstalledSw": "NA", "capacitystatus": "Used"}},
    "WIN_M5L":    {"sku": "WIN_M5L",    "productFamily": "Compute Instance", "attributes": {"instanceType": "m5.large",  "tenancy": "Shared",    "operatingSystem": "Windows", "preInstalledSw": "NA", "capacitystatus": "Used"}},
    "DED_M5L":    {"sku": "DED_M5L",    "productFamily": "Compute Instance", "attributes": {"instanceType": "m5.large",  "tenancy": "Dedicated", "operatingSystem": "Linux",   "preInstalledSw": "NA", "capacitystatus": "Used"}},
    "LINUX_C5X":  {"sku": "LINUX_C5X",  "productFamily": "Compute Instance", "attributes": {"instanceType": "c5.xlarge", "tenancy": "Shared",    "operatingSystem": "Linux",   "preInstalledSw": "NA", "capacitystatus": "Used"}},
    "XFER":       {"sku": "XFER",       "productFamily": "Data Transfer",    "attributes": {"transferType": "InterRegion Outbound"}}
  },
  "terms": {
    "OnDemand": {
      "WIN_M5L":    {"WIN_M5L.T":    {"priceDimensions": {"WIN_M5L.T.D":    {"unit": "Hrs", "pricePerUnit": {"USD": "0.1880000000"}}}}},
      "LINUX_M5L":  {"LINUX_M5L.T":  {"priceDimensions": {"LINUX_M5L.T.D":  {"unit": "Hrs", "pricePerUnit": {"USD": "0.0960000000"}}}}},
      "LINUX_M5L2": {"LINUX_M5L2.T": {"priceDimensions": {"LINUX_M5L2.T.D": {"unit": "Hrs", "pricePerUnit": {"USD": "0.5000000000"}}}}},
      "DED_M5L":    {"DED_M5L.T":    {"priceDimensions": {"DED_M5L.T.D":    {"unit": "Hrs", "pricePerUnit": {"USD": "0.1060000000"}}}}},
      "LINUX_C5X":  {"LINUX_C5X.T":  {"priceDimensions": {"LINUX_C5X.T.D":  {"unit": "Hrs", "pricePerUnit": {"USD": "0.1700000000"}}}}},
      "XFER":       {"XFER.T":       {"priceDimensions": {"XFER.T.D":       {"unit": "GB",  "pricePerUnit": {"USD": "0.0200000000"}}}}}
    },
    "Reserved": {not json at all
`

func TestParseEC2OnDemandStreamsAndStopsAfterOnDemand(t *testing.T) {
	prices, err := parseEC2OnDemand(strings.NewReader(ec2OfferFile))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"m5.large": 0.096, "c5.xlarge": 0.17}
	if len(prices) != len(want) {
		t.Fatalf("prices = %v, want %v", prices, want)
	}
	for k, v := range want {
		if prices[k] != v {
			t.Errorf("%s = %v, want %v (Linux, shared tenancy, lowest rate)", k, prices[k], v)
		}
	}
}

func TestParseEC2OnDemandRejectsFilesWithoutPrices(t *testing.T) {
	for name, body := range map[string]string{
		"no terms":    `{"products": {}}`,
		"no ondemand": `{"products": {}, "terms": {"Reserved": {}}}`,
		"no matches":  `{"products": {"W": {"sku": "W", "attributes": {"instanceType": "m5.large", "operatingSystem": "Windows"}}}, "terms": {"OnDemand": {}}}`,
		"not json":    `<html>`,
	} {
		if _, err := parseEC2OnDemand(strings.NewReader(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// offerServer serves ec2OfferFile per region and counts downloads.
type offerServer struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	gate  chan struct{} // when non-nil, responses wait for it to close
	fail  atomic.Bool
	total atomic.Int64
}

func newOfferServer(t *testing.T) *offerServer {
	s := &offerServer{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.total.Add(1)
		s.mu.Lock()
		s.hits[r.URL.Path]++
		gate := s.gate
		s.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if s.fail.Load() {
			http.Error(w, "price list unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(ec2OfferFile))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *offerServer) downloads(region string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits["/offers/v1.0/aws/AmazonEC2/current/"+region+"/index.json"]
}

func TestAWSQuoteDownloadsEachRegionOnce(t *testing.T) {
	srv := newOfferServer(t)
	a := &AWS{Endpoint: srv.URL}
	ctx := context.Background()

	for _, tc := range []struct {
		sku, region string
		want        float64
	}{
		{"m5.large", "us-east-1", 0.096},
		{"c5.xlarge", "us-east-1", 0.17},
		{"m5.large", "us-east-1", 0.096},
		{"m5.large", "eu-west-1", 0.096},
	} {
		q, err := a.Quote(ctx, tc.sku, tc.region, "On-Demand")
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.sku, tc.region, err)
		}
		if q.PricePerHour != tc.want || q.Cloud != CloudAWS || q.Lifecycle != "on-demand" || q.Currency != "USD" {
			t.Fatalf("%s/%s: %+v", tc.sku, tc.region, q)
		}
	}
	if got := srv.downloads("us-east-1"); got != 1 {
		t.Errorf("us-east-1 downloaded %d times, want 1", got)
	}
	if got := srv.downloads("eu-west-1"); got != 1 {
		t.Errorf("eu-west-1 downloaded %d times, want 1", got)
	}
}

func TestAWSQuoteUnsupportedLifecycleAndUnknownType(t *testing.T) {
	srv := newOfferServer(t)
	a := &AWS{Endpoint: srv.URL}
	ctx := context.Background()

	if _, err := a.Quote(ctx, "m5.large", "us-east-1", "spot"); !errors.Is(err, ErrUnimplemented) {
		t.Fatalf("spot: err = %v, want ErrUnimplemented", err)
	}
	if srv.total.Load() != 0 {
		t.Fatal("spot must not download the offer file")
	}
	if _, err := a.Quote(ctx, "z9.huge", "us-east-1", "on-demand"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown type: err = %v, want ErrNotFound", err)
	}
}

func TestAWSQuoteReturnsErrWarmingWhileDownloadingThenServes(t *testing.T) {
	srv := newOfferServer(t)
	gate := make(chan struct{})
	srv.mu.Lock()
	srv.gate = gate
	srv.mu.Unlock()
	a := &AWS{Endpoint: srv.URL, MaxWait: 20 * time.Millisecond}
	ctx := context.Background()

	if _, err := a.Quote(ctx, "m5.large", "us-east-1", "on-demand"); !errors.Is(err, ErrWarming) {
		t.Fatalf("cold quote: err = %v, want ErrWarming", err)
	}
	// A caller whose context ends first gets its own error; the
	// download carries on regardless.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.Quote(cctx, "m5.large", "us-east-1", "on-demand"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: err = %v, want context.Canceled", err)
	}

	close(gate)
	if err := a.Warm(ctx, "us-east-1"); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	q, err := a.Quote(ctx, "m5.large", "us-east-1", "on-demand")
	if err != nil || q.PricePerHour != 0.096 {
		t.Fatalf("warm quote = %+v, %v", q, err)
	}
	if got := srv.downloads("us-east-1"); got != 1 {
		t.Errorf("downloaded %d times, want 1 (callers share the download)", got)
	}
}

func TestAWSFailedDownloadIsRememberedUntilRetryAfter(t *testing.T) {
	srv := newOfferServer(t)
	srv.fail.Store(true)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	a := &AWS{Endpoint: srv.URL, RetryAfter: time.Minute, TTL: time.Hour, now: func() time.Time { return now }}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := a.Quote(ctx, "m5.large", "us-east-1", "on-demand")
		if err == nil || !strings.Contains(err.Error(), "http 503") {
			t.Fatalf("quote #%d: err = %v, want the http 503", i, err)
		}
	}
	if got := srv.downloads("us-east-1"); got != 1 {
		t.Fatalf("downloaded %d times during the outage, want 1", got)
	}

	srv.fail.Store(false)
	now = now.Add(2 * time.Minute) // past RetryAfter
	if q, err := a.Quote(ctx, "m5.large", "us-east-1", "on-demand"); err != nil || q.PricePerHour != 0.096 {
		t.Fatalf("after RetryAfter: %+v, %v", q, err)
	}
	now = now.Add(30 * time.Minute) // within TTL: served from the sheet
	if _, err := a.Quote(ctx, "c5.xlarge", "us-east-1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour) // past TTL: downloaded again
	if _, err := a.Quote(ctx, "c5.xlarge", "us-east-1", "on-demand"); err != nil {
		t.Fatal(err)
	}
	if got := srv.downloads("us-east-1"); got != 3 {
		t.Fatalf("downloads = %d, want 3 (failure, retry, TTL refresh)", got)
	}
}

func TestCatalogFallsBackQuietlyWhileAWSWarmsAndRefresherWarms(t *testing.T) {
	srv := newOfferServer(t)
	gate := make(chan struct{})
	srv.mu.Lock()
	srv.gate = gate
	srv.mu.Unlock()
	aws := &AWS{Endpoint: srv.URL, MaxWait: 20 * time.Millisecond}
	c := NewCatalog(time.Hour, discardLog()).WithLive(aws)
	ctx := context.Background()

	// Cold: m5.large falls back to the static table; c5.xlarge has no
	// static entry, so the warming error surfaces.
	if _, err := c.Quote(ctx, CloudAWS, "m5.large", "us-east-1", "on-demand"); err != nil {
		t.Fatalf("cold quote should fall back, got %v", err)
	}
	if _, err := c.Quote(ctx, CloudAWS, "c5.xlarge", "us-east-1", "on-demand"); !errors.Is(err, ErrWarming) {
		t.Fatalf("cold quote with no static entry: err = %v, want ErrWarming", err)
	}

	close(gate)
	r := &Refresher{Catalog: c, Interval: time.Hour, Targets: []QuoteKey{
		{Cloud: CloudAWS, SKU: "c5.xlarge", Region: "us-east-1", Lifecycle: "on-demand"},
	}, Log: discardLog()}
	r.refresh(ctx, discardLog())

	q, err := c.Quote(ctx, CloudAWS, "c5.xlarge", "us-east-1", "on-demand")
	if err != nil || q.PricePerHour != 0.17 {
		t.Fatalf("after warm-up: %+v, %v", q, err)
	}
	if got := srv.downloads("us-east-1"); got != 1 {
		t.Errorf("downloaded %d times, want 1", got)
	}
}
