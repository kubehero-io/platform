// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package profiles

import (
	"math"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

func TestProfilesAgainstClickHouse(t *testing.T) {
	db := chtest.Open(t, "profiles")
	now := time.Now().UTC()
	hour := timewin.FloorHour(now).Add(-3 * time.Hour) // [hour, hour+1h) current
	base := hour.Add(-24 * time.Hour)                  // baseline window

	chtest.Insert(t, db, "profile_stacks", []string{"stack_hash", "frames", "last_seen"}, [][]any{
		{uint64(1), []string{"main", "handle", "json.Marshal"}, now},
		{uint64(2), []string{"main", "handle", "db.Query"}, now},
		{uint64(3), []string{"main", "walk", "visit", "walk"}, now},
		{uint64(1), []string{"main", "handle", "json.Marshal"}, now}, // duplicate write
		// hash 4 is sampled but its frames were never written/evicted.
	})
	cols := []string{"ts", "org_id", "cluster_id", "service", "namespace", "workload", "pod", "container", "node",
		"type", "unit", "origin", "labels", "stack_hash", "value", "duration_ns"}
	row := func(ts time.Time, hash uint64, v int64, typ string) []any {
		return []any{ts, "default", "c1", "checkout", "shop", "checkout", "checkout-7d9f8c6b5-bcdfg", "app", "n1",
			typ, "nanoseconds", "ebpf", map[string]string{"version": "v2"}, hash, v, int64(10e9)}
	}
	var samples [][]any
	for i := 0; i < 3; i++ {
		ts := hour.Add(time.Duration(i*10) * time.Minute)
		samples = append(samples, row(ts, 1, 100e6, "cpu"), row(ts, 2, 50e6, "cpu"), row(ts, 3, 20e6, "cpu"), row(ts, 4, 10e6, "cpu"))
	}
	samples = append(samples, row(hour, 1, 999, "alloc_space"))
	// Baseline: marshal was cheaper.
	samples = append(samples, row(base, 1, 100e6, "cpu"), row(base, 2, 150e6, "cpu"))
	chtest.Insert(t, db, "profile_samples", cols, samples)

	// CPU cost for pricing: $0.001/s of CPU cost for the profiled hour.
	var cost [][]any
	for ts := hour; ts.Before(hour.Add(time.Hour)); ts = ts.Add(time.Minute) {
		cost = append(cost, []any{ts.UnixMilli(), "default", "c1", "n1", "shop", "checkout-7d9f8c6b5-bcdfg", "t", "", "general",
			"aws", "us-east-1", "m7i.large", "on-demand", "", uint32(1000), uint64(1 << 30), float32(0), 0.0015, 0.0,
			float32(60), "checkout", "Deployment", "z1", uint32(500), uint64(1 << 29), 0.001, 0.0005})
	}
	chtest.Insert(t, db, "pod_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "namespace", "pod", "team",
		"cost_center", "nodepool", "cloud", "region", "sku", "lifecycle", "gpu_kind", "cpu_millicores", "mem_bytes",
		"gpu_util_pct", "cost_usd_sec", "recoverable_usd_sec", "interval_sec", "workload", "workload_kind", "zone",
		"cpu_usage_millicores", "mem_usage_bytes", "cpu_cost_usd_sec", "ram_cost_usd_sec"}, cost)

	s := &Service{CH: db}
	sel := &kuberov1.ProfileSelector{Service: "checkout", Labels: map[string]string{"version": "v2"}}
	start, end := hour.UnixMilli(), hour.Add(time.Hour).UnixMilli()

	fg, err := s.GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{Selector: sel,
		StartUnixMs: start, EndUnixMs: end,
		BaselineStartUnixMs: base.UnixMilli(), BaselineEndUnixMs: base.Add(time.Hour).UnixMilli()}))
	if err != nil {
		t.Fatal(err)
	}
	m := fg.Msg
	if m.GetTotal() != 540e6 || m.GetSamples() != 12 || m.GetUnit() != "nanoseconds" || m.GetBaselineTotal() != 250e6 {
		t.Fatalf("header: total %d samples %d unit %q baseline %d", m.GetTotal(), m.GetSamples(), m.GetUnit(), m.GetBaselineTotal())
	}
	byPath := map[string]*kuberov1.FlameNode{}
	nodes := m.GetNodes()
	var path func(i int32) string
	path = func(i int32) string {
		if i <= 0 {
			return ""
		}
		return path(nodes[i].GetParent()) + "/" + nodes[i].GetName()
	}
	for i, n := range nodes {
		byPath[path(int32(i))] = n
	}
	if n := byPath["/main/handle/json.Marshal"]; n == nil || n.GetTotal() != 300e6 || n.GetBaselineTotal() != int64(math.Round(100e6*540.0/250)) {
		t.Fatalf("json.Marshal: %+v", n)
	}
	if n := byPath["/"+unknownStack]; n == nil || n.GetTotal() != 30e6 {
		t.Fatalf("evicted stack must stay visible: %+v", n)
	}
	if n := byPath["/main/walk/visit/walk"]; n == nil || n.GetSelf() != 60e6 {
		t.Fatalf("recursive path: %+v", n)
	}
	// Pricing: $0.001/s CPU over the hour → $3.60/h → × 720 = $2592/mo.
	if math.Abs(m.GetCostUsdMonth()-2592) > 0.01 {
		t.Fatalf("cost_usd_month = %v", m.GetCostUsdMonth())
	}

	// A tight stack cap keeps totals exact via "[other stacks]".
	capped := &Service{CH: db, MaxStacks: 1}
	c, err := capped.GetFlamegraph(viewer(), connect.NewRequest(&kuberov1.GetFlamegraphRequest{Selector: sel, StartUnixMs: start, EndUnixMs: end}))
	if err != nil {
		t.Fatal(err)
	}
	var other int64
	for _, n := range c.Msg.GetNodes() {
		if n.GetName() == otherStacks {
			other = n.GetTotal()
		}
	}
	if c.Msg.GetTotal() != 540e6 || other != 240e6 {
		t.Fatalf("capped: total %d other %d", c.Msg.GetTotal(), other)
	}

	top, err := s.GetTopFunctions(viewer(), connect.NewRequest(&kuberov1.GetTopFunctionsRequest{Selector: sel,
		StartUnixMs: start, EndUnixMs: end, OrderBy: "total"}))
	if err != nil {
		t.Fatal(err)
	}
	fs := top.Msg.GetFunctions()
	by := map[string]*kuberov1.TopFunction{}
	for _, f := range fs {
		by[f.GetName()] = f
	}
	if fs[0].GetName() != "main" || by["walk"].GetTotal() != 60e6 || by["json.Marshal"].GetSelf() != 300e6 {
		t.Fatalf("top functions: %+v", fs)
	}
	if math.Abs(by["json.Marshal"].GetSelfCostUsdMonth()-2592*300/540) > 0.01 {
		t.Fatalf("self cost %v", by["json.Marshal"].GetSelfCostUsdMonth())
	}

	targets, err := s.ListProfileTargets(viewer(), connect.NewRequest(&kuberov1.ListProfileTargetsRequest{StartUnixMs: start, EndUnixMs: end}))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets.Msg.GetTargets()) != 1 {
		t.Fatalf("targets: %+v", targets.Msg.GetTargets())
	}
	tg := targets.Msg.GetTargets()[0]
	if tg.GetService() != "checkout" || len(tg.GetTypes()) != 2 || tg.GetOrigin() != "ebpf" {
		t.Fatalf("target: %+v", tg)
	}
	// 540ms of CPU in a 1h window.
	if math.Abs(tg.GetCpuCoresAvg()-0.54/3600) > 1e-9 {
		t.Fatalf("cpu cores avg %v", tg.GetCpuCoresAvg())
	}
	// Whole-workload cost ($0.0015/s) scaled to a month.
	if math.Abs(tg.GetCostUsdMonth()-0.0015*3600*720) > 0.01 {
		t.Fatalf("target cost %v", tg.GetCostUsdMonth())
	}
}
