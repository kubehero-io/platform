// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package telemetry

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Network transfer list prices in $/GB (decimal GB, what the clouds
// bill). Internet egress is the first paid tier; cross-zone is the
// per-direction intra-region rate. Azure does not bill intra-region
// cross-zone traffic. Unknown clouds (on-prem, undetected) are priced
// like AWS so the number is conservative rather than zero.
var (
	defaultEgressUSDPerGB = map[string]float64{
		"aws": 0.09, "gcp": 0.12, "azure": 0.087, "": 0.09,
	}
	defaultCrossZoneUSDPerGB = map[string]float64{
		"aws": 0.01, "gcp": 0.01, "azure": 0, "": 0.01,
	}
)

// NetPricing prices flows. A non-negative override replaces the
// per-cloud default for every cloud (KUBEHERO_NET_EGRESS_USD_PER_GB,
// KUBEHERO_NET_CROSS_ZONE_USD_PER_GB).
type NetPricing struct {
	EgressOverride    float64 // < 0 = use per-cloud defaults
	CrossZoneOverride float64 // < 0 = use per-cloud defaults
}

// DefaultNetPricing uses the per-cloud list prices.
func DefaultNetPricing() NetPricing {
	return NetPricing{EgressOverride: -1, CrossZoneOverride: -1}
}

// Rates returns ($/GB egress, $/GB cross-zone) for a cloud
// (aws | gcp | azure | "" unknown).
func (p NetPricing) Rates(cloud string) (egress, crossZone float64) {
	egress, ok := defaultEgressUSDPerGB[cloud]
	if !ok {
		egress = defaultEgressUSDPerGB[""]
	}
	crossZone, ok = defaultCrossZoneUSDPerGB[cloud]
	if !ok {
		crossZone = defaultCrossZoneUSDPerGB[""]
	}
	if p.EgressOverride >= 0 {
		egress = p.EgressOverride
	}
	if p.CrossZoneOverride >= 0 {
		crossZone = p.CrossZoneOverride
	}
	return egress, crossZone
}

// FlowCost prices one flow's bytes. Internet egress and cross-zone
// transfer are exclusive in practice (an internet destination has no
// zone); if both flags are set, egress — the higher rate — applies.
func (p NetPricing) FlowCost(cloud string, bytes uint64, egress, crossZone bool) float64 {
	e, x := p.Rates(cloud)
	gb := float64(bytes) / 1e9
	switch {
	case egress:
		return gb * e
	case crossZone:
		return gb * x
	}
	return 0
}

// Zone naming conventions per cloud (topology.kubernetes.io/zone).
var (
	// us-east-1a, us-gov-west-1b, eu-central-1c, Local Zones like
	// us-east-1-bos-1a, and AZ IDs like use1-az1.
	awsZone = regexp.MustCompile(`^([a-z]{2}(-gov|-iso[a-z]*)?-[a-z]+-\d+[a-z]|[a-z]{2}-[a-z]+-\d+-[a-z]+-\d+[a-z]|[a-z]{3,4}\d+-az\d+)$`)
	// us-central1-a, europe-west4-b, northamerica-northeast1-c.
	gcpZone = regexp.MustCompile(`^[a-z]+-[a-z]+\d+-[a-z]$`)
	// eastus-1, westeurope-3, eastus2-2 (AKS zone labels): Azure region
	// names always carry a compass word and zones are numbered 1-3,
	// which keeps on-prem names like rack-7 out.
	azureZone = regexp.MustCompile(`^[a-z]*(east|west|north|south|central)[a-z]*\d?-[1-3]$`)
)

// CloudFromZone guesses the cloud from a zone name; "" when the name
// matches no convention.
func CloudFromZone(zone string) string {
	switch {
	case zone == "":
		return ""
	case gcpZone.MatchString(zone):
		return "gcp"
	case awsZone.MatchString(zone):
		return "aws"
	case azureZone.MatchString(zone):
		return "azure"
	}
	return ""
}

// CloudLookup returns the cloud a cluster runs on ("" when unknown).
type CloudLookup func(ctx context.Context, clusterID string) (string, error)

// CloudResolver caches which cloud each cluster runs on so flows can
// be priced with that cloud's rates. The source of truth is the cost
// plane (the collector reads spec.providerID into node samples); zone
// names are the fallback when a cluster has not reported node cost yet.
type CloudResolver struct {
	Lookup CloudLookup // nil = zone heuristic only
	TTL    time.Duration

	mu sync.Mutex
	m  map[string]cloudEntry
}

type cloudEntry struct {
	cloud   string
	expires time.Time
}

// maxCloudCacheClusters bounds the cache; clusters are few, so a full
// cache is simply reset.
const maxCloudCacheClusters = 4096

// Cloud resolves the cloud for a cluster, falling back to the zones
// seen on the flow's endpoints.
func (r *CloudResolver) Cloud(ctx context.Context, clusterID string, zones ...string) string {
	cloud := r.cached(ctx, clusterID)
	if cloud != "" {
		return cloud
	}
	for _, z := range zones {
		if c := CloudFromZone(z); c != "" {
			return c
		}
	}
	return ""
}

func (r *CloudResolver) cached(ctx context.Context, clusterID string) string {
	if r == nil || r.Lookup == nil || clusterID == "" {
		return ""
	}
	now := time.Now()
	r.mu.Lock()
	if e, ok := r.m[clusterID]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.cloud
	}
	r.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cloud, err := r.Lookup(lctx, clusterID)
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if err != nil || cloud == "" {
		// Retry sooner: the cluster may simply not have reported yet.
		ttl = time.Minute
		cloud = ""
	}
	cloud = strings.ToLower(cloud)
	r.mu.Lock()
	if r.m == nil || len(r.m) >= maxCloudCacheClusters {
		r.m = make(map[string]cloudEntry)
	}
	r.m[clusterID] = cloudEntry{cloud: cloud, expires: now.Add(ttl)}
	r.mu.Unlock()
	return cloud
}
