// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package clickhouse_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

// Private database for this package's write-path tests (see chtest).
const testDB = "kh_ingest"

func TestSignalWriterRoundTrip(t *testing.T) {
	db := chtest.Open(t, testDB)
	ctx := context.Background()
	w := &clickhouse.SignalWriter{Conn: db.Native}
	now := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)

	logs := []clickhouse.LogRow{
		{TS: now.Add(123456789), OrgID: "default", ClusterID: "c1", Namespace: "shop", Workload: "api",
			WorkloadKind: "Deployment", Pod: "api-1", Container: "app", Node: "n1", Team: "payments",
			Stream: "stdout", Level: "error", TraceID: "abc", Labels: map[string]string{"app": "api"}, Body: "boom"},
		{TS: now, OrgID: "default", ClusterID: "c1", Namespace: "shop", Workload: "api", Pod: "api-1",
			Container: "app", Level: "info", Body: "exactly on the minute"},
	}
	if err := w.WriteLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	var body, app string
	var tsNano int64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT body, labels['app'], toUnixTimestamp64Nano(ts) FROM logs WHERE level = 'error'`).Scan(&body, &app, &tsNano); err != nil {
		t.Fatal(err)
	}
	if body != "boom" || app != "api" || tsNano != now.Add(123456789).UnixNano() {
		t.Fatalf("log row = %q %q %d", body, app, tsNano)
	}
	// Rollup with 0003's edge counters: one line sits exactly on the
	// minute boundary.
	var lines, bytes, edgeLines, edgeBytes uint64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT sum(lines), sum(bytes), sum(edge_lines), sum(edge_bytes)
		FROM log_volume_1m WHERE cluster_id = 'c1'`).Scan(&lines, &bytes, &edgeLines, &edgeBytes); err != nil {
		t.Fatal(err)
	}
	if lines != 2 || bytes != uint64(len("boom")+len("exactly on the minute")) || edgeLines != 1 || edgeBytes != uint64(len("exactly on the minute")) {
		t.Fatalf("log_volume_1m = lines %d bytes %d edge %d/%d", lines, bytes, edgeLines, edgeBytes)
	}

	if err := w.WriteProfileStacks(ctx, []clickhouse.ProfileStackRow{
		{StackHash: 42, Frames: []string{"main", "handler", "json.Marshal"}, LastSeen: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteProfileSamples(ctx, []clickhouse.ProfileSampleRow{
		{TS: now, OrgID: "default", ClusterID: "c1", Service: "api", Namespace: "shop", Type: "cpu",
			Unit: "nanoseconds", Origin: "ebpf", StackHash: 42, Value: 1e7, DurationNS: 15e9},
	}); err != nil {
		t.Fatal(err)
	}
	var frames []string
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT st.frames FROM profile_samples s
		JOIN profile_stacks st ON st.stack_hash = s.stack_hash WHERE s.service = 'api'`).Scan(&frames); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 || frames[2] != "json.Marshal" {
		t.Fatalf("frames = %v", frames)
	}

	if err := w.WriteFlows(ctx, []clickhouse.FlowRow{
		{TS: now, OrgID: "default", ClusterID: "c1", WindowSec: 15, SrcKind: "pod", SrcNamespace: "shop",
			SrcWorkload: "api", SrcZone: "a", DstKind: "external", DstName: "s3", Port: 443, Protocol: "tcp",
			Direction: "egress", Bytes: 2e9, Packets: 10, Egress: true, CostUSD: 0.18},
	}); err != nil {
		t.Fatal(err)
	}
	var flowCost float64
	var egress uint8
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT sum(cost_usd), max(egress) FROM net_flows_1h WHERE src_workload = 'api'`).Scan(&flowCost, &egress); err != nil {
		t.Fatal(err)
	}
	if math.Abs(flowCost-0.18) > 1e-9 || egress != 1 {
		t.Fatalf("net_flows_1h cost=%v egress=%d", flowCost, egress)
	}

	if err := w.WriteUsage(ctx, []clickhouse.UsageRow{
		{TS: now, OrgID: "default", ClusterID: "c1", Namespace: "shop", Workload: "api", Pod: "api-1",
			Container: "app", CPUUsageCores: 0.25, MemWorkingSetBytes: 1 << 28, CPURequestCores: 1, MemRequestBytes: 1 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	if n := db.Count(t, `SELECT sum(samples) FROM container_usage_5m WHERE workload = 'api'`); n != 1 {
		t.Fatalf("container_usage_5m samples = %d", n)
	}

	if err := w.WriteEvents(ctx, []clickhouse.EventRow{
		{TS: now, OrgID: "default", ClusterID: "c1", Kind: "oom_killed", Severity: "warn", Namespace: "shop",
			Pod: "api-1", Reason: "OOMKilled", Message: "container app", Attributes: map[string]string{"limit": "256Mi"}, Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if n := db.Count(t, `SELECT count() FROM cluster_events WHERE kind = 'oom_killed' AND attributes['limit'] = '256Mi'`); n != 1 {
		t.Fatalf("cluster_events = %d", n)
	}
}

func TestPodCostWriterStoresAllTables(t *testing.T) {
	db := chtest.Open(t, testDB)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour)
	w := &clickhouse.PodCostWriter{DB: db.SQL, Linger: 10 * time.Millisecond, Now: func() time.Time { return hour.Add(30 * time.Minute) }}

	var samples []clickhouse.Sample
	for i := 0; i < 12; i++ { // one minute of 5s samples
		samples = append(samples, clickhouse.Sample{
			OrgID: "default", ClusterID: "c1", Namespace: "shop", Pod: "api-1", Node: "n1",
			Workload: "api", WorkloadKind: "Deployment", Zone: "us-east-1a", Cloud: "aws",
			CPUMilli: 500, CPUUsageMilli: 250, MemBytes: 1 << 30, MemUsageBytes: 1 << 29,
			CostUSDSec: 0.001, CPUCostUSDSec: 0.0006, RAMCostUSDSec: 0.0004, GPUCount: 0,
			IntervalSec: 5, TsUnixMS: hour.Add(time.Duration(i) * 5 * time.Second).UnixMilli(),
			Labels: map[string]string{"app.kubernetes.io/name": "api"},
		})
	}
	samples = append(samples, clickhouse.Sample{ClusterID: "c1", Pod: "", CostUSDSec: 1})                                   // no pod
	samples = append(samples, clickhouse.Sample{ClusterID: "c1", Pod: "x", CostUSDSec: math.NaN()})                         // bad cost
	samples = append(samples, clickhouse.Sample{ClusterID: "c1", Pod: "x", TsUnixMS: hour.Add(24 * time.Hour).UnixMilli()}) // future
	nodes := []clickhouse.NodeSample{{
		OrgID: "default", ClusterID: "c1", Node: "n1", Cloud: "aws", PricePerHour: 3.6, PriceSource: "estimate",
		CostUSDSec: 0.001, IdleUSDSec: 0.0004, IntervalSec: 0, TsUnixMS: hour.UnixMilli(),
	}}

	res, err := w.Write(ctx, samples, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 13 || res.Dropped != 3 {
		t.Fatalf("result = %+v, want written 13 dropped 3", res)
	}
	var cost, cpuCost, podSec float64
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT sum(cost_usd), sum(cpu_cost_usd), sum(pod_seconds) FROM workload_cost_1h
		WHERE cluster_id = 'c1' AND workload = 'api' AND zone = 'us-east-1a' AND cloud = 'aws'`).Scan(&cost, &cpuCost, &podSec); err != nil {
		t.Fatal(err)
	}
	if math.Abs(cost-0.06) > 1e-9 || math.Abs(cpuCost-0.036) > 1e-9 || podSec != 60 {
		t.Fatalf("workload_cost_1h cost=%v cpu=%v pod_seconds=%v", cost, cpuCost, podSec)
	}
	var nodeCost, idle, nodeSec float64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT sum(cost_usd), sum(idle_usd), sum(node_seconds) FROM node_cost_1h WHERE node = 'n1'`).Scan(&nodeCost, &idle, &nodeSec); err != nil {
		t.Fatal(err)
	}
	// interval 0 → 5s default.
	if math.Abs(nodeCost-0.005) > 1e-12 || math.Abs(idle-0.002) > 1e-12 || nodeSec != 5 {
		t.Fatalf("node_cost_1h cost=%v idle=%v seconds=%v", nodeCost, idle, nodeSec)
	}
	var app string
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT labels['app.kubernetes.io/name'] FROM pod_metadata FINAL WHERE pod = 'api-1'`).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if app != "api" {
		t.Fatalf("pod_metadata label = %q", app)
	}
}

// Burn rate over real rows: 60 samples × 5s at $0.01/s is $3 in five
// minutes → $25 920/mo, i.e. 2.0× a $12 960 ceiling.
func TestBurnRateIntegrationUsesInterval(t *testing.T) {
	db := chtest.Open(t, testDB)
	ctx := context.Background()
	end := time.Now().UTC().Truncate(time.Second)
	w := &clickhouse.PodCostWriter{DB: db.SQL, Linger: 10 * time.Millisecond, Now: func() time.Time { return end }}
	var samples []clickhouse.Sample
	for i := 0; i < 60; i++ {
		samples = append(samples, clickhouse.Sample{
			OrgID: "default", ClusterID: "c1", Namespace: "shop", Pod: "api-1", CostUSDSec: 0.01, IntervalSec: 5,
			TsUnixMS: end.Add(-5*time.Minute + time.Duration(i)*5*time.Second).UnixMilli(),
		})
	}
	if _, err := w.Write(ctx, samples, nil); err != nil {
		t.Fatal(err)
	}
	p := &clickhouse.BurnRateProvider{DB: db.SQL, Now: func() time.Time { return end }}
	r, err := p.Compute(ctx, "c1", "shop", "5m", 12960)
	if err != nil {
		t.Fatal(err)
	}
	if r.BurnRateMilli != 2000 {
		t.Fatalf("burn rate = %d‰, want 2000‰ (pre-fix code reported 400‰)", r.BurnRateMilli)
	}
	// Fleet-wide (empty cluster + namespace) sees the same rows.
	if r, err := p.Compute(ctx, "", "", "5m", 12960); err != nil || r.BurnRateMilli != 2000 {
		t.Fatalf("fleet-wide burn rate = %+v, %v", r, err)
	}
}
