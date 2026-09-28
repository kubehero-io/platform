// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package source assembles the briefing Snapshot from the control
// plane's READ-ONLY RPCs (via internal/backend). The advisor never calls
// mutation RPCs and never talks to the Kubernetes API — backend is the
// only data-in path, and this package only reads through it.
package source

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
)

// Snapshot is the compact view of a cluster (or the fleet) the brains
// reason over. It is also the JSON the LLM brain reads, so field names
// are chosen to be self-explanatory and every list is short.
type Snapshot struct {
	// Origin is "control-plane" or "demo".
	Origin    string                `json:"origin"`
	ClusterID string                `json:"clusterId,omitempty"`
	Window    string                `json:"window"`
	Clusters  []Cluster             `json:"clusters,omitempty"`
	Waste     []WasteRecommendation `json:"wasteRecommendations,omitempty"`
	Anomalies []Anomaly             `json:"anomalies,omitempty"`
	Spend     *TeamSpendReport      `json:"teamSpend,omitempty"`
	BurnRate  *BurnRate             `json:"burnRate,omitempty"`

	// Enrichment. Each one is optional: a failed or unimplemented RPC
	// leaves it empty and the brains simply say less.
	CostAllocation []NamespaceCost   `json:"costAllocationTopNamespaces,omitempty"`
	Efficiency     *Efficiency       `json:"efficiency,omitempty"`
	Rightsizing    []RightsizingItem `json:"topRightsizing,omitempty"`
	FiringAlerts   []FiringAlert     `json:"alertsFiring,omitempty"`
	NetworkTop     []NetworkTalker   `json:"networkCostTopTalkers,omitempty"`
	LogErrors      *LogErrorVolume   `json:"logErrorVolume,omitempty"`
}

type Cluster struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Cloud  string `json:"cloud"`
	Region string `json:"region"`
	Nodes  int32  `json:"nodes"`
}

type WasteRecommendation struct {
	Rank                string  `json:"rank"`
	Workload            string  `json:"workload"`
	Namespace           string  `json:"namespace"`
	Cluster             string  `json:"cluster"`
	Cloud               string  `json:"cloud"`
	Signal              string  `json:"signal"`
	RecoverableUSDMonth float64 `json:"recoverableUsdMonth"`
	Action              string  `json:"action"`
	Severity            string  `json:"severity"`
}

type Anomaly struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Title          string  `json:"title"`
	Subject        string  `json:"subject"`
	Detail         string  `json:"detail"`
	DeltaPct       float64 `json:"deltaPct"`
	ImpactUSDMonth float64 `json:"impactUsdMonth"`
	Severity       string  `json:"severity"`
	Source         string  `json:"source"`
	LinkPath       string  `json:"linkPath"`
}

type TeamSpend struct {
	Team                string  `json:"team"`
	CostCenter          string  `json:"costCenter"`
	SpendUSDMonth       float64 `json:"spendUsdMonth"`
	RecoverableUSDMonth float64 `json:"recoverableUsdMonth"`
	GPUIdleUSDMonth     float64 `json:"gpuIdleUsdMonth"`
}

type TeamSpendReport struct {
	Teams                    []TeamSpend `json:"teams"`
	FleetTotalUSDMonth       float64     `json:"fleetTotalUsdMonth"`
	FleetRecoverableUSDMonth float64     `json:"fleetRecoverableUsdMonth"`
}

type BurnRate struct {
	BurnRateMilli int32  `json:"burnRateMilli"`
	Available     bool   `json:"available"`
	Source        string `json:"source"`
}

// NamespaceCost is one namespace's allocated cost over the window.
type NamespaceCost struct {
	Namespace      string  `json:"namespace"`
	CostUSD        float64 `json:"costUsd"`      // spent in the window
	CostUSDMonth   float64 `json:"costUsdMonth"` // window cost extrapolated to 730h
	CPUEfficiency  float64 `json:"cpuEfficiency"`
	RAMEfficiency  float64 `json:"ramEfficiency"`
	RecoverableUSD float64 `json:"recoverableUsd"`
}

type Efficiency struct {
	Score               float64 `json:"score"` // 0..100
	CPUEfficiency       float64 `json:"cpuEfficiency"`
	RAMEfficiency       float64 `json:"ramEfficiency"`
	IdleUSDMonth        float64 `json:"idleUsdMonth"`
	RecoverableUSDMonth float64 `json:"recoverableUsdMonth"`
}

type RightsizingItem struct {
	Cluster         string  `json:"cluster"`
	Namespace       string  `json:"namespace"`
	Workload        string  `json:"workload"`
	Kind            string  `json:"kind"`
	Container       string  `json:"container"`
	SavingsUSDMonth float64 `json:"savingsUsdMonth"`
	Confidence      string  `json:"confidence"`
	Direction       string  `json:"direction"`
	Reason          string  `json:"reason"`
	OOMKills        int32   `json:"oomKills,omitempty"`
}

type FiringAlert struct {
	Name      string  `json:"name"`
	Namespace string  `json:"namespace,omitempty"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	Value     float64 `json:"value"`
	FiredAt   string  `json:"firedAt"`
	LinkPath  string  `json:"linkPath,omitempty"`
}

type NetworkTalker struct {
	Namespace         string  `json:"namespace"`
	Workload          string  `json:"workload"`
	EgressUSDMonth    float64 `json:"egressUsdMonth"`
	CrossZoneUSDMonth float64 `json:"crossZoneUsdMonth"`
	TotalUSDMonth     float64 `json:"totalUsdMonth"`
	TopDestination    string  `json:"topDestination"`
}

// LogErrorVolume summarises error-level log lines over the window.
type LogErrorVolume struct {
	TotalLines  int64             `json:"totalErrorLines"`
	ByNamespace []NamespaceErrors `json:"byNamespace"`
}

type NamespaceErrors struct {
	Namespace string `json:"namespace"`
	Lines     int64  `json:"lines"`
	// SpikeRatio compares the error rate in the last quarter of the
	// window with the rest of it; >= 3 reads as a spike.
	SpikeRatio float64 `json:"spikeRatio"`
}

// Source produces a Snapshot for a cluster + window.
type Source interface {
	Fetch(ctx context.Context, clusterID, window string) (*Snapshot, error)
}

// Enrichment list caps: the snapshot is prompt input, so it stays small.
const (
	topNamespaces  = 8
	topRightsizing = 5
	topAlerts      = 10
	topNetwork     = 5
	topLogNS       = 5
)

// ControlPlane builds snapshots from a Backend (the live control plane,
// or backend.Demo for the demo source).
type ControlPlane struct {
	Backend backend.Backend
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// NewControlPlane returns a source over b.
func NewControlPlane(b backend.Backend) *ControlPlane {
	return &ControlPlane{Backend: b}
}

func (c *ControlPlane) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Fetch reads every signal concurrently. Core signals (clusters, waste,
// anomalies, team spend) decide reachability; enrichment failures are
// silently skipped.
func (c *ControlPlane) Fetch(ctx context.Context, clusterID, window string) (*Snapshot, error) {
	snap := &Snapshot{Origin: c.Backend.Origin(), ClusterID: clusterID, Window: window}
	b := c.Backend
	now := c.now()
	windowDur := 24 * time.Hour
	if window == "7d" {
		windowDur = 7 * 24 * time.Hour
	}

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	run := func(core bool, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil && core {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}

	run(true, func() error {
		res, err := b.ListClusters(ctx, &kuberov1.ListClustersRequest{PageSize: 50})
		if err != nil {
			return fmt.Errorf("ListClusters: %w", err)
		}
		snap.Clusters = clustersFrom(res)
		return nil
	})
	run(true, func() error {
		res, err := b.ListWasteRecommendations(ctx, &kuberov1.ListWasteRecommendationsRequest{ClusterId: clusterID, Limit: 10})
		if err != nil {
			return fmt.Errorf("ListWasteRecommendations: %w", err)
		}
		snap.Waste = wasteFrom(res)
		return nil
	})
	run(true, func() error {
		res, err := b.ListAnomalies(ctx, &kuberov1.ListAnomaliesRequest{Window: window, Limit: 10})
		if err != nil {
			return fmt.Errorf("ListAnomalies: %w", err)
		}
		snap.Anomalies = anomaliesFrom(res)
		return nil
	})
	run(true, func() error {
		res, err := b.GetTeamSpend(ctx, &kuberov1.GetTeamSpendRequest{Window: window})
		if err != nil {
			return fmt.Errorf("GetTeamSpend: %w", err)
		}
		snap.Spend = spendFrom(res)
		return nil
	})

	// ─── enrichment (optional) ────────────────────────────────────────
	run(false, func() error {
		res, err := b.GetAllocation(ctx, &kuberov1.GetAllocationRequest{
			Window: window, Aggregate: []string{"namespace"}, ClusterId: clusterID,
		})
		if err != nil || isDemoLabelMismatch(snap.Origin, res.GetSource()) {
			return err
		}
		snap.CostAllocation = namespaceCostsFrom(res)
		return nil
	})
	run(false, func() error {
		res, err := b.GetEfficiency(ctx, &kuberov1.GetEfficiencyRequest{ClusterId: clusterID, Window: "7d"})
		if err != nil || isDemoLabelMismatch(snap.Origin, res.GetSource()) {
			return err
		}
		snap.Efficiency = &Efficiency{
			Score: res.GetScore(), CPUEfficiency: res.GetCpuEfficiency(), RAMEfficiency: res.GetRamEfficiency(),
			IdleUSDMonth: res.GetIdleCostUsdMonth(), RecoverableUSDMonth: res.GetRecoverableUsdMonth(),
		}
		return nil
	})
	run(false, func() error {
		res, err := b.ListRightsizing(ctx, &kuberov1.ListRightsizingRequest{ClusterId: clusterID, Window: "7d", Limit: 50})
		if err != nil || isDemoLabelMismatch(snap.Origin, res.GetSource()) {
			return err
		}
		snap.Rightsizing = rightsizingFrom(res)
		return nil
	})
	run(false, func() error {
		res, err := b.ListAlerts(ctx, &kuberov1.ListAlertsRequest{State: "firing", Limit: 50})
		if err != nil {
			return err
		}
		snap.FiringAlerts = alertsFrom(res)
		return nil
	})
	run(false, func() error {
		res, err := b.ListNetworkCosts(ctx, &kuberov1.ListNetworkCostsRequest{
			ClusterId: clusterID, StartUnixMs: now.Add(-windowDur).UnixMilli(), EndUnixMs: now.UnixMilli(), Limit: 20,
		})
		if err != nil || isDemoLabelMismatch(snap.Origin, res.GetSource()) {
			return err
		}
		snap.NetworkTop = networkFrom(res)
		return nil
	})
	run(false, func() error {
		res, err := b.GetLogVolume(ctx, &kuberov1.GetLogVolumeRequest{
			Query: `{level="error"}`, GroupBy: "namespace", ClusterId: clusterID,
			StartUnixMs: now.Add(-windowDur).UnixMilli(), EndUnixMs: now.UnixMilli(),
		})
		if err != nil {
			return err
		}
		snap.LogErrors = logErrorsFrom(res)
		return nil
	})

	wg.Wait()

	// GetBurnRate needs a ceiling to compare against; the fleet total is
	// a conservative reference when we have one.
	if snap.Spend != nil && snap.Spend.FleetTotalUSDMonth > 0 {
		if res, err := b.GetBurnRate(ctx, &kuberov1.GetBurnRateRequest{
			ClusterId: clusterID, Window: "24h", MonthlyCeilingUsd: snap.Spend.FleetTotalUSDMonth,
		}); err == nil {
			snap.BurnRate = &BurnRate{BurnRateMilli: res.GetBurnRateMilli(), Available: res.GetAvailable(), Source: res.GetSource()}
		}
	}

	if snap.Clusters == nil && snap.Waste == nil && snap.Anomalies == nil && snap.Spend == nil {
		return nil, fmt.Errorf("control plane unreachable: %w", errors.Join(errs...))
	}
	return snap, nil
}

// isDemoLabelMismatch drops enrichment the control plane labels "demo"
// while the snapshot claims to be live: a live briefing must never mix
// in fixture rows as if they were real.
func isDemoLabelMismatch(origin, src string) bool {
	return origin != "demo" && src == "demo"
}

func clustersFrom(res *kuberov1.ListClustersResponse) []Cluster {
	out := []Cluster{}
	for _, c := range res.GetClusters() {
		out = append(out, Cluster{ID: c.GetId(), Name: c.GetName(), Cloud: c.GetCloud(), Region: c.GetRegion(), Nodes: c.GetNodes()})
	}
	return out
}

func wasteFrom(res *kuberov1.ListWasteRecommendationsResponse) []WasteRecommendation {
	out := []WasteRecommendation{}
	for _, w := range res.GetRecommendations() {
		out = append(out, WasteRecommendation{
			Rank: w.GetRank(), Workload: w.GetWorkload(), Namespace: w.GetNamespace(), Cluster: w.GetCluster(),
			Cloud: w.GetCloud(), Signal: w.GetSignal(), RecoverableUSDMonth: w.GetRecoverableUsdMonth(),
			Action: w.GetAction(), Severity: w.GetSeverity(),
		})
	}
	return out
}

func anomaliesFrom(res *kuberov1.ListAnomaliesResponse) []Anomaly {
	out := []Anomaly{}
	for _, a := range res.GetAnomalies() {
		out = append(out, Anomaly{
			ID: a.GetId(), Kind: a.GetKind(), Title: a.GetTitle(), Subject: a.GetSubject(), Detail: a.GetDetail(),
			DeltaPct: a.GetDeltaPct(), ImpactUSDMonth: a.GetImpactUsdMonth(), Severity: a.GetSeverity(),
			Source: a.GetSource(), LinkPath: a.GetLinkPath(),
		})
	}
	return out
}

func spendFrom(res *kuberov1.GetTeamSpendResponse) *TeamSpendReport {
	out := &TeamSpendReport{
		FleetTotalUSDMonth:       res.GetFleetTotalUsdMonth(),
		FleetRecoverableUSDMonth: res.GetFleetRecoverableUsdMonth(),
	}
	for _, t := range res.GetTeams() {
		out.Teams = append(out.Teams, TeamSpend{
			Team: t.GetTeam(), CostCenter: t.GetCostCenter(), SpendUSDMonth: t.GetSpendUsdMonth(),
			RecoverableUSDMonth: t.GetRecoverableUsdMonth(), GPUIdleUSDMonth: t.GetGpuIdleUsdMonth(),
		})
	}
	return out
}

func namespaceCostsFrom(res *kuberov1.GetAllocationResponse) []NamespaceCost {
	var out []NamespaceCost
	for _, a := range res.GetAllocations() {
		name := a.GetProperties()["namespace"]
		if name == "" {
			name = a.GetName()
		}
		if name == "" || name == "__idle__" {
			continue
		}
		monthly := 0.0
		if m := a.GetMinutes(); m > 0 {
			monthly = a.GetTotalCost() / (m / 60) * 730
		}
		out = append(out, NamespaceCost{
			Namespace: name, CostUSD: round2(a.GetTotalCost()), CostUSDMonth: round2(monthly),
			CPUEfficiency: round2(a.GetCpuEfficiency()), RAMEfficiency: round2(a.GetRamEfficiency()),
			RecoverableUSD: round2(a.GetRecoverableCost()),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Namespace < out[j].Namespace
	})
	return capN(out, topNamespaces)
}

func rightsizingFrom(res *kuberov1.ListRightsizingResponse) []RightsizingItem {
	var out []RightsizingItem
	for _, r := range res.GetRecommendations() {
		if r.GetDirection() == "ok" {
			continue
		}
		out = append(out, RightsizingItem{
			Cluster: r.GetCluster(), Namespace: r.GetNamespace(), Workload: r.GetWorkload(), Kind: r.GetWorkloadKind(),
			Container: r.GetContainer(), SavingsUSDMonth: round2(r.GetSavingsUsdMonth()), Confidence: r.GetConfidence(),
			Direction: r.GetDirection(), Reason: r.GetReason(), OOMKills: r.GetOomKills(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SavingsUSDMonth != out[j].SavingsUSDMonth {
			return out[i].SavingsUSDMonth > out[j].SavingsUSDMonth
		}
		return out[i].Namespace+"/"+out[i].Workload+"/"+out[i].Container < out[j].Namespace+"/"+out[j].Workload+"/"+out[j].Container
	})
	return capN(out, topRightsizing)
}

var severityRank = map[string]int{"critical": 0, "warn": 1, "info": 2}

func alertsFrom(res *kuberov1.ListAlertsResponse) []FiringAlert {
	var out []FiringAlert
	for _, a := range res.GetAlerts() {
		if a.GetState() != "firing" || a.GetSilenced() {
			continue
		}
		out = append(out, FiringAlert{
			Name: a.GetRuleName(), Namespace: a.GetLabels()["namespace"], Severity: a.GetSeverity(), Summary: a.GetSummary(),
			Value: a.GetValue(), FiredAt: a.GetFiredAt(), LinkPath: a.GetLinkPath(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if severityRank[out[i].Severity] != severityRank[out[j].Severity] {
			return severityRank[out[i].Severity] < severityRank[out[j].Severity]
		}
		return out[i].Name < out[j].Name
	})
	return capN(out, topAlerts)
}

func networkFrom(res *kuberov1.ListNetworkCostsResponse) []NetworkTalker {
	var out []NetworkTalker
	for _, c := range res.GetCosts() {
		out = append(out, NetworkTalker{
			Namespace: c.GetNamespace(), Workload: c.GetWorkload(), EgressUSDMonth: round2(c.GetEgressUsdMonth()),
			CrossZoneUSDMonth: round2(c.GetCrossZoneUsdMonth()), TotalUSDMonth: round2(c.GetTotalUsdMonth()),
			TopDestination: c.GetTopDestination(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalUSDMonth != out[j].TotalUSDMonth {
			return out[i].TotalUSDMonth > out[j].TotalUSDMonth
		}
		return out[i].Namespace+"/"+out[i].Workload < out[j].Namespace+"/"+out[j].Workload
	})
	return capN(out, topNetwork)
}

func logErrorsFrom(res *kuberov1.GetLogVolumeResponse) *LogErrorVolume {
	out := &LogErrorVolume{}
	for _, s := range res.GetSeries() {
		ns := s.GetLabels()["namespace"]
		if ns == "" {
			continue
		}
		lines, ratio := SeriesSpike(s.GetPoints())
		out.TotalLines += lines
		out.ByNamespace = append(out.ByNamespace, NamespaceErrors{Namespace: ns, Lines: lines, SpikeRatio: ratio})
	}
	if out.TotalLines == 0 && res.GetTotalLines() > 0 {
		out.TotalLines = res.GetTotalLines()
	}
	sort.SliceStable(out.ByNamespace, func(i, j int) bool {
		if out.ByNamespace[i].Lines != out.ByNamespace[j].Lines {
			return out.ByNamespace[i].Lines > out.ByNamespace[j].Lines
		}
		return out.ByNamespace[i].Namespace < out.ByNamespace[j].Namespace
	})
	out.ByNamespace = capN(out.ByNamespace, topLogNS)
	return out
}

// maxSpikeRatio caps spike ratios so a zero baseline stays JSON-encodable.
const maxSpikeRatio = 99

// Shift is the strongest recent level change in a series: the mean of
// its last RecentPoints points against the mean of everything before.
type Shift struct {
	Total        float64
	Recent       float64
	Baseline     float64
	ChangePct    float64
	RecentPoints int
}

// Ratio is Recent/Baseline, capped (0 without a baseline or recent data).
func (s Shift) Ratio() float64 {
	switch {
	case s.Baseline > 0:
		return round1(math.Min(s.Recent/s.Baseline, maxSpikeRatio))
	case s.Recent > 0:
		return maxSpikeRatio
	}
	return 0
}

// recentWindows are the trailing windows (in points) tried. A 10-hour
// jump in a 7-day hourly series shows at 6 points, where comparing a
// fixed "last quarter" would dilute it to a few percent.
var recentWindows = []int{3, 6, 12, 24}

// SeriesShift finds the strongest recent shift in points.
func SeriesShift(points []*kuberov1.Point) Shift {
	var out Shift
	for _, p := range points {
		out.Total += p.GetValue()
	}
	for _, w := range recentWindows {
		if w > len(points)/2 {
			break
		}
		cut := len(points) - w
		var head, tail float64
		for i, p := range points {
			if i < cut {
				head += p.GetValue()
			} else {
				tail += p.GetValue()
			}
		}
		base, recent := head/float64(cut), tail/float64(w)
		if base <= 0 {
			if recent > 0 && out.RecentPoints == 0 {
				out.Recent, out.RecentPoints = recent, w
			}
			continue
		}
		change := (recent/base - 1) * 100
		// Windows grow, so a later one wins unless its shift is clearly
		// smaller: on (near) ties the longer span is the steadier claim.
		if out.Baseline == 0 || math.Abs(change) >= math.Abs(out.ChangePct)-0.5 {
			out.Baseline, out.Recent, out.ChangePct, out.RecentPoints = base, recent, change, w
		}
	}
	return out
}

// SeriesSpike returns a series' total and its recent/baseline ratio.
func SeriesSpike(points []*kuberov1.Point) (int64, float64) {
	sh := SeriesShift(points)
	return int64(sh.Total), sh.Ratio()
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func capN[T any](in []T, n int) []T {
	if len(in) > n {
		return in[:n]
	}
	return in
}
