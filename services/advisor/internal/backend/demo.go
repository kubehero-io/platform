// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package backend

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// Demo serves one small, internally consistent fleet so demo briefings
// and investigations tell a coherent story:
//
//   - payments: checkout-api's upstream (payments-db) started timing out
//     ~10h ago; retries drove an error-log spike, a TLS-handshake-heavy
//     CPU profile, cross-zone retransmits and an HPA scale-out — spend
//     jumped ~28%. checkout-api is also 4x over-requested on CPU.
//   - ml-inference: the GPU nodepool grew 3→5 nodes with flat traffic
//     (spend +34% w/w, GPU utilisation 7%).
//   - data: batch-etl requests 64Gi and uses 11Gi.
//
// Every response that carries a source field says "demo".
type Demo struct {
	// Now anchors every timestamp; defaults to time.Now.
	Now func() time.Time
}

var _ Backend = Demo{}

func (Demo) Origin() string { return "demo" }

func (d Demo) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// ─── fixtures ────────────────────────────────────────────────────────────

type demoWorkload struct {
	cluster, namespace, name, team string
	monthly                        float64 // total $/mo
	cpuShare                       float64 // share of compute cost that is CPU (rest RAM/GPU)
	cpuEff, ramEff                 float64
	gpuMonthly, networkMonthly     float64
	recoverable                    float64
	cpuReq, cpuUsed                float64 // cores
	memReqGiB, memUsedGiB          float64
	zone                           string
}

var demoWorkloads = []demoWorkload{
	{"eks-use1-prod", "ml-inference", "model-server-a100", "ml-platform", 52000, 0.1, 0.21, 0.35, 41000, 380, 18400, 32, 6.7, 256, 90, "us-east-1a"},
	{"eks-use1-prod", "ml-inference", "embedder", "ml-platform", 22000, 0.6, 0.64, 0.92, 0, 0, 0, 24, 15.4, 96, 88, "us-east-1b"},
	{"eks-use1-prod", "payments", "checkout-api", "payments", 19500, 0.7, 0.26, 0.52, 0, 640, 6200, 160, 41.6, 320, 166, "us-east-1a"},
	{"eks-use1-prod", "payments", "ledger", "payments", 11500, 0.6, 0.71, 0.66, 0, 90, 0, 48, 34, 192, 127, "us-east-1b"},
	{"gke-euw1-prod", "data", "batch-etl", "data", 12500, 0.3, 0.55, 0.17, 0, 1300, 3100, 32, 17.6, 64, 11, "europe-west1-b"},
	{"eks-use1-prod", "edge", "api-gateway", "edge", 6900, 0.6, 0.48, 0.61, 0, 2400, 0, 24, 11.5, 48, 29, "us-east-1a"},
	{"eks-use1-prod", "platform", "ingress-nginx", "platform", 4000, 0.6, 0.55, 0.6, 0, 150, 0, 12, 6.6, 24, 14, "us-east-1c"},
}

const hoursPerMonth = 730.0

func (Demo) ListClusters(context.Context, *kuberov1.ListClustersRequest) (*kuberov1.ListClustersResponse, error) {
	return &kuberov1.ListClustersResponse{Clusters: []*kuberov1.Cluster{
		{Id: "eks-use1-prod", Name: "eks-use1-prod", Cloud: "aws", Region: "us-east-1", Nodes: 42},
		{Id: "gke-euw1-prod", Name: "gke-euw1-prod", Cloud: "gcp", Region: "europe-west1", Nodes: 18},
	}}, nil
}

func (Demo) ListWasteRecommendations(_ context.Context, r *kuberov1.ListWasteRecommendationsRequest) (*kuberov1.ListWasteRecommendationsResponse, error) {
	recs := []*kuberov1.WasteRecommendation{
		{Rank: "01", Workload: "model-server-a100", Namespace: "ml-inference", Cluster: "eks-use1-prod", Cloud: "EKS",
			Signal: "gpu.util=7%  req=2xA100", RecoverableUsdMonth: 18400, Action: "review", Severity: "warn"},
		{Rank: "02", Workload: "checkout-api", Namespace: "payments", Cluster: "eks-use1-prod", Cloud: "EKS",
			Signal: "cpu.req=16  used=0.41", RecoverableUsdMonth: 6200, Action: "apply", Severity: "accent"},
		{Rank: "03", Workload: "batch-etl", Namespace: "data", Cluster: "gke-euw1-prod", Cloud: "GKE",
			Signal: "mem.req=64Gi  used=11Gi", RecoverableUsdMonth: 3100, Action: "apply", Severity: "accent"},
	}
	return &kuberov1.ListWasteRecommendationsResponse{Recommendations: limitSlice(recs, int(r.GetLimit()))}, nil
}

func (Demo) ListAnomalies(_ context.Context, r *kuberov1.ListAnomaliesRequest) (*kuberov1.ListAnomaliesResponse, error) {
	as := []*kuberov1.Anomaly{
		{Id: "anom-7c91", Kind: "spend", Title: "ml-inference spend +34% w/w", Subject: "ml-inference",
			Detail: "GPU nodepool grew 3→5 nodes with flat request volume", DeltaPct: 34, ImpactUsdMonth: 12300,
			Severity: "warn", Source: "demo", LinkPath: "/workloads/eks-use1-prod/ml-inference/model-server-a100"},
		{Id: "anom-2b44", Kind: "capacity", Title: "payments pending pods", Subject: "payments",
			Detail: "6 pods unschedulable for 40m on nodepool general-m5", DeltaPct: 0, ImpactUsdMonth: 2100,
			Severity: "info", Source: "demo", LinkPath: "/capacity/eks-use1-prod/payments"},
	}
	return &kuberov1.ListAnomaliesResponse{Anomalies: limitSlice(as, int(r.GetLimit())), Total: int32(len(as))}, nil
}

func (Demo) GetTeamSpend(context.Context, *kuberov1.GetTeamSpendRequest) (*kuberov1.GetTeamSpendResponse, error) {
	return &kuberov1.GetTeamSpendResponse{
		Teams: []*kuberov1.TeamSpend{
			{Team: "ml-platform", CostCenter: "cc-401", SpendUsdMonth: 74000, RecoverableUsdMonth: 21500, GpuIdleUsdMonth: 16800},
			{Team: "payments", CostCenter: "cc-102", SpendUsdMonth: 31000, RecoverableUsdMonth: 6200},
		},
		FleetTotalUsdMonth:       128400,
		FleetRecoverableUsdMonth: 27700,
	}, nil
}

func (Demo) GetBurnRate(context.Context, *kuberov1.GetBurnRateRequest) (*kuberov1.GetBurnRateResponse, error) {
	return &kuberov1.GetBurnRateResponse{BurnRateMilli: 1350, Available: true, Source: "demo"}, nil
}

func (d Demo) GetWorkload(ctx context.Context, r *kuberov1.GetWorkloadRequest) (*kuberov1.GetWorkloadResponse, error) {
	waste, _ := d.ListWasteRecommendations(ctx, &kuberov1.ListWasteRecommendationsRequest{})
	out := &kuberov1.GetWorkloadResponse{}
	for _, w := range waste.GetRecommendations() {
		if w.GetNamespace() == r.GetNamespace() && w.GetWorkload() == r.GetName() {
			out.Recommendation = w
		}
	}
	if r.GetName() == "checkout-api" {
		now := d.now().UTC()
		out.History = []*kuberov1.AuditEntry{
			{Id: "aud-demo-2", At: now.Add(-10 * time.Hour).Format(time.RFC3339), Policy: "HorizontalPodAutoscaler/checkout-api",
				Action: "hpa scale 6→10 replicas", Cluster: "eks-use1-prod", Outcome: "applied"},
			{Id: "aud-demo-1", At: now.Add(-11 * time.Hour).Format(time.RFC3339), Policy: "Deployment/payments-db-proxy",
				Action: "config change: pool size 50→20", Cluster: "eks-use1-prod", Outcome: "applied"},
		}
	}
	return out, nil
}

func (Demo) ListCapacityDemands(context.Context, *kuberov1.ListCapacityDemandsRequest) (*kuberov1.ListCapacityDemandsResponse, error) {
	return &kuberov1.ListCapacityDemandsResponse{
		Demands: []*kuberov1.CapacityDemand{{
			Id: "demand-eks-use1-payments-1", Cluster: "eks-use1-prod", Namespace: "payments", Workload: "checkout-api",
			PendingPods: 6, RequestedCpu: "96 cores", RequestedMem: "192 GiB", OldestPendingAge: "40m",
			RecommendedAction: "scale nodepool general-m5 by 2 nodes", RecommendedCostUsdMonth: 560,
			BlockedCostUsdMonth: 2100, Source: "demo",
		}},
		TotalPendingPods: 6, TotalBlockedUsdMonth: 2100,
	}, nil
}

// ─── cost ────────────────────────────────────────────────────────────────

var windowHours = map[string]float64{
	"": 24, "1h": 1, "24h": 24, "today": 12, "yesterday": 24, "7d": 168, "week": 168,
	"14d": 336, "30d": 720, "month": 720, "lastmonth": 720,
}

func (d Demo) GetAllocation(_ context.Context, r *kuberov1.GetAllocationRequest) (*kuberov1.GetAllocationResponse, error) {
	hours, ok := windowHours[r.GetWindow()]
	if !ok {
		hours = 24
	}
	f := hours / hoursPerMonth
	dims := r.GetAggregate()
	if len(dims) == 0 {
		dims = []string{"namespace"}
	}
	rows := map[string]*kuberov1.Allocation{}
	totals := &kuberov1.Allocation{Name: "__total__"}
	for _, w := range demoWorkloads {
		if !matchesFilters(w, r.GetFilters()) || (r.GetClusterId() != "" && r.GetClusterId() != w.cluster) {
			continue
		}
		keyParts := make([]string, 0, len(dims))
		props := map[string]string{}
		for _, dim := range dims {
			v := w.dim(dim)
			keyParts = append(keyParts, v)
			props[dim] = v
		}
		key := strings.Join(keyParts, "/")
		row := rows[key]
		if row == nil {
			row = &kuberov1.Allocation{Name: key, Properties: props, Minutes: hours * 60}
			rows[key] = row
		}
		compute := w.monthly - w.gpuMonthly - w.networkMonthly
		addAlloc(row, w, compute, f)
		addAlloc(totals, w, compute, f)
	}
	out := make([]*kuberov1.Allocation, 0, len(rows))
	for _, a := range rows {
		finishAlloc(a)
		out = append(out, a)
	}
	finishAlloc(totals)
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalCost != out[j].TotalCost {
			return out[i].TotalCost > out[j].TotalCost
		}
		return out[i].Name < out[j].Name
	})
	end := d.now()
	return &kuberov1.GetAllocationResponse{
		Allocations: out, Totals: totals,
		StartUnixMs: end.Add(-time.Duration(hours) * time.Hour).UnixMilli(), EndUnixMs: end.UnixMilli(),
		Source: "demo",
	}, nil
}

func addAlloc(a *kuberov1.Allocation, w demoWorkload, compute, f float64) {
	a.CpuCost += compute * w.cpuShare * f
	a.RamCost += compute * (1 - w.cpuShare) * f
	a.GpuCost += w.gpuMonthly * f
	a.NetworkCost += w.networkMonthly * f
	a.TotalCost += w.monthly * f
	a.RecoverableCost += w.recoverable * f
	a.CpuCoreRequestAverage += w.cpuReq
	a.CpuCoreUsageAverage += w.cpuUsed
	a.RamByteRequestAverage += w.memReqGiB * (1 << 30)
	a.RamByteUsageAverage += w.memUsedGiB * (1 << 30)
}

func finishAlloc(a *kuberov1.Allocation) {
	if a.CpuCoreRequestAverage > 0 {
		a.CpuEfficiency = a.CpuCoreUsageAverage / a.CpuCoreRequestAverage
	}
	if a.RamByteRequestAverage > 0 {
		a.RamEfficiency = a.RamByteUsageAverage / a.RamByteRequestAverage
	}
	if c := a.CpuCost + a.RamCost; c > 0 {
		a.TotalEfficiency = (a.CpuEfficiency*a.CpuCost + a.RamEfficiency*a.RamCost) / c
	}
}

func (w demoWorkload) dim(dim string) string {
	switch dim {
	case "cluster":
		return w.cluster
	case "workload", "controller":
		return w.name
	case "team":
		return w.team
	case "zone":
		return w.zone
	case "nodepool":
		if w.gpuMonthly > 0 {
			return "gpu-a100"
		}
		return "general-m5"
	default:
		return w.namespace
	}
}

func matchesFilters(w demoWorkload, filters map[string]string) bool {
	for k, v := range filters {
		if w.dim(k) != v {
			return false
		}
	}
	return true
}

// spendMultiplier shapes the demo spend curve: checkout-api scaled out
// ~10h ago (+45%, which is +28% for payments as a whole) and the
// ml-inference GPU pool has run +34% for the last 3 days.
func spendMultiplier(w demoWorkload, age time.Duration) float64 {
	switch {
	case w.name == "checkout-api" && age < 10*time.Hour:
		return 1.45
	case w.namespace == "ml-inference" && age < 72*time.Hour:
		return 1.34
	}
	return 1
}

func (d Demo) GetCostTimeseries(_ context.Context, r *kuberov1.GetCostTimeseriesRequest) (*kuberov1.GetCostTimeseriesResponse, error) {
	window := r.GetWindow()
	if window == "" {
		window = "30d"
	}
	hours, ok := windowHours[window]
	if !ok {
		hours = 720
	}
	step := time.Hour
	if r.GetStep() == "1d" || (r.GetStep() == "" && hours > 48) {
		step = 24 * time.Hour
	}
	end := d.now().Truncate(time.Hour)
	start := end.Add(-time.Duration(hours) * time.Hour)
	groups := map[string]*kuberov1.Series{}
	total := 0.0
	for _, w := range demoWorkloads {
		if !matchesFilters(w, r.GetFilters()) {
			continue
		}
		key := ""
		if gb := r.GetGroupBy(); gb != "" {
			key = w.dim(gb)
		}
		s := groups[key]
		if s == nil {
			labels := map[string]string{}
			if gb := r.GetGroupBy(); gb != "" {
				labels[gb] = key
			}
			s = &kuberov1.Series{Labels: labels}
			groups[key] = s
		}
		hourly := w.monthly / hoursPerMonth
		for i, t := 0, start; t.Before(end); i, t = i+1, t.Add(step) {
			v := hourly * step.Hours() * spendMultiplier(w, end.Sub(t))
			if i < len(s.Points) {
				s.Points[i].Value += v
			} else {
				s.Points = append(s.Points, &kuberov1.Point{TsUnixMs: t.UnixMilli(), Value: v})
			}
			total += v
		}
	}
	series := make([]*kuberov1.Series, 0, len(groups))
	for _, s := range groups {
		series = append(series, s)
	}
	sortSeries(series)
	if top := int(r.GetTop()); top > 0 && len(series) > top {
		series = series[:top]
	}
	return &kuberov1.GetCostTimeseriesResponse{
		Series: series, TotalUsd: total, ForecastMonthUsd: 128400 * 1.06, Source: "demo",
	}, nil
}

// sortSeries orders by total, largest first, with a label tie-break so
// map-built fixtures come out in a stable order.
func sortSeries(ss []*kuberov1.Series) {
	sort.Slice(ss, func(i, j int) bool {
		if a, b := seriesSum(ss[i]), seriesSum(ss[j]); a != b {
			return a > b
		}
		return fmt.Sprint(ss[i].GetLabels()) < fmt.Sprint(ss[j].GetLabels())
	})
}

func seriesSum(s *kuberov1.Series) float64 {
	sum := 0.0
	for _, p := range s.GetPoints() {
		sum += p.GetValue()
	}
	return sum
}

func (Demo) ListRightsizing(_ context.Context, r *kuberov1.ListRightsizingRequest) (*kuberov1.ListRightsizingResponse, error) {
	all := []*kuberov1.RightsizingRecommendation{
		{Id: "eks-use1-prod/payments/checkout-api/app", Cluster: "eks-use1-prod", Namespace: "payments",
			Workload: "checkout-api", WorkloadKind: "Deployment", Container: "app", Replicas: 10,
			CpuRequestCores: 16, CpuP95Cores: 3.1, CpuMaxCores: 5.2, CpuRecommendedCores: 3.6,
			MemRequestBytes: 32 << 30, MemP99Bytes: 15 << 30, MemMaxBytes: 16 << 30, MemRecommendedBytes: 18 << 30,
			CurrentCostUsdMonth: 19500, RecommendedCostUsdMonth: 13300, SavingsUsdMonth: 6200,
			Samples: 60480, Window: "7d", Confidence: "high", Direction: "downsize",
			Reason: "p95 CPU is 3.1 of 16 requested cores; memory max 16Gi of 32Gi requested"},
		{Id: "gke-euw1-prod/data/batch-etl/etl", Cluster: "gke-euw1-prod", Namespace: "data",
			Workload: "batch-etl", WorkloadKind: "StatefulSet", Container: "etl", Replicas: 2,
			CpuRequestCores: 16, CpuP95Cores: 8.8, CpuMaxCores: 12, CpuRecommendedCores: 10.2,
			MemRequestBytes: 64 << 30, MemP99Bytes: 10 << 30, MemMaxBytes: 11 << 30, MemRecommendedBytes: 13 << 30,
			CurrentCostUsdMonth: 12500, RecommendedCostUsdMonth: 9400, SavingsUsdMonth: 3100,
			Samples: 20160, Window: "7d", Confidence: "medium", Direction: "downsize",
			Reason: "memory max 11Gi of 64Gi requested"},
		{Id: "eks-use1-prod/ml-inference/embedder/embedder", Cluster: "eks-use1-prod", Namespace: "ml-inference",
			Workload: "embedder", WorkloadKind: "Deployment", Container: "embedder", Replicas: 4,
			CpuRequestCores: 6, CpuP95Cores: 3.9, CpuMaxCores: 5.5, CpuRecommendedCores: 4.5,
			MemRequestBytes: 24 << 30, MemP99Bytes: 23 << 30, MemMaxBytes: 24 << 30, MemRecommendedBytes: 28 << 30,
			CurrentCostUsdMonth: 22000, RecommendedCostUsdMonth: 22310, SavingsUsdMonth: -310,
			Samples: 60480, Window: "7d", Confidence: "high", Direction: "upsize", OomKills: 4,
			Reason: "4 OOM kills in the window; memory runs at its limit"},
	}
	var out []*kuberov1.RightsizingRecommendation
	total := 0.0
	for _, rec := range all {
		if ns := r.GetNamespace(); ns != "" && rec.Namespace != ns {
			continue
		}
		if r.GetMinSavingsUsdMonth() > 0 && rec.SavingsUsdMonth < r.GetMinSavingsUsdMonth() {
			continue
		}
		out = append(out, rec)
		total += rec.SavingsUsdMonth
	}
	return &kuberov1.ListRightsizingResponse{
		Recommendations: limitSlice(out, int(r.GetLimit())), TotalSavingsUsdMonth: total, Source: "demo",
	}, nil
}

func (Demo) GetEfficiency(context.Context, *kuberov1.GetEfficiencyRequest) (*kuberov1.GetEfficiencyResponse, error) {
	return &kuberov1.GetEfficiencyResponse{
		Score: 41, CpuEfficiency: 0.31, RamEfficiency: 0.43, IdleCostUsdMonth: 18900, RecoverableUsdMonth: 27700,
		Clusters: []*kuberov1.EfficiencyBreakdown{
			{Name: "eks-use1-prod", CpuEfficiency: 0.29, RamEfficiency: 0.47, IdleCostUsdMonth: 16100, TotalCostUsdMonth: 115900, Score: 39},
			{Name: "gke-euw1-prod", CpuEfficiency: 0.55, RamEfficiency: 0.17, IdleCostUsdMonth: 2800, TotalCostUsdMonth: 12500, Score: 44},
		},
		Namespaces: []*kuberov1.EfficiencyBreakdown{
			{Name: "ml-inference", CpuEfficiency: 0.38, RamEfficiency: 0.61, IdleCostUsdMonth: 11200, TotalCostUsdMonth: 74000, Score: 33},
			{Name: "payments", CpuEfficiency: 0.36, RamEfficiency: 0.57, IdleCostUsdMonth: 4300, TotalCostUsdMonth: 31000, Score: 45},
			{Name: "data", CpuEfficiency: 0.55, RamEfficiency: 0.17, IdleCostUsdMonth: 2800, TotalCostUsdMonth: 12500, Score: 44},
		},
		Source: "demo",
	}, nil
}

// ─── logs ────────────────────────────────────────────────────────────────

var labelRE = regexp.MustCompile(`([a-z_]+)\s*=\s*"([^"]*)"`)

// queryLabels extracts exact-match label matchers from a LogQL selector —
// enough for the demo to answer the questions investigations ask.
func queryLabels(q string) map[string]string {
	out := map[string]string{}
	for _, m := range labelRE.FindAllStringSubmatch(q, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// errorRate is the demo's error lines per hour for a namespace at a given age.
func errorRate(namespace string, age time.Duration) float64 {
	switch namespace {
	case "payments":
		if age < 10*time.Hour {
			return 2300
		}
		return 45
	case "ml-inference":
		return 12
	case "data":
		return 30
	}
	return 4
}

func (d Demo) timeRange(startMs, endMs int64, def time.Duration) (time.Time, time.Time) {
	end := d.now()
	if endMs > 0 {
		end = time.UnixMilli(endMs)
	}
	start := end.Add(-def)
	if startMs > 0 {
		start = time.UnixMilli(startMs)
	}
	return start, end
}

func (d Demo) GetLogVolume(_ context.Context, r *kuberov1.GetLogVolumeRequest) (*kuberov1.GetLogVolumeResponse, error) {
	start, end := d.timeRange(r.GetStartUnixMs(), r.GetEndUnixMs(), time.Hour)
	labels := queryLabels(r.GetQuery())
	groupBy := r.GetGroupBy()
	if groupBy == "" {
		groupBy = "level"
	}
	step := time.Duration(r.GetStepMs()) * time.Millisecond
	if step <= 0 {
		step = end.Sub(start) / 60
		if step < time.Minute {
			step = time.Minute
		}
	}
	namespaces := []string{"payments", "ml-inference", "data", "edge", "platform"}
	if ns := labels["namespace"]; ns != "" {
		namespaces = []string{ns}
	}
	levels := []string{"error", "warn", "info"}
	if lv := labels["level"]; lv != "" {
		levels = []string{lv}
	}
	series := map[string]*kuberov1.Series{}
	var totalLines int64
	for _, ns := range namespaces {
		for _, lv := range levels {
			key := lv
			switch groupBy {
			case "namespace":
				key = ns
			case "workload":
				key = demoWorkloadFor(ns)
			}
			s := series[key]
			if s == nil {
				s = &kuberov1.Series{Labels: map[string]string{groupBy: key}}
				series[key] = s
			}
			i := 0
			for t := start; t.Before(end); t = t.Add(step) {
				rate := errorRate(ns, end.Sub(t))
				switch lv {
				case "warn":
					rate *= 1.8
				case "info":
					rate = 900
				}
				v := math.Round(rate * step.Hours())
				if i < len(s.Points) {
					s.Points[i].Value += v
				} else {
					s.Points = append(s.Points, &kuberov1.Point{TsUnixMs: t.UnixMilli(), Value: v})
				}
				totalLines += int64(v)
				i++
			}
		}
	}
	out := &kuberov1.GetLogVolumeResponse{TotalLines: totalLines, TotalBytes: totalLines * 310}
	for _, s := range series {
		out.Series = append(out.Series, s)
	}
	sortSeries(out.Series)
	out.EstCostUsdMonth = float64(out.TotalBytes) / (1 << 30) * 0.5 * hoursPerMonth / math.Max(end.Sub(start).Hours(), 1)
	return out, nil
}

type demoPattern struct {
	namespace, pattern, level, sample string
	count                             int64
}

var demoPatterns = []demoPattern{
	{"payments", "upstream timeout calling <_> after <_>ms", "error", "upstream timeout calling payments-db-proxy:5432 after 3000ms", 18240},
	{"payments", "retrying charge <_> attempt <_>/3", "warn", "retrying charge ch_9Qx1 attempt 2/3", 6410},
	{"payments", "db pool exhausted (max=<_>, waiting=<_>)", "error", "db pool exhausted (max=20, waiting=57)", 2150},
	{"payments", "GET /healthz 200 <_>ms", "info", "GET /healthz 200 2ms", 3300},
	{"ml-inference", "batch <_> served in <_>ms (gpu=<_>)", "info", "batch 81231 served in 41ms (gpu=0)", 9800},
	{"data", "spilled <_> MiB to disk", "warn", "spilled 512 MiB to disk", 410},
}

func (d Demo) GetLogPatterns(_ context.Context, r *kuberov1.GetLogPatternsRequest) (*kuberov1.GetLogPatternsResponse, error) {
	labels := queryLabels(r.GetQuery())
	_, end := d.timeRange(r.GetStartUnixMs(), r.GetEndUnixMs(), time.Hour)
	var total int64
	var picked []demoPattern
	for _, p := range demoPatterns {
		if ns := labels["namespace"]; ns != "" && p.namespace != ns {
			continue
		}
		if lv := labels["level"]; lv != "" && p.level != lv {
			continue
		}
		picked = append(picked, p)
		total += p.count
	}
	out := &kuberov1.GetLogPatternsResponse{LinesAnalyzed: total}
	for _, p := range picked {
		trend := make([]*kuberov1.Point, 0, 12)
		for i := 11; i >= 0; i-- {
			t := end.Add(-time.Duration(i) * time.Hour)
			v := float64(p.count) / 12
			if p.namespace == "payments" && p.level != "info" {
				if i >= 10 {
					v = float64(p.count) * 0.004
				} else {
					v = float64(p.count) * 0.0996
				}
			}
			trend = append(trend, &kuberov1.Point{TsUnixMs: t.UnixMilli(), Value: math.Round(v)})
		}
		out.Patterns = append(out.Patterns, &kuberov1.LogPattern{
			Pattern: p.pattern, Count: p.count, Level: p.level, Sample: p.sample, Trend: trend,
			SharePct: 100 * float64(p.count) / float64(max(total, 1)),
		})
	}
	out.Patterns = limitSlice(out.Patterns, int(r.GetLimit()))
	return out, nil
}

func (d Demo) QueryLogs(_ context.Context, r *kuberov1.QueryLogsRequest) (*kuberov1.QueryLogsResponse, error) {
	q := strings.TrimSpace(r.GetQuery())
	labels := queryLabels(q)
	start, end := d.timeRange(r.GetStartUnixMs(), r.GetEndUnixMs(), time.Hour)
	if isMetricQuery(q) {
		vol, _ := d.GetLogVolume(context.Background(), &kuberov1.GetLogVolumeRequest{
			Query: q, StartUnixMs: start.UnixMilli(), EndUnixMs: end.UnixMilli(), GroupBy: "namespace", StepMs: r.GetStepMs(),
		})
		return &kuberov1.QueryLogsResponse{ResultType: "matrix", Series: vol.GetSeries()}, nil
	}
	limit := int(r.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	var matching []demoPattern
	for _, p := range demoPatterns {
		if ns := labels["namespace"]; ns != "" && p.namespace != ns {
			continue
		}
		if lv := labels["level"]; lv != "" && p.level != lv {
			continue
		}
		matching = append(matching, p)
	}
	var lines []*kuberov1.LogLine
	for i := 0; len(matching) > 0 && i < limit && i < 200; i++ {
		ts := end.Add(-time.Duration(i) * 7 * time.Second)
		if ts.Before(start) {
			break
		}
		p := matching[i%len(matching)]
		lines = append(lines, &kuberov1.LogLine{
			TsUnixNano: ts.UnixNano(), Body: p.sample, Level: p.level,
			Labels: map[string]string{"namespace": p.namespace, "cluster": "eks-use1-prod", "workload": demoWorkloadFor(p.namespace)},
		})
	}
	return &kuberov1.QueryLogsResponse{ResultType: "streams", Lines: lines}, nil
}

func isMetricQuery(q string) bool {
	for _, fn := range []string{"count_over_time", "rate(", "bytes_over_time", "bytes_rate", "sum(", "sum by", "topk"} {
		if strings.Contains(q, fn) {
			return true
		}
	}
	return false
}

func demoWorkloadFor(namespace string) string {
	switch namespace {
	case "payments":
		return "checkout-api"
	case "ml-inference":
		return "model-server-a100"
	case "data":
		return "batch-etl"
	}
	return namespace
}

// ─── profiles ────────────────────────────────────────────────────────────

func (d Demo) ListProfileTargets(context.Context, *kuberov1.ListProfileTargetsRequest) (*kuberov1.ListProfileTargetsResponse, error) {
	last := d.now().Add(-15 * time.Second).UnixMilli()
	return &kuberov1.ListProfileTargetsResponse{Targets: []*kuberov1.ProfileTarget{
		{Service: "checkout-api", Namespace: "payments", Workload: "checkout-api", Types: []string{"cpu", "alloc_space"},
			Origin: "ebpf", LastSeenUnixMs: last, CpuCoresAvg: 41.6, CostUsdMonth: 19500},
		{Service: "api-gateway", Namespace: "edge", Workload: "api-gateway", Types: []string{"cpu"},
			Origin: "ebpf", LastSeenUnixMs: last, CpuCoresAvg: 11.5, CostUsdMonth: 6900},
		{Service: "model-server-a100", Namespace: "ml-inference", Workload: "model-server-a100", Types: []string{"cpu"},
			Origin: "pprof-scrape", LastSeenUnixMs: last, CpuCoresAvg: 6.7, CostUsdMonth: 52000},
	}}, nil
}

type demoFrame struct {
	name           string
	parent         int
	self, baseSelf int64
}

// checkoutFrames is checkout-api's CPU tree; baseSelf is the pre-incident
// profile (TLS handshakes quadrupled once retries opened new connections).
var checkoutFrames = []demoFrame{
	{"total", -1, 0, 0},
	{"main.(*Server).handleCharge", 0, 20, 30},
	{"payments.(*Client).Charge", 1, 10, 15},
	{"net/http.(*Client).Do", 2, 30, 30},
	{"crypto/tls.(*Conn).Handshake", 3, 2100, 520},
	{"database/sql.(*DB).conn", 2, 900, 150},
	{"encoding/json.(*encodeState).marshal", 1, 1400, 1300},
	{"regexp.(*Regexp).doExecute", 1, 600, 580},
	{"runtime.mallocgc", 0, 500, 420},
	{"runtime.gcBgMarkWorker", 0, 450, 380},
}

func (Demo) GetFlamegraph(_ context.Context, r *kuberov1.GetFlamegraphRequest) (*kuberov1.GetFlamegraphResponse, error) {
	diff := r.GetBaselineStartUnixMs() > 0 && r.GetBaselineEndUnixMs() > 0
	nodes := make([]*kuberov1.FlameNode, len(checkoutFrames))
	depth := make([]int32, len(checkoutFrames))
	for i, f := range checkoutFrames {
		if f.parent >= 0 {
			depth[i] = depth[f.parent] + 1
		}
		nodes[i] = &kuberov1.FlameNode{Name: f.name, Parent: int32(f.parent), Depth: depth[i], Self: f.self}
		if diff {
			nodes[i].BaselineSelf = f.baseSelf
		}
	}
	// Totals: self plus every descendant (frames are listed parent-first).
	for i := len(nodes) - 1; i >= 0; i-- {
		nodes[i].Total += nodes[i].Self
		nodes[i].BaselineTotal += nodes[i].BaselineSelf
		if p := nodes[i].Parent; p >= 0 {
			nodes[p].Total += nodes[i].Total
			nodes[p].BaselineTotal += nodes[i].BaselineTotal
		}
	}
	return &kuberov1.GetFlamegraphResponse{
		Nodes: nodes, Total: nodes[0].Total, Unit: "samples", Type: "cpu",
		BaselineTotal: nodes[0].BaselineTotal, Samples: nodes[0].Total, CostUsdMonth: 13650,
	}, nil
}

func (d Demo) GetTopFunctions(ctx context.Context, r *kuberov1.GetTopFunctionsRequest) (*kuberov1.GetTopFunctionsResponse, error) {
	fg, _ := d.GetFlamegraph(ctx, &kuberov1.GetFlamegraphRequest{Selector: r.GetSelector()})
	total := fg.GetTotal()
	var fns []*kuberov1.TopFunction
	for _, n := range fg.GetNodes()[1:] {
		fns = append(fns, &kuberov1.TopFunction{
			Name: n.Name, Self: n.Self, Total: n.Total,
			SelfPct: 100 * float64(n.Self) / float64(total), TotalPct: 100 * float64(n.Total) / float64(total),
			SelfCostUsdMonth: fg.GetCostUsdMonth() * float64(n.Self) / float64(total),
		})
	}
	by := r.GetOrderBy()
	sort.SliceStable(fns, func(i, j int) bool {
		if by == "total" {
			return fns[i].Total > fns[j].Total
		}
		return fns[i].Self > fns[j].Self
	})
	limit := int(r.GetLimit())
	if limit <= 0 {
		limit = 25
	}
	return &kuberov1.GetTopFunctionsResponse{Functions: limitSlice(fns, limit), Total: total, Unit: "samples"}, nil
}

// ─── network ─────────────────────────────────────────────────────────────

func (Demo) GetServiceMap(_ context.Context, r *kuberov1.GetServiceMapRequest) (*kuberov1.GetServiceMapResponse, error) {
	nodes := []*kuberov1.ServiceMapNode{
		{Id: "workload:edge/api-gateway", Name: "api-gateway", Namespace: "edge", Kind: "workload", Zone: "us-east-1a", CostUsdMonth: 2400},
		{Id: "workload:payments/checkout-api", Name: "checkout-api", Namespace: "payments", Kind: "workload", Zone: "us-east-1a", CostUsdMonth: 640},
		{Id: "service:payments/payments-db-proxy", Name: "payments-db-proxy", Namespace: "payments", Kind: "service", Zone: "us-east-1b"},
		{Id: "external:stripe.com", Name: "stripe.com", Kind: "external"},
		{Id: "external:internet", Name: "internet", Kind: "external"},
		{Id: "workload:data/batch-etl", Name: "batch-etl", Namespace: "data", Kind: "workload", Zone: "europe-west1-b", CostUsdMonth: 1300},
		{Id: "external:storage.googleapis.com", Name: "storage.googleapis.com", Kind: "external"},
		{Id: "workload:ml-inference/model-server-a100", Name: "model-server-a100", Namespace: "ml-inference", Kind: "workload", Zone: "us-east-1a", CostUsdMonth: 380},
		{Id: "workload:ml-inference/embedder", Name: "embedder", Namespace: "ml-inference", Kind: "workload", Zone: "us-east-1b"},
	}
	edges := []*kuberov1.ServiceMapEdge{
		{Source: "workload:edge/api-gateway", Target: "external:internet", Port: 443, Protocol: "tcp", Bytes: 4.1e12, BytesPerSec: 1.6e6, Egress: true, CostUsdMonth: 2100},
		{Source: "workload:data/batch-etl", Target: "external:storage.googleapis.com", Port: 443, Protocol: "tcp", Bytes: 2.6e12, BytesPerSec: 1.0e6, Egress: true, CostUsdMonth: 1300},
		{Source: "workload:payments/checkout-api", Target: "service:payments/payments-db-proxy", Port: 5432, Protocol: "tcp", Bytes: 6.4e10, BytesPerSec: 2.5e4, CrossZone: true, CostUsdMonth: 640, Retransmits: 18400},
		{Source: "workload:ml-inference/model-server-a100", Target: "workload:ml-inference/embedder", Port: 8080, Protocol: "tcp", Bytes: 3.8e10, BytesPerSec: 1.5e4, CrossZone: true, CostUsdMonth: 380},
		{Source: "workload:payments/checkout-api", Target: "external:stripe.com", Port: 443, Protocol: "tcp", Bytes: 9e9, BytesPerSec: 3.5e3, Egress: true, CostUsdMonth: 45},
	}
	if ns := r.GetNamespace(); ns != "" {
		keep := map[string]bool{}
		var fe []*kuberov1.ServiceMapEdge
		for _, e := range edges {
			if strings.Contains(e.Source, ":"+ns+"/") || strings.Contains(e.Target, ":"+ns+"/") {
				fe = append(fe, e)
				keep[e.Source], keep[e.Target] = true, true
			}
		}
		var fn []*kuberov1.ServiceMapNode
		for _, n := range nodes {
			if keep[n.Id] {
				fn = append(fn, n)
			}
		}
		nodes, edges = fn, fe
	}
	total, egress, cross := 0.0, 0.0, 0.0
	for _, e := range edges {
		total += e.CostUsdMonth
		if e.Egress {
			egress += e.Bytes / 1e9
		}
		if e.CrossZone {
			cross += e.Bytes / 1e9
		}
	}
	return &kuberov1.GetServiceMapResponse{Nodes: nodes, Edges: edges, TotalCostUsdMonth: total,
		EgressGb: egress, CrossZoneGb: cross, Source: "demo"}, nil
}

func (Demo) ListNetworkCosts(_ context.Context, r *kuberov1.ListNetworkCostsRequest) (*kuberov1.ListNetworkCostsResponse, error) {
	costs := []*kuberov1.NetworkCost{
		{Namespace: "edge", Workload: "api-gateway", EgressGb: 4100, EgressUsdMonth: 2100, TotalUsdMonth: 2100, TopDestination: "internet"},
		{Namespace: "data", Workload: "batch-etl", EgressGb: 2600, EgressUsdMonth: 1300, TotalUsdMonth: 1300, TopDestination: "storage.googleapis.com"},
		{Namespace: "payments", Workload: "checkout-api", CrossZoneGb: 64, CrossZoneUsdMonth: 640, EgressGb: 9, EgressUsdMonth: 45, TotalUsdMonth: 685, TopDestination: "payments-db-proxy"},
		{Namespace: "ml-inference", Workload: "model-server-a100", CrossZoneGb: 38, CrossZoneUsdMonth: 380, TotalUsdMonth: 380, TopDestination: "embedder"},
	}
	total := 0.0
	for _, c := range costs {
		total += c.TotalUsdMonth
	}
	return &kuberov1.ListNetworkCostsResponse{Costs: limitSlice(costs, int(r.GetLimit())), TotalUsdMonth: total, Source: "demo"}, nil
}

// ─── alerts ──────────────────────────────────────────────────────────────

func (d Demo) ListAlerts(_ context.Context, r *kuberov1.ListAlertsRequest) (*kuberov1.ListAlertsResponse, error) {
	now := d.now().UTC()
	all := []*kuberov1.Alert{
		{Id: "alrt-payments-errors", RuleId: "rule-payments-errors", RuleName: "PaymentsErrorRate", Kind: "logs",
			State: "firing", Severity: "critical", Labels: map[string]string{"namespace": "payments"}, Value: 6.3,
			Summary: "payments error rate 6.3% (threshold 2%)", StartedAt: now.Add(-10 * time.Hour).Format(time.RFC3339),
			FiredAt: now.Add(-9*time.Hour - 55*time.Minute).Format(time.RFC3339), LinkPath: "/logs?namespace=payments&level=error"},
		{Id: "alrt-gpu-idle", RuleId: "rule-gpu-idle", RuleName: "GPUIdleSpend", Kind: "cost",
			State: "firing", Severity: "warn", Labels: map[string]string{"namespace": "ml-inference"}, Value: 7,
			Summary: "ml-inference GPU utilisation 7% while spending $41k/mo on A100s", StartedAt: now.Add(-70 * time.Hour).Format(time.RFC3339),
			FiredAt: now.Add(-69 * time.Hour).Format(time.RFC3339), LinkPath: "/workloads/eks-use1-prod/ml-inference/model-server-a100"},
		{Id: "alrt-ch-disk", RuleId: "rule-ch-disk", RuleName: "ClickHouseDiskPressure", Kind: "event",
			State: "resolved", Severity: "warn", Summary: "ClickHouse volume 86% full", StartedAt: now.Add(-30 * time.Hour).Format(time.RFC3339),
			ResolvedAt: now.Add(-26 * time.Hour).Format(time.RFC3339)},
	}
	out := &kuberov1.ListAlertsResponse{}
	for _, a := range all {
		if st := r.GetState(); st != "" && a.State != st {
			continue
		}
		switch a.State {
		case "firing":
			out.Firing++
		case "pending":
			out.Pending++
		}
		out.Alerts = append(out.Alerts, a)
	}
	out.Alerts = limitSlice(out.Alerts, int(r.GetLimit()))
	return out, nil
}

func limitSlice[T any](in []T, n int) []T {
	if n > 0 && len(in) > n {
		return in[:n]
	}
	return in
}
