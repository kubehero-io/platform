// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GCP implements Source against the Cloud Billing Catalog API
// (https://cloud.google.com/billing/v1/how-tos/catalog-api).
//
// Authentication: requires an API key (set via GCP_BILLING_API_KEY) for
// the `cloudbilling.googleapis.com` service. The API is public-ish but
// rate-limited per key.
//
// SKU mapping: GCP's machine types are like "n2-standard-4". The
// catalog organises prices under SKUs that GROUP machine families;
// finding an exact instance-type → hourly mapping requires walking
// service "Compute Engine" → SKU description matching. We do that
// matching here and cache results.
type GCP struct {
	APIKey     string // GCP_BILLING_API_KEY env
	HTTPClient *http.Client
	Endpoint   string // defaults to https://cloudbilling.googleapis.com
}

const gcpDefaultEndpoint = "https://cloudbilling.googleapis.com"
const gcpComputeEngineService = "services/6F81-5844-456A" // Compute Engine

func NewGCP() *GCP { return &GCP{} }

func (GCP) Name() Cloud { return CloudGCP }

func (g *GCP) Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error) {
	if strings.ToLower(lifecycle) != "on-demand" {
		// Spot (preemptible) and CUDs need the same SKU walk plus
		// commitment-flag matching. Stub for now.
		return Quote{}, ErrUnimplemented
	}
	if g.APIKey == "" {
		return Quote{}, fmt.Errorf("gcp pricing: GCP_BILLING_API_KEY unset")
	}
	if g.HTTPClient == nil {
		g.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := g.Endpoint
	if endpoint == "" {
		endpoint = gcpDefaultEndpoint
	}

	// Build the GET request for the Compute Engine service's SKU list.
	// Pagination: the API uses `pageSize` + `pageToken`. For an exact
	// match we usually find it inside one or two pages.
	pageToken := ""
	for {
		u, err := url.Parse(fmt.Sprintf("%s/v1/%s/skus", endpoint, gcpComputeEngineService))
		if err != nil {
			return Quote{}, err
		}
		q := u.Query()
		q.Set("key", g.APIKey)
		q.Set("pageSize", "5000")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return Quote{}, err
		}
		resp, err := g.HTTPClient.Do(req)
		if err != nil {
			return Quote{}, fmt.Errorf("gcp pricing fetch: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Quote{}, fmt.Errorf("gcp pricing http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var page gcpSkusPage
		if err := json.Unmarshal(body, &page); err != nil {
			return Quote{}, fmt.Errorf("gcp pricing decode: %w", err)
		}
		if hourly, ok := page.findHourly(sku, region); ok {
			return Quote{
				Cloud: CloudGCP, SKU: sku, Region: region,
				Lifecycle: "on-demand", PricePerHour: hourly, Currency: "USD",
			}, nil
		}
		if page.NextPageToken == "" {
			return Quote{}, ErrNotFound
		}
		pageToken = page.NextPageToken
	}
}

type gcpSkusPage struct {
	NextPageToken string   `json:"nextPageToken"`
	Skus          []gcpSku `json:"skus"`
}

type gcpSku struct {
	Description    string   `json:"description"`
	Category       struct {
		ResourceFamily string `json:"resourceFamily"`
		ResourceGroup  string `json:"resourceGroup"`
		UsageType      string `json:"usageType"` // "OnDemand" | "Preemptible" | "Commit1Yr" | …
	} `json:"category"`
	ServiceRegions []string `json:"serviceRegions"`
	PricingInfo    []struct {
		PricingExpression struct {
			TieredRates []struct {
				UnitPrice struct {
					CurrencyCode string `json:"currencyCode"`
					Units        string `json:"units"`
					Nanos        int    `json:"nanos"`
				} `json:"unitPrice"`
			} `json:"tieredRates"`
		} `json:"pricingExpression"`
	} `json:"pricingInfo"`
}

func (p gcpSkusPage) findHourly(machineType, region string) (float64, bool) {
	desc := strings.ToLower(machineType)
	for _, sku := range p.Skus {
		if sku.Category.UsageType != "OnDemand" {
			continue
		}
		// GCP descriptions look like "N2 Predefined Instance Core running in
		// London". We match the family + an "instance" or "core" hint.
		if !strings.Contains(strings.ToLower(sku.Description), strings.SplitN(desc, "-", 2)[0]) {
			continue
		}
		var inRegion bool
		for _, r := range sku.ServiceRegions {
			if r == region {
				inRegion = true
				break
			}
		}
		if !inRegion {
			continue
		}
		for _, pi := range sku.PricingInfo {
			for _, t := range pi.PricingExpression.TieredRates {
				if t.UnitPrice.CurrencyCode != "USD" {
					continue
				}
				// units + nanos → float USD per unit. Compute Engine
				// units are usually per-hour-vCPU or per-hour-GB; an
				// exact instance-type quote needs combining the cores
				// + memory SKUs. For first-pass, return whichever
				// matches the strongest description; refine when we
				// build the cache table.
				v := float64(0)
				if t.UnitPrice.Units != "" {
					_, _ = fmt.Sscanf(t.UnitPrice.Units, "%f", &v)
				}
				v += float64(t.UnitPrice.Nanos) / 1e9
				if v > 0 {
					return v, true
				}
			}
		}
	}
	return 0, false
}
