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
	"time"
)

// AWS implements Source against the AWS Pricing API
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
type AWS struct {
	HTTPClient *http.Client
	Endpoint   string // defaults to the public price-list API
}

const awsDefaultEndpoint = "https://pricing.us-east-1.amazonaws.com"

func NewAWS() *AWS { return &AWS{} }

func (AWS) Name() Cloud { return CloudAWS }

func (a *AWS) Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error) {
	if strings.ToLower(lifecycle) != "on-demand" {
		// The public price list only carries on-demand. Spot needs
		// the EC2 SpotPriceHistory API; reserved/SP need the
		// pricing/savingsplans family. Both demand IAM. Defer to
		// Static for those until creds are wired.
		return Quote{}, ErrUnimplemented
	}
	if a.HTTPClient == nil {
		a.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = awsDefaultEndpoint
	}

	// AWS Pricing API offers a JSON index of every service. EC2 lives at
	//   /offers/v1.0/aws/AmazonEC2/current/<region>/index.json
	// The full file is huge (~50MB); we filter by SKU client-side.
	// In production, the pricing-engine pre-builds a per-SKU cache via
	// the CronJob; this method is the fallback for cache misses.
	u, err := url.Parse(fmt.Sprintf("%s/offers/v1.0/aws/AmazonEC2/current/%s/index.json", endpoint, region))
	if err != nil {
		return Quote{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Quote{}, err
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("aws pricing fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Quote{}, fmt.Errorf("aws pricing http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var idx awsPriceIndex
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		return Quote{}, fmt.Errorf("aws pricing decode: %w", err)
	}
	hourly, ok := idx.findOnDemandHourly(sku)
	if !ok {
		return Quote{}, ErrNotFound
	}
	return Quote{
		Cloud: CloudAWS, SKU: sku, Region: region,
		Lifecycle: "on-demand", PricePerHour: hourly, Currency: "USD",
	}, nil
}

// awsPriceIndex partial decode — the AWS file is huge, we only keep the
// fields we need.
type awsPriceIndex struct {
	Products map[string]struct {
		SKU           string `json:"sku"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"products"`
	Terms struct {
		OnDemand map[string]map[string]struct {
			PriceDimensions map[string]struct {
				PricePerUnit map[string]string `json:"pricePerUnit"`
				Unit         string            `json:"unit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

func (idx awsPriceIndex) findOnDemandHourly(instanceType string) (float64, bool) {
	// Find the SKU(s) matching the instance type via attributes.
	var skus []string
	for _, p := range idx.Products {
		if p.Attributes["instanceType"] == instanceType &&
			p.Attributes["tenancy"] == "Shared" &&
			p.Attributes["operatingSystem"] == "Linux" &&
			p.Attributes["preInstalledSw"] == "NA" &&
			p.Attributes["capacitystatus"] == "Used" {
			skus = append(skus, p.SKU)
		}
	}
	for _, sku := range skus {
		offers, ok := idx.Terms.OnDemand[sku]
		if !ok {
			continue
		}
		for _, offer := range offers {
			for _, dim := range offer.PriceDimensions {
				if !strings.HasPrefix(strings.ToLower(dim.Unit), "hr") {
					continue
				}
				usd, ok := dim.PricePerUnit["USD"]
				if !ok {
					continue
				}
				v, err := strconv.ParseFloat(usd, 64)
				if err == nil && v > 0 {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// ErrUnimplemented is returned when the source can't answer a particular
// lifecycle or SKU shape (e.g. Spot via the public price list).
var ErrUnimplemented = errors.New("pricing source: not implemented for this lifecycle")

// ErrNotFound is returned when the source can't find a quote for the
// given SKU + region.
var ErrNotFound = errors.New("pricing source: SKU not found")
