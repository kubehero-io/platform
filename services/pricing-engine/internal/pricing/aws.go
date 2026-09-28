// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AWS implements Source against the AWS Price List bulk API
// (https://aws.amazon.com/pricing/aws-price-list-api/). The API is
// public — no credentials needed to read on-demand rates — so this
// works in production with zero IAM setup. We accept an optional
// HTTPClient + Endpoint so tests can swap in an httptest server.
//
// SKU: AWS uses instance-type strings like "m5.large", "p4d.24xlarge".
// Region: AWS region code, e.g. "us-east-1".
// Lifecycle: "on-demand" only via the public price list. Spot prices
// require the Spot Pricing History API (signed); when callers ask for
// spot we fall back to ErrUnimplemented and let Static answer.
//
// The only credential-free source is the regional EC2 offer file,
// which runs to hundreds of MB (us-east-1 is ~460 MB). It is never
// held in memory: one streaming pass per region extracts the on-demand
// Linux hourly rate of every instance type (a few KB), and reading
// stops once the on-demand terms are done. The sheet is reused for TTL,
// so N instance types in a region cost one download, not N.
//
// A download runs detached from the caller. Quote waits at most MaxWait
// for a region still downloading and then returns ErrWarming (the
// Catalog answers from its stale cache or the static table) while the
// download completes for everyone after it; the Refresher calls Warm,
// which waits for the whole download, so in steady state requests only
// ever read a ready sheet.
type AWS struct {
	HTTPClient *http.Client
	Endpoint   string // defaults to the public price-list API
	// TTL is how long a region's price sheet is reused. Default 12h.
	TTL time.Duration
	// FetchTimeout bounds one region download. Default 5m.
	FetchTimeout time.Duration
	// RetryAfter is how long a failed download is remembered before the
	// next attempt, so a provider outage isn't hammered. Default 5m.
	RetryAfter time.Duration
	// MaxWait is how long Quote waits for a region that is still
	// downloading before returning ErrWarming. Default 15s.
	MaxWait time.Duration

	mu      sync.Mutex
	regions map[string]*awsSheet
	now     func() time.Time // tests
}

// awsSheet is one region's on-demand price sheet, or its in-flight
// download. prices and err are written once, before done is closed.
type awsSheet struct {
	done    chan struct{}
	prices  map[string]float64 // instance type → USD/hour
	err     error
	expires time.Time // guarded by AWS.mu
}

const (
	awsDefaultEndpoint     = "https://pricing.us-east-1.amazonaws.com"
	awsDefaultFetchTimeout = 5 * time.Minute
	awsDefaultRetryAfter   = 5 * time.Minute
	awsDefaultMaxWait      = 15 * time.Second
)

func NewAWS() *AWS { return &AWS{} }

func (*AWS) Name() Cloud { return CloudAWS }

func (a *AWS) Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error) {
	if strings.ToLower(lifecycle) != "on-demand" {
		// The public price list only carries on-demand. Spot needs
		// the EC2 SpotPriceHistory API; reserved/SP need the
		// pricing/savingsplans family. Both demand IAM (signed
		// requests via the AWS SDK, which we deliberately don't
		// depend on). Defer to Static for those until creds are
		// wired.
		return Quote{}, fmt.Errorf(
			"aws live pricing supports only the %q lifecycle (requested %q); spot, savings-plan, and committed require signed AWS APIs and fall back to the static table when known: %w",
			"on-demand", lifecycle, ErrUnimplemented)
	}
	maxWait := a.MaxWait
	if maxWait <= 0 {
		maxWait = awsDefaultMaxWait
	}
	prices, err := a.sheet(ctx, region, maxWait)
	if err != nil {
		return Quote{}, err
	}
	hourly, ok := prices[sku]
	if !ok {
		return Quote{}, ErrNotFound
	}
	return Quote{
		Cloud: CloudAWS, SKU: sku, Region: region,
		Lifecycle: "on-demand", PricePerHour: hourly, Currency: "USD",
	}, nil
}

// Warm downloads region's price sheet if it isn't fresh and waits for
// it (bounded by ctx and FetchTimeout). It implements Warmer.
func (a *AWS) Warm(ctx context.Context, region string) error {
	_, err := a.sheet(ctx, region, 0)
	return err
}

// sheet returns region's price sheet, starting a download when there is
// none, it expired, or the last attempt failed more than RetryAfter ago.
// maxWait > 0 caps how long it waits for a download in flight.
func (a *AWS) sheet(ctx context.Context, region string, maxWait time.Duration) (map[string]float64, error) {
	now := a.clock()
	a.mu.Lock()
	if a.regions == nil {
		a.regions = map[string]*awsSheet{}
	}
	s := a.regions[region]
	if s == nil || (isClosed(s.done) && !now.Before(s.expires)) {
		s = &awsSheet{done: make(chan struct{})}
		a.regions[region] = s
		go a.fetch(s, region)
	}
	a.mu.Unlock()

	var capped <-chan time.Time
	if maxWait > 0 {
		t := time.NewTimer(maxWait)
		defer t.Stop()
		capped = t.C
	}
	select {
	case <-s.done:
		return s.prices, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-capped:
		return nil, fmt.Errorf("aws price sheet for %s is still downloading: %w", region, ErrWarming)
	}
}

func (a *AWS) fetch(s *awsSheet, region string) {
	timeout := a.FetchTimeout
	if timeout <= 0 {
		timeout = awsDefaultFetchTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	prices, err := a.download(ctx, region)

	ttl := a.TTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	if err != nil {
		if ttl = a.RetryAfter; ttl <= 0 {
			ttl = awsDefaultRetryAfter
		}
	}
	a.mu.Lock()
	s.prices, s.err = prices, err
	s.expires = a.clock().Add(ttl)
	a.mu.Unlock()
	close(s.done)
}

func (a *AWS) download(ctx context.Context, region string) (map[string]float64, error) {
	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient // bounded by ctx (FetchTimeout)
	}
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = awsDefaultEndpoint
	}
	u, err := url.Parse(fmt.Sprintf("%s/offers/v1.0/aws/AmazonEC2/current/%s/index.json",
		endpoint, url.PathEscape(region)))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aws pricing fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("aws pricing http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	prices, err := parseEC2OnDemand(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("aws pricing decode %s: %w", region, err)
	}
	return prices, nil
}

func (a *AWS) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// parseEC2OnDemand streams an EC2 offer file and returns the on-demand
// hourly USD rate of every Linux, shared-tenancy, no-preinstalled-
// software instance type. Products and terms are decoded one entry at
// a time and discarded unless they match, and it returns as soon as
// terms.OnDemand has been read (the Reserved terms that follow are
// never touched), so memory stays proportional to the number of
// instance types, not the size of the file.
//
// The offer file lists "products" before "terms"; an OnDemand term for
// a product not seen yet is skipped.
func parseEC2OnDemand(r io.Reader) (map[string]float64, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	instanceOf := map[string]string{} // product SKU → instance type
	for dec.More() {
		key, err := keyToken(dec)
		if err != nil {
			return nil, err
		}
		switch key {
		case "products":
			if err := readEC2Products(dec, instanceOf); err != nil {
				return nil, fmt.Errorf("products: %w", err)
			}
		case "terms":
			prices, err := readEC2OnDemandTerms(dec, instanceOf)
			if err != nil {
				return nil, fmt.Errorf("terms: %w", err)
			}
			if len(prices) == 0 {
				return nil, errors.New("no on-demand Linux instance prices in the offer file")
			}
			return prices, nil
		default:
			if err := skipValue(dec); err != nil {
				return nil, err
			}
		}
	}
	return nil, errors.New("offer file has no terms")
}

func readEC2Products(dec *json.Decoder, instanceOf map[string]string) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		if _, err := keyToken(dec); err != nil {
			return err
		}
		var p struct {
			SKU        string `json:"sku"`
			Attributes struct {
				InstanceType    string `json:"instanceType"`
				Tenancy         string `json:"tenancy"`
				OperatingSystem string `json:"operatingSystem"`
				PreInstalledSw  string `json:"preInstalledSw"`
				CapacityStatus  string `json:"capacitystatus"`
			} `json:"attributes"`
		}
		if err := dec.Decode(&p); err != nil {
			return err
		}
		at := p.Attributes
		if p.SKU != "" && at.InstanceType != "" && at.Tenancy == "Shared" &&
			at.OperatingSystem == "Linux" && at.PreInstalledSw == "NA" && at.CapacityStatus == "Used" {
			instanceOf[p.SKU] = at.InstanceType
		}
	}
	return expectDelim(dec, '}')
}

// readEC2OnDemandTerms reads the "terms" object up to the end of its
// OnDemand member and returns the hourly rate per instance type (the
// lowest positive one when several products map to the same type).
func readEC2OnDemandTerms(dec *json.Decoder, instanceOf map[string]string) (map[string]float64, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	for dec.More() {
		key, err := keyToken(dec)
		if err != nil {
			return nil, err
		}
		if key != "OnDemand" {
			if err := skipValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		if err := expectDelim(dec, '{'); err != nil {
			return nil, err
		}
		prices := map[string]float64{}
		for dec.More() {
			sku, err := keyToken(dec)
			if err != nil {
				return nil, err
			}
			instanceType, ok := instanceOf[sku]
			if !ok {
				var skip json.RawMessage // one product's offers: small
				if err := dec.Decode(&skip); err != nil {
					return nil, err
				}
				continue
			}
			var offers map[string]struct {
				PriceDimensions map[string]struct {
					PricePerUnit map[string]string `json:"pricePerUnit"`
					Unit         string            `json:"unit"`
				} `json:"priceDimensions"`
			}
			if err := dec.Decode(&offers); err != nil {
				return nil, err
			}
			for _, offer := range offers {
				for _, dim := range offer.PriceDimensions {
					if !strings.HasPrefix(strings.ToLower(dim.Unit), "hr") {
						continue
					}
					v, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
					if err != nil || v <= 0 {
						continue
					}
					if cur, seen := prices[instanceType]; !seen || v < cur {
						prices[instanceType] = v
					}
				}
			}
		}
		return prices, nil // Reserved terms follow; nothing else needed
	}
	return nil, errors.New("no OnDemand terms")
}

func keyToken(dec *json.Decoder) (string, error) {
	t, err := dec.Token()
	if err != nil {
		return "", err
	}
	k, ok := t.(string)
	if !ok {
		return "", fmt.Errorf("want an object key, got %v", t)
	}
	return k, nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("want %q, got %v", want, t)
	}
	return nil
}

// skipValue consumes the next value token by token, so skipping a large
// object never buffers it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

// ErrUnimplemented is returned when the source can't answer a particular
// lifecycle or SKU shape (e.g. Spot via the public price list).
var ErrUnimplemented = errors.New("pricing source: not implemented for this lifecycle")

// ErrNotFound is returned when the source can't find a quote for the
// given SKU + region.
var ErrNotFound = errors.New("pricing source: SKU not found")

// ErrWarming is returned while a source is still loading the data a
// quote needs; the Catalog falls back without treating it as an outage.
var ErrWarming = errors.New("pricing source: still loading")
