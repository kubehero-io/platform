// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ingest

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
)

// Price sources, recorded on every NodeCostSample so a dashboard can say
// how much of the bill is a list price versus a guess.
const (
	SourceAnnotation    = "annotation"
	SourcePricingEngine = "pricing-engine"
	SourceEstimate      = "estimate"
)

// AnnotationNodeHourlyUSD pins a node's hourly price. It wins over every
// other source: it is how operators encode negotiated discounts,
// reserved-instance rates or on-prem amortisation.
const AnnotationNodeHourlyUSD = "kubehero.io/node-hourly-usd"

// Estimate coefficients, used only when neither the annotation nor the
// pricing engine can price a node. CPU and memory are the historical
// m5-family approximation; the per-accelerator term keeps GPU nodes from
// being priced like CPU boxes (it sits between T4/L4-class and
// A100-class on-demand list prices) so the GPU share of the cost model
// has something sensible to split.
const (
	estimateUSDPerCoreHour = 0.04
	estimateUSDPerGiBHour  = 0.005
	estimateUSDPerGPUHour  = 1.50
	// maxSanePrice rejects typos like "4800" for "4.800" in the
	// annotation: no single node lists above ~$100/h today.
	maxSanePrice = 1000.0
)

// Pricer resolves a node's hourly price. Quotes from the pricing engine
// are cached for an hour (list prices move monthly at most); a failed
// quote falls back to the last good quote, else the estimate, and is
// retried after a short negative-cache window so a down pricing engine
// isn't hammered from every node every scan.
type Pricer struct {
	engine  kuberov1connect.PricingServiceClient // nil: no pricing engine configured
	log     *slog.Logger
	now     func() time.Time
	ttl     time.Duration
	negTTL  time.Duration
	timeout time.Duration

	mu    sync.Mutex
	cache map[quoteKey]*quoteEntry
}

type quoteKey struct{ cloud, sku, region, lifecycle string }

type quoteEntry struct {
	price     float64 // last good quote (0 = never quoted)
	nextFetch time.Time
	warned    bool
}

// NewPricer builds a Pricer; engine may be nil.
func NewPricer(engine kuberov1connect.PricingServiceClient, log *slog.Logger) *Pricer {
	if log == nil {
		log = slog.Default()
	}
	return &Pricer{
		engine:  engine,
		log:     log,
		now:     time.Now,
		ttl:     time.Hour,
		negTTL:  5 * time.Minute,
		timeout: 5 * time.Second,
		cache:   map[quoteKey]*quoteEntry{},
	}
}

// Price returns the node's $/hour and where it came from.
func (p *Pricer) Price(ctx context.Context, n *corev1.Node) (float64, string) {
	if n == nil {
		return 0, SourceEstimate
	}
	if v, ok := parsePrice(n.Annotations[AnnotationNodeHourlyUSD]); ok {
		return v, SourceAnnotation
	}
	if p.engine != nil {
		key := quoteKey{cloud: kube.Cloud(n), sku: kube.SKU(n), region: kube.Region(n), lifecycle: kube.Lifecycle(n)}
		if key.cloud != "" && key.sku != "" && key.region != "" {
			if v, ok := p.quote(ctx, key); ok {
				return v, SourcePricingEngine
			}
		}
	}
	return Estimate(n), SourceEstimate
}

func (p *Pricer) quote(ctx context.Context, key quoteKey) (float64, bool) {
	p.mu.Lock()
	e := p.cache[key]
	if e == nil {
		e = &quoteEntry{}
		p.cache[key] = e
	}
	now := p.now()
	if now.Before(e.nextFetch) {
		price := e.price
		p.mu.Unlock()
		return price, price > 0
	}
	// Claim the refresh so concurrent callers use the current value
	// meanwhile instead of stampeding the engine.
	e.nextFetch = now.Add(p.negTTL)
	p.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	resp, err := p.engine.Quote(cctx, connect.NewRequest(&kuberov1.QuoteRequest{
		Cloud: key.cloud, Sku: key.sku, Region: key.region, Lifecycle: key.lifecycle,
	}))

	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		price := resp.Msg.GetPricePerHour()
		cur := strings.ToUpper(resp.Msg.GetCurrency())
		if price > 0 && price < maxSanePrice && !math.IsNaN(price) && (cur == "" || cur == "USD") {
			e.price, e.nextFetch, e.warned = price, p.now().Add(p.ttl), false
			return price, true
		}
		err = connect.NewError(connect.CodeDataLoss, errUnusableQuote)
	}
	if !e.warned {
		e.warned = true
		p.log.Warn("pricing engine quote failed — using last good quote or estimate",
			"cloud", key.cloud, "sku", key.sku, "region", key.region, "lifecycle", key.lifecycle,
			"has_stale_quote", e.price > 0, "err", err)
	}
	return e.price, e.price > 0
}

type quoteError string

func (e quoteError) Error() string { return string(e) }

const errUnusableQuote = quoteError("quote is not a usable positive USD price")

// Estimate is the fallback price scaled to the node's allocatable.
func Estimate(n *corev1.Node) float64 {
	cores := float64(kube.AllocatableCPUMillis(n)) / 1000
	gib := float64(kube.AllocatableMemBytes(n)) / (1 << 30)
	if cores < 1 {
		cores = 1
	}
	if gib < 1 {
		gib = 1
	}
	return cores*estimateUSDPerCoreHour + gib*estimateUSDPerGiBHour + float64(kube.NodeGPUs(n))*estimateUSDPerGPUHour
}

// parsePrice accepts a plain positive decimal ("0.192", "1.5e0").
func parsePrice(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v >= maxSanePrice {
		return 0, false
	}
	return v, true
}
