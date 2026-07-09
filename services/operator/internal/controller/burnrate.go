// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package controller

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// BurnRateProvider returns the current burn rate (× 1000) for a budget
// over the given window, e.g. 1500 means "spending at 1.5× budget".
//
// Implementations:
//   - ControlPlaneBurnRate — asks the control-plane's GetBurnRate RPC
//     (ClickHouse-backed) via the generated connect client (production).
//   - StubBurnRate — always 0 (chart smoke tests, kind demo, unit tests).
//
// We keep the surface tiny so the reconciler is testable without
// pulling ClickHouse client + queries into the operator module.
type BurnRateProvider interface {
	// BurnRateMilli reads the most recent burn rate over `window`
	// (e.g. "5m", "1h") for the budget identified by namespace/name.
	// Returns 0 + nil error when no data is available — the operator
	// treats that as "below trigger". A non-nil error means the source
	// is unreachable; the reconciler maps that to Tripped=Unknown, so
	// a policy can never trip on missing data either way.
	BurnRateMilli(ctx context.Context, namespace, name, window string) (int32, error)
}

// StubBurnRate always reports 0. The operator runs with this when the
// control-plane endpoint is unset, so policies stay observe-only and
// `helm test kubehero` does not require a live data plane.
type StubBurnRate struct{}

func (StubBurnRate) BurnRateMilli(_ context.Context, _, _, _ string) (int32, error) {
	return 0, nil
}

// FixedBurnRate is a test double — useful for table-driven reconciler
// tests where you want a specific reading deterministically.
type FixedBurnRate struct{ Value int32 }

func (f FixedBurnRate) BurnRateMilli(_ context.Context, _, _, _ string) (int32, error) {
	return f.Value, nil
}

// CeilingResolver maps a (namespace, budgetRef) to the parsed monthly
// USD ceiling. main.go injects a k8s-client-backed function that reads
// the matching BudgetPolicy; tests inject closures.
type CeilingResolver func(ctx context.Context, namespace, budgetRef string) (float64, error)

// burnRateReads counts burn-rate lookups by outcome so operators can
// alert on a control-plane that has gone dark (outcome="error") or has
// no data for the configured windows (outcome="unavailable") — the two
// states in which policies silently stop being able to trip.
var burnRateReads = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "kubehero_operator_burnrate_reads_total",
		Help: "Burn-rate reads by the CeilingPolicy reconciler, by outcome (ok, unavailable, error, cache_hit).",
	},
	[]string{"outcome"},
)

func init() {
	// controller-runtime's registry is what the manager's /metrics serves.
	metrics.Registry.MustRegister(burnRateReads)
}

const (
	// burnRateTimeout bounds a single GetBurnRate round trip. Reconciles
	// run every ~30s, so a slow control-plane must never stall the
	// workqueue for longer than this.
	burnRateTimeout = 5 * time.Second

	// burnRateCacheTTL is how long a reading (or a failure) is reused
	// before the control-plane is asked again — a busy reconcile loop
	// (many policies over the same scope, or rapid requeues) must not
	// hammer the control-plane.
	burnRateCacheTTL = 10 * time.Second
)

// ControlPlaneBurnRate reads burn rates from the control-plane's
// GetBurnRate RPC over Connect JSON, using the generated connect-go
// client from packages/proto.
//
// Failure semantics are deliberately conservative — the operator must
// never trip a policy on missing data:
//   - RPC error            → 0 + the error (reconciler → Tripped=Unknown) + warn log
//   - available=false      → 0 + nil ("no data" == "below trigger") + warn log
//   - unresolvable ceiling → 0 + nil (nothing meaningful to compare)
//
// Results (including failures) are cached per (namespace, budgetRef,
// window) for burnRateCacheTTL.
type ControlPlaneBurnRate struct {
	client    kuberov1connect.ControlPlaneServiceClient
	clusterID string
	resolve   CeilingResolver

	ttl time.Duration
	now func() time.Time // injectable for cache-TTL tests

	mu    sync.Mutex
	cache map[string]burnRateCacheEntry
}

type burnRateCacheEntry struct {
	milli   int32
	err     error
	expires time.Time
}

// NewControlPlaneBurnRate builds the production provider. baseURL is
// the control-plane's HTTP endpoint (CONTROL_PLANE_URL); token, when
// non-empty, is sent as a Bearer Authorization header on every call.
func NewControlPlaneBurnRate(baseURL, token, clusterID string, resolve CeilingResolver) *ControlPlaneBurnRate {
	httpClient := &http.Client{
		Timeout:   burnRateTimeout,
		Transport: bearerTransport{token: token, inner: http.DefaultTransport},
	}
	return &ControlPlaneBurnRate{
		client: kuberov1connect.NewControlPlaneServiceClient(
			httpClient,
			strings.TrimRight(baseURL, "/"),
			connect.WithProtoJSON(), // Connect JSON — same wire shape the CLI + curl use
		),
		clusterID: clusterID,
		resolve:   resolve,
		ttl:       burnRateCacheTTL,
		now:       time.Now,
		cache:     map[string]burnRateCacheEntry{},
	}
}

// bearerTransport stamps an Authorization header on outgoing requests.
type bearerTransport struct {
	token string
	inner http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.inner.RoundTrip(req)
}

func (p *ControlPlaneBurnRate) BurnRateMilli(
	ctx context.Context,
	namespace, budgetRef, window string,
) (int32, error) {
	log := logf.FromContext(ctx)

	monthlyCeiling := 0.0
	if p.resolve != nil {
		v, err := p.resolve(ctx, namespace, budgetRef)
		if err != nil {
			return 0, fmt.Errorf("resolve ceiling: %w", err)
		}
		monthlyCeiling = v
	}
	if monthlyCeiling <= 0 {
		// No parseable ceiling → no meaningful burn rate; below trigger.
		return 0, nil
	}

	key := namespace + "|" + budgetRef + "|" + window
	if milli, err, ok := p.cached(key); ok {
		burnRateReads.WithLabelValues("cache_hit").Inc()
		return milli, err
	}

	callCtx, cancel := context.WithTimeout(ctx, burnRateTimeout)
	defer cancel()
	resp, err := p.client.GetBurnRate(callCtx, connect.NewRequest(&kuberov1.GetBurnRateRequest{
		ClusterId:         p.clusterID,
		Namespace:         namespace,
		Window:            window,
		MonthlyCeilingUsd: monthlyCeiling,
	}))
	if err != nil {
		// Conservative: surface the failure (reconciler flips Tripped to
		// Unknown — never True) and remember it for a TTL so an outage
		// doesn't turn into a request storm.
		burnRateReads.WithLabelValues("error").Inc()
		wrapped := fmt.Errorf("control-plane GetBurnRate: %w", err)
		p.store(key, 0, wrapped)
		log.Error(wrapped, "burn-rate read failed; treating as no-data — policy will NOT trip",
			"namespace", namespace, "budgetRef", budgetRef, "window", window)
		return 0, wrapped
	}
	if !resp.Msg.GetAvailable() {
		burnRateReads.WithLabelValues("unavailable").Inc()
		p.store(key, 0, nil)
		log.Info("warning: control-plane reports no burn-rate data for window; treating as below trigger",
			"namespace", namespace, "budgetRef", budgetRef, "window", window,
			"source", resp.Msg.GetSource())
		return 0, nil
	}

	burnRateReads.WithLabelValues("ok").Inc()
	milli := resp.Msg.GetBurnRateMilli()
	p.store(key, milli, nil)
	return milli, nil
}

func (p *ControlPlaneBurnRate) cached(key string) (int32, error, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[key]
	if !ok || p.now().After(e.expires) {
		return 0, nil, false
	}
	return e.milli, e.err, true
}

func (p *ControlPlaneBurnRate) store(key string, milli int32, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[key] = burnRateCacheEntry{milli: milli, err: err, expires: p.now().Add(p.ttl)}
}

// SelectBurnRateProvider picks the burn-rate source for the
// CeilingPolicy reconciler. An empty endpoint keeps the stub — policies
// can then NEVER trip — so main.go logs that state loudly. The second
// return reports whether the live control-plane provider was chosen.
func SelectBurnRateProvider(
	endpoint, token, clusterID string,
	resolve CeilingResolver,
) (BurnRateProvider, bool) {
	if strings.TrimSpace(endpoint) == "" {
		return StubBurnRate{}, false
	}
	return NewControlPlaneBurnRate(endpoint, token, clusterID, resolve), true
}

// ParseCeilingUSD turns a BudgetPolicy.Spec.Ceiling string ("$100000/mo",
// "$300/hr", "5000") into a monthly USD number. Returns 0 on failure.
func ParseCeilingUSD(s string) float64 {
	s = strings.TrimSpace(s)
	m := ceilingRE.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	raw := strings.ReplaceAll(m[1], ",", "")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "hr", "hour":
		return v * 24 * 30
	default:
		return v
	}
}

var ceilingRE = regexp.MustCompile(`^\$?([0-9][0-9,]*(?:\.[0-9]+)?)(?:\s*/?\s*(mo|month|hr|hour))?$`)
