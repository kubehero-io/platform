// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package rightsizing turns measured container usage into VPA-class
// request recommendations, priced with what the workload actually
// costs.
//
// Inputs per (cluster, namespace, workload, container) over the
// observation window: t-digest percentiles and maxima of CPU and
// working-set memory from container_usage_5m, the current requests and
// limits, OOM kills from cluster_events, and the workload's $/core-hour
// and $/GiB-hour from workload_cost_1h.
//
// The rules, in order of precedence:
//
//	cpu  = chosen percentile (p95 by default) × (1 + headroom),
//	       floored at 10m, rounded up to 5m
//	mem  = max(p99, observed max) × (1 + headroom), floored at 32Mi,
//	       rounded up to 1Mi — never below the observed max: running out
//	       of memory is an OOM kill, not a latency tradeoff
//	OOM  = any OOM kill in the window freezes memory at or above the
//	       current request and raises it to 1.25 × max(observed max,
//	       current limit) — the working set hit the limit when it died
//
// Confidence reflects how much history backs the numbers: under a day
// of 5-minute buckets is low, under five days medium, else high.
package rightsizing

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

const (
	GiB = 1 << 30
	MiB = 1 << 20

	cpuFloorCores = 0.010 // 10m
	cpuStepCores  = 0.005 // 5m
	memFloorBytes = 32 * MiB
	memStepBytes  = MiB

	// DefaultHeadroomPct is added on top of the sizing percentile.
	DefaultHeadroomPct = 15.0
	// oomUpsize is the memory multiplier after an OOM kill.
	oomUpsize = 1.25
	// changeBand: within ±10% of the current request is "ok".
	changeBand = 0.10
	// bucket is the container_usage_5m resolution.
	bucket = 5 * time.Minute
)

// Quantiles of one resource over the window.
type Quantiles struct {
	P50, P90, P95, P99, Max float64
}

// Usage is one container's measured history.
type Usage struct {
	Cluster      string
	Namespace    string
	Workload     string
	WorkloadKind string
	Container    string

	CPU Quantiles // cores
	Mem Quantiles // bytes

	CPURequest, CPULimit float64 // cores (0 = unset)
	MemRequest, MemLimit float64 // bytes (0 = unset)

	Samples      int64
	Buckets      int64   // distinct 5m buckets observed
	ThrottleRisk float64 // share of buckets where p99 ≥ 90% of the CPU limit
	MaxRestarts  uint32
}

// Price is what the workload pays per unit, from the cost rollup.
type Price struct {
	CPUCoreHour float64 // $ per allocated core-hour
	RAMGiBHour  float64 // $ per allocated GiB-hour
	Replicas    float64 // average concurrently running pods
	Cloud       string  // aws | gcp | azure | ""
}

// Options are the tunables of one run.
type Options struct {
	HeadroomPct   float64 // default 15
	CPUPercentile string  // p90 | p95 | p99 | max (default p95)
	Window        time.Duration
	WindowLabel   string // echoed back, e.g. "7d"
}

// Recommendation is one container's verdict (mirrors the proto).
type Recommendation struct {
	ID           string
	Cluster      string
	Namespace    string
	Workload     string
	WorkloadKind string
	Container    string
	Replicas     float64

	CPURequest, CPULimit              float64
	CPUP50, CPUP95, CPUP99, CPUMax    float64
	CPURecommended                    float64
	MemRequest, MemLimit              float64
	MemP50, MemP99, MemMax            float64
	MemRecommended                    float64
	CurrentCostMonth, RecommendedCost float64
	SavingsMonth                      float64
	CPUSavingsMonth, MemSavingsMonth  float64

	Samples      int64
	Window       string
	Confidence   string
	Direction    string
	Reason       string
	OOMKills     int
	ThrottleRisk float64
	Cloud        string
}

// Direction values.
const (
	Downsize = "downsize"
	Upsize   = "upsize"
	OK       = "ok"
)

// ValidPercentile normalises the CPU percentile choice.
func ValidPercentile(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "p95":
		return "p95", nil
	case "p90":
		return "p90", nil
	case "p99":
		return "p99", nil
	case "max":
		return "max", nil
	}
	return "", fmt.Errorf("cpu_percentile must be p90, p95, p99 or max, got %q", s)
}

func (q Quantiles) pick(p string) float64 {
	switch p {
	case "p90":
		return q.P90
	case "p99":
		return q.P99
	case "max":
		return q.Max
	}
	return q.P95
}

// ceilTo rounds v up to a multiple of step, ignoring float noise.
func ceilTo(v, step float64) float64 {
	return math.Ceil(v/step-1e-9) * step
}

// Recommend computes one container's recommendation. Pure.
func Recommend(u Usage, oomKills int, price Price, o Options) Recommendation {
	headroom := o.HeadroomPct
	if headroom <= 0 {
		headroom = DefaultHeadroomPct
	}
	pct := o.CPUPercentile
	if pct == "" {
		pct = "p95"
	}
	h := 1 + headroom/100

	cpuRec := math.Max(u.CPU.pick(pct)*h, cpuFloorCores)
	cpuRec = round6(ceilTo(cpuRec, cpuStepCores))

	memObserved := math.Max(u.Mem.P99, u.Mem.Max)
	memRec := math.Max(memObserved*h, memFloorBytes)
	if oomKills > 0 {
		base := math.Max(u.Mem.Max, u.MemLimit)
		memRec = math.Max(memRec, math.Max(u.MemRequest, base*oomUpsize))
	}
	memRec = ceilTo(memRec, memStepBytes)

	r := Recommendation{
		ID:           strings.Join([]string{u.Cluster, u.Namespace, u.Workload, u.Container}, "/"),
		Cluster:      u.Cluster,
		Namespace:    u.Namespace,
		Workload:     u.Workload,
		WorkloadKind: u.WorkloadKind,
		Container:    u.Container,
		Replicas:     price.Replicas,
		CPURequest:   u.CPURequest, CPULimit: u.CPULimit,
		CPUP50: u.CPU.P50, CPUP95: u.CPU.P95, CPUP99: u.CPU.P99, CPUMax: u.CPU.Max,
		CPURecommended: cpuRec,
		MemRequest:     u.MemRequest, MemLimit: u.MemLimit,
		MemP50: u.Mem.P50, MemP99: u.Mem.P99, MemMax: u.Mem.Max,
		MemRecommended: memRec,
		Samples:        u.Samples,
		Window:         o.WindowLabel,
		OOMKills:       oomKills,
		ThrottleRisk:   u.ThrottleRisk,
		Cloud:          price.Cloud,
		Confidence:     confidence(u.Buckets),
	}

	// Cost impact at the workload's own unit prices. The billing basis
	// is max(request, usage) — a container requesting less than it
	// uses already pays for its usage — approximated by the median.
	replicas := math.Max(price.Replicas, 1)
	monthly := func(cores, bytes float64) (float64, float64) {
		return cores * price.CPUCoreHour * replicas * timewin.MonthHours,
			bytes / GiB * price.RAMGiBHour * replicas * timewin.MonthHours
	}
	curCPU, curMem := monthly(math.Max(u.CPURequest, u.CPU.P50), math.Max(u.MemRequest, u.Mem.P50))
	recCPU, recMem := monthly(math.Max(cpuRec, u.CPU.P50), math.Max(memRec, u.Mem.P50))
	r.CurrentCostMonth = curCPU + curMem
	r.RecommendedCost = recCPU + recMem
	r.CPUSavingsMonth = curCPU - recCPU
	r.MemSavingsMonth = curMem - recMem
	r.SavingsMonth = r.CurrentCostMonth - r.RecommendedCost

	cpuUp := u.CPURequest == 0 || cpuRec > u.CPURequest*(1+changeBand)
	cpuDown := u.CPURequest > 0 && cpuRec < u.CPURequest*(1-changeBand)
	memUp := u.MemRequest == 0 || memRec > u.MemRequest*(1+changeBand)
	memDown := u.MemRequest > 0 && memRec < u.MemRequest*(1-changeBand)
	switch {
	case cpuUp || memUp:
		// Risk first: an under-provisioned resource is the headline
		// even when the other one could shrink.
		r.Direction = Upsize
	case cpuDown || memDown:
		r.Direction = Downsize
	default:
		r.Direction = OK
	}
	r.Reason = reason(u, r, pct, headroom)
	return r
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// confidence grades history depth by observed 5-minute buckets.
func confidence(buckets int64) string {
	days := float64(buckets) * bucket.Minutes() / (24 * 60)
	switch {
	case days < 1:
		return "low"
	case days < 5:
		return "medium"
	}
	return "high"
}

func reason(u Usage, r Recommendation, pct string, headroom float64) string {
	var parts []string
	pctLabel := strings.ToUpper(pct[:1]) + pct[1:]
	if pct == "max" {
		pctLabel = "max"
	}
	cpu := fmt.Sprintf("CPU %s %s cores + %.0f%% headroom → %s cores", pctLabel,
		fmtCores(u.CPU.pick(pct)), headroom, fmtCores(r.CPURecommended))
	switch {
	case u.CPURequest == 0:
		cpu += " (no request set today)"
	case r.CPURecommended < u.CPURequest:
		cpu += fmt.Sprintf(" (request %s: %.0f%% of it sits idle at %s)", fmtCores(u.CPURequest),
			100*(1-u.CPU.pick(pct)/u.CPURequest), pct)
	default:
		cpu += fmt.Sprintf(" (request %s is too tight)", fmtCores(u.CPURequest))
	}
	parts = append(parts, cpu+".")

	mem := fmt.Sprintf("Memory max(p99, max) %s + %.0f%% → %s", fmtBytes(math.Max(u.Mem.P99, u.Mem.Max)),
		headroom, fmtBytes(r.MemRecommended))
	if u.MemRequest == 0 {
		mem += " (no request set today)"
	} else {
		mem += fmt.Sprintf(" (request %s)", fmtBytes(u.MemRequest))
	}
	parts = append(parts, mem+".")

	if r.OOMKills > 0 {
		parts = append(parts, fmt.Sprintf("%d OOM kill(s) in the window: memory is never lowered after an OOM and is raised to %.2f× max(observed max, limit %s).",
			r.OOMKills, oomUpsize, fmtBytes(u.MemLimit)))
	}
	if u.ThrottleRisk >= 0.05 {
		parts = append(parts, fmt.Sprintf("CPU p99 reached ≥90%% of its limit in %.0f%% of 5-minute windows — throttling risk.", 100*u.ThrottleRisk))
	}
	switch r.Confidence {
	case "low":
		parts = append(parts, "Less than a day of usage history — low confidence; re-check after a few days.")
	case "medium":
		parts = append(parts, "Under five days of history — medium confidence; weekly peaks may be missing.")
	}
	return strings.Join(parts, " ")
}

func fmtCores(c float64) string {
	if c < 1 {
		return fmt.Sprintf("%.0fm", c*1000)
	}
	return fmt.Sprintf("%.2f", c)
}

func fmtBytes(b float64) string {
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.2f GiB", b/GiB)
	case b >= MiB:
		return fmt.Sprintf("%.0f MiB", b/MiB)
	}
	return fmt.Sprintf("%.0f B", b)
}

// FmtCores / FmtBytes are exported for signal lines built elsewhere.
func FmtCores(c float64) string { return fmtCores(c) }

// FmtBytes renders bytes in MiB/GiB.
func FmtBytes(b float64) string { return fmtBytes(b) }
