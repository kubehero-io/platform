// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package insights

import (
	"math"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func pp(cluster, wl, pod string, cpuMilli, memBytes, gpu, ageSec string, np string) pendingPod {
	a := map[string]string{"cpu_millicores": cpuMilli, "mem_bytes": memBytes, "gpu": gpu, "age_sec": ageSec}
	if np != "" {
		a["nodepool"] = np
	}
	return pendingPod{cluster: cluster, namespace: "data", workload: wl, pod: pod, attrs: a, lastTS: now.Add(-2 * time.Minute)}
}

var skus = []SKU{
	{Cluster: "c1", Nodepool: "general", Name: "m7i.xlarge", PricePerHour: 0.2016, CPUCores: 4, MemBytes: 16 << 30},
	{Cluster: "c1", Nodepool: "big", Name: "m7i.4xlarge", PricePerHour: 0.8064, CPUCores: 16, MemBytes: 64 << 30},
	{Cluster: "c1", Nodepool: "gpu", Name: "g6.xlarge", PricePerHour: 0.805, CPUCores: 4, MemBytes: 16 << 30, GPUs: 1, GPUKind: "L4"},
	{Cluster: "c2", Nodepool: "general", Name: "n2-standard-8", PricePerHour: 0.39, CPUCores: 8, MemBytes: 32 << 30},
}

func TestBuildDemandsPricesCheapestFittingSKU(t *testing.T) {
	pods := []pendingPod{
		// 3 pods × 3 cores / 8 GiB: only the 4-core node fits one pod
		// each by CPU → 3 nodes of m7i.xlarge ($0.2016/h).
		pp("c1", "etl", "etl-1", "3000", "8589934592", "0", "600", ""),
		pp("c1", "etl", "etl-2", "3000", "8589934592", "0", "1200", ""),
		pp("c1", "etl", "etl-3", "3000", "8589934592", "0", "60", ""),
		// A GPU pod only fits the GPU SKU.
		pp("c1", "trainer", "trainer-0", "2000", "4294967296", "1", "30", ""),
		// Nothing in c2 fits 12 cores.
		pp("c2", "huge", "huge-0", "12000", "1073741824", "0", "0", ""),
		// No requests recorded: affinity/taints, not capacity.
		pp("c2", "odd", "odd-0", "", "", "", "0", ""),
	}
	ds := buildDemands(pods, skus, now)
	by := map[string]Demand{}
	for _, d := range ds {
		by[d.Workload] = d
	}
	etl := by["etl"]
	if etl.PendingPods != 3 || etl.NodesToAdd != 3 || etl.SKU != "m7i.xlarge" {
		t.Fatalf("etl: %+v", etl)
	}
	if math.Abs(etl.RecommendedUSDMo-3*0.2016*720) > 1e-6 {
		t.Errorf("etl recommended $ = %v", etl.RecommendedUSDMo)
	}
	// 9 cores blocked × ($0.2016 / 4 cores) × 720h.
	if math.Abs(etl.BlockedUSDMo-9*0.2016/4*720) > 1e-6 {
		t.Errorf("etl blocked $ = %v", etl.BlockedUSDMo)
	}
	if etl.OldestAge != 20*time.Minute+2*time.Minute || FmtAge(etl.OldestAge) != "22m" {
		t.Errorf("oldest age %v", etl.OldestAge)
	}
	if !strings.Contains(etl.RecommendedAction, "scale nodepool general by 3 nodes") {
		t.Errorf("action %q", etl.RecommendedAction)
	}
	if tr := by["trainer"]; tr.SKU != "g6.xlarge" || tr.GPUKind != "L4" {
		t.Errorf("gpu pod must land on the GPU SKU: %+v", tr)
	}
	if h := by["huge"]; h.SKU != "" || !strings.Contains(h.RecommendedAction, "no node type") {
		t.Errorf("unfittable pod: %+v", h)
	}
	if o := by["odd"]; !strings.HasPrefix(o.RecommendedAction, "investigate") {
		t.Errorf("request-less pod: %+v", o)
	}
	if ds[0].BlockedUSDMo < ds[len(ds)-1].BlockedUSDMo {
		t.Error("demands must sort by blocked $ desc")
	}
	if ds[0].ID == "" || !strings.HasPrefix(ds[0].ID, "demand-") {
		t.Error("stable id")
	}
}

func TestCheapestFitPrefersHintedNodepool(t *testing.T) {
	s, per := cheapestFit(skus, "c1", "big", 1, 1<<30, 0)
	if s == nil || s.Name != "m7i.4xlarge" || per != 16 {
		t.Fatalf("hinted pool: %+v %d", s, per)
	}
	s, _ = cheapestFit(skus, "c1", "nonexistent", 1, 1<<30, 0)
	if s == nil || s.Name != "m7i.xlarge" {
		t.Fatalf("unknown hint falls back to the whole cluster: %+v", s)
	}
}

func TestScoreSpikes(t *testing.T) {
	steady := make([]float64, 25)
	for i := range steady {
		steady[i] = 100 + float64(i%3)
	}
	spike := append([]float64(nil), steady...)
	spike[24] = 900
	silent := make([]float64, 25)
	silent[24] = 400
	small := make([]float64, 25)
	small[24] = 10
	out := ScoreSpikes(map[WorkloadKey][]float64{
		{"c", "a", "steady"}: steady, {"c", "a", "spike"}: spike, {"c", "a", "silent"}: silent, {"c", "a", "small"}: small,
	}, 24, 3)
	names := map[string]LogSpike{}
	for _, s := range out {
		names[s.Workload] = s
	}
	if _, ok := names["spike"]; !ok {
		t.Error("spike not flagged")
	}
	if _, ok := names["silent"]; !ok {
		t.Error("burst out of silence not flagged")
	}
	if _, ok := names["steady"]; ok {
		t.Error("steady flagged")
	}
	if _, ok := names["small"]; ok {
		t.Error("tiny counts must not count as spikes")
	}
}

func TestFormatting(t *testing.T) {
	if FmtAge(4*time.Hour+12*time.Minute) != "4h 12m" || FmtAge(50*time.Hour) != "2d 2h" {
		t.Error("age")
	}
	if FmtCores(16) != "16 cores" || FmtCores(0.5) != "500m cores" || FmtBytes(64<<30) != "64 GiB" {
		t.Errorf("%s %s %s", FmtCores(16), FmtCores(0.5), FmtBytes(64<<30))
	}
}
