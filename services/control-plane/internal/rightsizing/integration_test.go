// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package rightsizing

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

func TestRecommendAgainstClickHouse(t *testing.T) {
	db := chtest.Open(t, "rightsizing")
	now := time.Now().UTC()
	day0 := now.Truncate(timewin.Day).Add(-3 * timewin.Day)

	// api/app: CPU uniform over [0.1, 0.5] cores (p95 ≈ 0.48), memory
	// 400 MiB with one 420 MiB peak; requests 2 cores / 1 GiB.
	// cache/redis: CPU pinned at 96% of its 0.25-core limit (throttling),
	// memory near its 512 MiB limit and OOM-killed twice.
	var usage [][]any
	i := 0
	for ts := day0; ts.Before(day0.Add(48 * time.Hour)); ts = ts.Add(time.Minute) {
		cpu := float32(0.1 + 0.4*float64(i%100)/99)
		mem := uint64(400 << 20)
		if i == 777 {
			mem = 420 << 20
		}
		i++
		for _, pod := range []string{"api-7d9f8c6b5-bcdfg", "api-7d9f8c6b5-hjklm"} {
			usage = append(usage, []any{ts, "default", "c1", "shop", "api", "Deployment", pod, "app", "n1", "payments",
				cpu, mem, float32(2), uint64(1 << 30), float32(4), uint64(2 << 30), uint32(0), ""})
		}
		usage = append(usage, []any{ts, "default", "c1", "shop", "cache", "StatefulSet", "cache-0", "redis", "n1", "payments",
			float32(0.24), uint64(500 << 20), float32(0.25), uint64(512 << 20), float32(0.25), uint64(512 << 20), uint32(2), "OOMKilled"})
	}
	chtest.Insert(t, db, "container_usage", []string{"ts", "org_id", "cluster_id", "namespace", "workload", "workload_kind",
		"pod", "container", "node", "team", "cpu_usage_cores", "mem_working_set_bytes", "cpu_request_cores",
		"mem_request_bytes", "cpu_limit_cores", "mem_limit_bytes", "restarts", "last_termination_reason"}, usage)

	chtest.Insert(t, db, "cluster_events", []string{"ts", "org_id", "cluster_id", "kind", "severity", "namespace", "workload",
		"pod", "container", "node", "reason", "message", "attributes", "count"}, [][]any{
		{day0.Add(5 * time.Hour), "default", "c1", "oom_killed", "warn", "shop", "cache", "cache-0", "redis", "n1", "OOMKilled", "", map[string]string{}, uint32(1)},
		{day0.Add(30 * time.Hour), "default", "c1", "oom_killed", "warn", "shop", "cache", "cache-0", "redis", "n1", "OOMKilled", "", map[string]string{}, uint32(1)},
	})

	// Billing: two api pods at 0.5 cores / 1 GiB billed, $0.00006/s CPU
	// and $0.00004/s RAM each → $0.432 per core-hour, $0.144 per GiB-hour.
	var cost [][]any
	for ts := day0; ts.Before(day0.Add(48 * time.Hour)); ts = ts.Add(5 * time.Minute) {
		for _, pod := range []string{"api-7d9f8c6b5-bcdfg", "api-7d9f8c6b5-hjklm"} {
			cost = append(cost, []any{ts.UnixMilli(), "default", "c1", "n1", "shop", pod, "payments", "", "general", "aws", "us-east-1",
				"m7i.large", "on-demand", "", uint32(500), uint64(1 << 30), float32(0), 0.0001, 0.0, float32(300), "api", "Deployment",
				"z1", uint32(250), uint64(512 << 20), 0.00006, 0.00004})
		}
	}
	chtest.Insert(t, db, "pod_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "namespace", "pod", "team", "cost_center",
		"nodepool", "cloud", "region", "sku", "lifecycle", "gpu_kind", "cpu_millicores", "mem_bytes", "gpu_util_pct",
		"cost_usd_sec", "recoverable_usd_sec", "interval_sec", "workload", "workload_kind", "zone",
		"cpu_usage_millicores", "mem_usage_bytes", "cpu_cost_usd_sec", "ram_cost_usd_sec"}, cost)

	e := &Engine{CH: db, CacheTTL: -1}
	w := timewin.Window{Start: day0, End: day0.Add(48 * time.Hour), Now: now}
	recs, err := e.Recommend(context.Background(), Query{Window: w, Options: Options{WindowLabel: "2d"}})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Recommendation{}
	for _, r := range recs {
		by[r.ID] = r
	}
	api, ok := by["c1/shop/api/app"]
	if !ok {
		t.Fatalf("no api rec in %v", recs)
	}
	if api.CPUP95 < 0.46 || api.CPUP95 > 0.50 {
		t.Errorf("cpu p95 = %v, want ≈0.48", api.CPUP95)
	}
	if api.CPURecommended < 0.54 || api.CPURecommended > 0.58 {
		t.Errorf("cpu rec = %v, want ≈0.555", api.CPURecommended)
	}
	if want := math.Ceil(420*1.15) * MiB; api.MemRecommended != want {
		t.Errorf("mem rec = %v MiB, want %v MiB (max 420Mi + 15%%)", api.MemRecommended/MiB, want/MiB)
	}
	if api.CPURequest != 2 || api.MemRequest != 1<<30 || api.Direction != Downsize || api.Confidence != "medium" {
		t.Errorf("api: req %v/%v dir %s conf %s", api.CPURequest, api.MemRequest, api.Direction, api.Confidence)
	}
	if math.Abs(api.Replicas-2) > 0.01 {
		t.Errorf("replicas = %v, want 2", api.Replicas)
	}
	// 2 cores → 0.555 at $0.432/core-h × 2 replicas × 720h, plus memory.
	wantCPU := (2 - api.CPURecommended) * 0.432 * 2 * 720
	if math.Abs(api.CPUSavingsMonth-wantCPU) > 0.01 {
		t.Errorf("cpu savings = %v, want %v", api.CPUSavingsMonth, wantCPU)
	}
	if api.ThrottleRisk != 0 {
		t.Errorf("api throttle risk = %v with a 4-core limit", api.ThrottleRisk)
	}

	redis := by["c1/shop/cache/redis"]
	if redis.OOMKills != 2 || redis.Direction != Upsize {
		t.Fatalf("redis: oom %d dir %s", redis.OOMKills, redis.Direction)
	}
	if redis.MemRecommended < 1.25*512*MiB {
		t.Errorf("redis mem rec %v MiB, want ≥ 640 MiB after OOM", redis.MemRecommended/MiB)
	}
	// p99 0.24 ≥ 90% of the 0.25 limit in every 5-minute bucket.
	if redis.ThrottleRisk < 0.99 {
		t.Errorf("redis throttle risk = %v, want ≈1", redis.ThrottleRisk)
	}
}
