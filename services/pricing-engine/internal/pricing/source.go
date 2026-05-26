// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import "context"

type Cloud string

const (
	CloudAWS   Cloud = "aws"
	CloudGCP   Cloud = "gcp"
	CloudAzure Cloud = "azure"
)

// Quote is the normalized per-second price for an instance SKU.
type Quote struct {
	Cloud        Cloud
	SKU          string // e.g. "m5.large", "Standard_D4s_v5", "n2-standard-4"
	Region       string
	Lifecycle    string // "on-demand" | "spot" | "savings-plan" | "committed"
	PricePerHour float64
	Currency     string
}

// Source resolves prices for a given cloud. Static, AWS, GCP, and Azure
// implementations each satisfy this interface.
type Source interface {
	Name() Cloud
	Quote(ctx context.Context, sku, region, lifecycle string) (Quote, error)
}
