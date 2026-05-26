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

// Azure implements Source against the Retail Prices API
// (https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices).
//
// No authentication required — the API is public. Filtering is done
// via OData ($filter) which makes per-SKU lookups cheap. Each call
// targets the exact serviceFamily + armRegionName + skuName.
type Azure struct {
	HTTPClient *http.Client
	Endpoint   string // defaults to https://prices.azure.com
}

const azureDefaultEndpoint = "https://prices.azure.com"

func NewAzure() *Azure { return &Azure{} }

func (Azure) Name() Cloud { return CloudAzure }

func (a *Azure) Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error) {
	if strings.ToLower(lifecycle) != "on-demand" {
		return Quote{}, ErrUnimplemented
	}
	if a.HTTPClient == nil {
		a.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = azureDefaultEndpoint
	}

	// $filter narrows to Virtual Machines, Linux, on-demand. The Retail
	// Prices API returns up to 100 rows per page; for a specific SKU
	// match we usually get the answer in the first page.
	filter := fmt.Sprintf(
		"serviceName eq 'Virtual Machines' and armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'",
		azureEscape(region), azureEscape(sku),
	)
	q := url.Values{}
	q.Set("api-version", "2023-01-01-preview")
	q.Set("$filter", filter)
	full := fmt.Sprintf("%s/api/retail/prices?%s", endpoint, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return Quote{}, err
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("azure pricing fetch: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("azure pricing http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var page azureRetailPage
	if err := json.Unmarshal(body, &page); err != nil {
		return Quote{}, fmt.Errorf("azure pricing decode: %w", err)
	}
	for _, item := range page.Items {
		// Skip Windows / spot rows; we asked for Consumption + the SKU
		// name above but Azure can return multiple meters for the same
		// SKU (e.g. with vs without OS).
		if strings.Contains(strings.ToLower(item.ProductName), "windows") {
			continue
		}
		if item.UnitPrice > 0 {
			return Quote{
				Cloud: CloudAzure, SKU: sku, Region: region,
				Lifecycle: "on-demand", PricePerHour: item.UnitPrice, Currency: item.CurrencyCode,
			}, nil
		}
	}
	return Quote{}, ErrNotFound
}

type azureRetailPage struct {
	Items []struct {
		CurrencyCode string  `json:"currencyCode"`
		UnitPrice    float64 `json:"unitPrice"`
		ArmSkuName   string  `json:"armSkuName"`
		ProductName  string  `json:"productName"`
	} `json:"Items"`
	NextPageLink string `json:"NextPageLink"`
}

// azureEscape OData-escapes a single quote for the $filter expression.
func azureEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }
