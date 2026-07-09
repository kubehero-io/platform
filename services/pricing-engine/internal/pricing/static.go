// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Static is a hand-maintained price map used in tests and as a fallback.
type Static struct {
	cloud Cloud
	table map[string]float64 // key = sku|region|lifecycle
}

func NewStatic(cloud Cloud) *Static {
	s := &Static{cloud: cloud, table: map[string]float64{}}
	// A few representative entries per cloud.
	switch cloud {
	case CloudAWS:
		s.table["m5.large|us-east-1|on-demand"] = 0.096
		s.table["p4d.24xlarge|us-east-1|on-demand"] = 32.77
		s.table["m5.large|us-east-1|spot"] = 0.029
	case CloudGCP:
		s.table["n2-standard-4|us-central1|on-demand"] = 0.194
		s.table["a2-highgpu-1g|us-central1|on-demand"] = 3.67
	case CloudAzure:
		s.table["Standard_D4s_v5|westeurope|on-demand"] = 0.192
		s.table["Standard_NC24ads_A100_v4|westeurope|on-demand"] = 4.10
	}
	return s
}

func (s *Static) Name() Cloud { return s.cloud }

func (s *Static) Quote(_ context.Context, sku, region, lifecycle string) (Quote, error) {
	key := fmt.Sprintf("%s|%s|%s", sku, region, lifecycle)
	price, ok := s.table[key]
	if !ok {
		return Quote{}, fmt.Errorf("%w: %s not in %s static table", ErrNotFound, key, s.cloud)
	}
	return Quote{
		Cloud:        s.cloud,
		SKU:          sku,
		Region:       region,
		Lifecycle:    lifecycle,
		PricePerHour: price,
		Currency:     "USD",
	}, nil
}

// Entries lists every price point in the table as QuoteKeys, sorted
// for determinism. The Catalog uses these as the "popular SKUs" the
// background Refresher keeps warm.
func (s *Static) Entries() []QuoteKey {
	out := make([]QuoteKey, 0, len(s.table))
	for k := range s.table {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, QuoteKey{Cloud: s.cloud, SKU: parts[0], Region: parts[1], Lifecycle: parts[2]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
