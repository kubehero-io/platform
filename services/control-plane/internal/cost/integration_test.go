// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

// Run with:
//
//	KUBEHERO_TEST_CLICKHOUSE_URL=clickhouse://kubehero:kubehero@localhost:19000/kubehero \
//	  go test -tags integration ./internal/cost/
package cost

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chfixture"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

const interval = 300 // seconds covered by each synthetic sample

type pod struct {
	cluster, ns, workload, kind, pod, node, zone, team, lifecycle string
	usdSec                                                        float64
}

var pods = []pod{
	{"c1", "shop", "api", "Deployment", "api-7d9f8c6b5-bcdfg", "n1", "z1", "payments", "on-demand", 0.0001},
	{"c1", "shop", "api", "Deployment", "api-7d9f8c6b5-hjklm", "n2", "z2", "payments", "on-demand", 0.0001},
	{"c1", "shop", "worker", "StatefulSet", "worker-0", "n2", "z2", "", "spot", 0.00005},
	{"c1", "kube-system", "coredns", "Deployment", "coredns-5d78c9869d-xqzvt", "n1", "z1", "platform", "on-demand", 0.00002},
	{"c2", "web", "frontend", "Deployment", "frontend-6b7f9d8c4-pqrst", "m1", "za", "web", "on-demand", 0.0002},
	{"c2", "web", "frontend", "Deployment", "frontend-6b7f9d8c4-vwxyz", "m1", "za", "web", "on-demand", 0.0002},
}

var nodes = []struct {
	cluster, node string
	usdSec        float64
}{{"c1", "n1", 0.0003}, {"c1", "n2", 0.0003}, {"c2", "m1", 0.0005}}

func seed(t *testing.T, db *sql.DB, day0 time.Time) {
	t.Helper()
	var podRows, nodeRows [][]any
	for ts := day0; ts.Before(day0.Add(48 * time.Hour)); ts = ts.Add(interval * time.Second) {
		ms := ts.UnixMilli()
		for _, p := range pods {
			podRows = append(podRows, []any{ms, "default", p.cluster, p.node, p.ns, p.pod, p.team, "cc1", "general",
				"aws", "us-east-1", "m7i.large", p.lifecycle, "", uint32(500), uint64(1 << 30), float32(0),
				p.usdSec, p.usdSec / 4, float32(interval), p.workload, p.kind, p.zone,
				uint32(250), uint64(512 << 20), p.usdSec * 0.6, p.usdSec * 0.4, float64(0), uint8(0)})
		}
		for _, n := range nodes {
			nodeRows = append(nodeRows, []any{ms, "default", n.cluster, n.node, "general", "aws", "us-east-1", "z1",
				"m7i.large", "on-demand", "", uint8(0), float32(interval), n.usdSec * 3600, "test",
				uint32(2000), uint64(8 << 30), uint32(1000), uint64(2 << 30), uint32(500), uint64(1 << 30),
				n.usdSec, n.usdSec / 2})
		}
	}
	chfixture.Insert(t, db, "pod_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "namespace", "pod", "team",
		"cost_center", "nodepool", "cloud", "region", "sku", "lifecycle", "gpu_kind", "cpu_millicores", "mem_bytes",
		"gpu_util_pct", "cost_usd_sec", "recoverable_usd_sec", "interval_sec", "workload", "workload_kind", "zone",
		"cpu_usage_millicores", "mem_usage_bytes", "cpu_cost_usd_sec", "ram_cost_usd_sec", "gpu_cost_usd_sec", "gpu_count"}, podRows)
	chfixture.Insert(t, db, "node_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "nodepool", "cloud", "region",
		"zone", "sku", "lifecycle", "gpu_kind", "gpu_count", "interval_sec", "price_per_hour", "price_source",
		"cpu_allocatable_millicores", "mem_allocatable_bytes", "cpu_requested_millicores", "mem_requested_bytes",
		"cpu_used_millicores", "mem_used_bytes", "cost_usd_sec", "idle_usd_sec"}, nodeRows)

	// Hourly flows from api: internet egress (seen once, at the sender)
	// and pod→pod cross-zone to worker, observed at BOTH ends — the
	// query must count it once.
	var flows [][]any
	flow := func(ts time.Time, dstKind, dstNS, dstWL, dstName, dir string, xz, eg uint8, usd float64) []any {
		return []any{ts, "default", "c1", uint16(300), "pod", "shop", "api", "api-7d9f8c6b5-bcdfg", "n1", "z1", "10.0.0.1", "api",
			dstKind, dstNS, dstWL, "", "", "z2", "10.0.0.2", "", dstName, uint16(443), "tcp", dir,
			uint64(1e6), uint64(1000), uint32(0), xz, eg, usd}
	}
	for h := 0; h < 48; h++ {
		ts := day0.Add(time.Duration(h) * time.Hour)
		flows = append(flows,
			flow(ts, "external", "", "", "s3.amazonaws.com", "egress", 0, 1, 0.002),
			flow(ts, "pod", "shop", "worker", "worker", "egress", 1, 0, 0.001),
			flow(ts, "pod", "shop", "worker", "worker", "ingress", 1, 0, 0.001),
		)
	}
	chfixture.Insert(t, db, "net_flows", []string{"ts", "org_id", "cluster_id", "window_sec", "src_kind", "src_namespace",
		"src_workload", "src_pod", "src_node", "src_zone", "src_ip", "src_name", "dst_kind", "dst_namespace",
		"dst_workload", "dst_pod", "dst_node", "dst_zone", "dst_ip", "dst_service", "dst_name", "port", "protocol",
		"direction", "bytes", "packets", "retransmits", "cross_zone", "egress", "cost_usd"}, flows)

	chfixture.Insert(t, db, "pod_metadata", []string{"org_id", "cluster_id", "namespace", "pod", "workload", "workload_kind",
		"node", "team", "cost_center", "labels", "updated_at"}, [][]any{
		{"default", "c1", "shop", "api-7d9f8c6b5-bcdfg", "api", "Deployment", "n1", "payments", "cc1", map[string]string{"app": "api"}, day0},
		{"default", "c1", "shop", "api-7d9f8c6b5-hjklm", "api", "Deployment", "n2", "payments", "cc1", map[string]string{"app": "api"}, day0},
		{"default", "c1", "shop", "worker-0", "worker", "StatefulSet", "n2", "", "cc1", map[string]string{"app": "worker"}, day0},
		{"default", "c2", "web", "frontend-6b7f9d8c4-pqrst", "frontend", "Deployment", "m1", "web", "cc1", map[string]string{"app": "web"}, day0},
		{"default", "c2", "web", "frontend-6b7f9d8c4-vwxyz", "frontend", "Deployment", "m1", "web", "cc1", map[string]string{"app": "web"}, day0},
	})

	// Container requests for api: app 0.4 cores / 0.8 GiB, sidecar
	// 0.1 / 0.2 → an 80/20 split.
	var usage [][]any
	for ts := day0; ts.Before(day0.Add(2 * time.Hour)); ts = ts.Add(time.Minute) {
		for _, c := range []struct {
			name string
			cpu  float32
			mem  uint64
		}{{"app", 0.4, 800 << 20}, {"sidecar", 0.1, 200 << 20}} {
			usage = append(usage, []any{ts, "default", "c1", "shop", "api", "Deployment", "api-7d9f8c6b5-bcdfg", c.name, "n1", "payments",
				c.cpu / 2, c.mem / 2, c.cpu, c.mem, float32(0), uint64(0), uint32(0), ""})
		}
	}
	chfixture.Insert(t, db, "container_usage", []string{"ts", "org_id", "cluster_id", "namespace", "workload", "workload_kind",
		"pod", "container", "node", "team", "cpu_usage_cores", "mem_working_set_bytes", "cpu_request_cores",
		"mem_request_bytes", "cpu_limit_cores", "mem_limit_bytes", "restarts", "last_termination_reason"}, usage)
}

func cents(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) >= 0.005 {
		t.Errorf("%s = %.6f, want %.6f", what, got, want)
	}
}

func TestAllocationAgainstClickHouse(t *testing.T) {
	db := chfixture.Open(t, "cost")
	now := time.Now().UTC()
	day0 := now.Truncate(timewin.Day).Add(-3 * timewin.Day)
	seed(t, db, day0)
	e := &Engine{CH: db}
	ctx := context.Background()

	run := func(t *testing.T, q AllocationQuery) map[string]*Alloc {
		t.Helper()
		sets, err := e.Allocate(ctx, q)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if len(sets) != 1 {
			t.Fatalf("sets = %d", len(sets))
		}
		return byName(sets[0])
	}
	aligned := timewin.Window{Start: day0, End: day0.Add(48 * time.Hour), Now: now}
	ns := []Dim{{Name: DimNamespace}}

	t.Run("aligned window, idle separate, to the cent", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: ns, IncludeIdle: true})
		cents(t, "shop", got["shop"].TotalCost(), 43.2+0.144)
		cents(t, "shop network", got["shop"].NetCost, 0.144) // cross-zone pair counted once
		cents(t, "shop internet", got["shop"].NetInternetCost, 0.096)
		cents(t, "shop cross-zone", got["shop"].NetCrossZoneCost, 0.048)
		cents(t, "kube-system", got["kube-system"].TotalCost(), 3.456)
		cents(t, "web", got["web"].TotalCost(), 69.12)
		cents(t, "c1 idle", got["c1/"+IdleName].IdleCost, 57.024)
		cents(t, "c2 idle", got["c2/"+IdleName].IdleCost, 17.28)
		var total float64
		for _, a := range got {
			total += a.TotalCost()
		}
		cents(t, "total", total, 190.08+0.144) // node $ + network $
		if eff := got["shop"].CPUEfficiency(); math.Abs(eff-0.5) > 1e-9 {
			t.Errorf("cpu efficiency %v", eff)
		}
		if got["shop"].Props[DimCluster] != "c1" {
			t.Errorf("props %v", got["shop"].Props)
		}
	})

	t.Run("unaligned window: raw head + rollup + raw tail", func(t *testing.T) {
		w := timewin.Window{Start: day0.Add(30 * time.Minute), End: day0.Add(24*time.Hour + 10*time.Minute), Now: now}
		if segs := planSegments(w.Start, w.QueryEnd(), now, false); len(segs) != 3 {
			t.Fatalf("expected a 3-segment plan, got %+v", segs)
		}
		got := run(t, AllocationQuery{Window: w, Dims: ns, IncludeIdle: true})
		var total float64
		for _, a := range got {
			total += a.TotalCost()
		}
		cents(t, "total", total, 23.004+34.08+28.116+8.52+0.072)
		cents(t, "shop", got["shop"].TotalCost(), 0.00025*85200+0.072)
	})

	t.Run("filter uses unfiltered denominators", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimWorkload}}, IncludeIdle: true,
			Filters: []Filter{{Dim: Dim{Name: DimNamespace}, Values: []string{"shop"}}}})
		cents(t, "api", got["api"].TotalCost(), 34.56+0.144)
		cents(t, "worker", got["worker"].TotalCost(), 8.64)
		cents(t, "filtered idle", got[IdleName].IdleCost, 57.024*43.2/46.656)
		if len(got) != 3 {
			t.Errorf("rows %v", keys(got))
		}
	})

	t.Run("shared namespaces and weighted idle", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: ns, ShareIdle: ShareIdleWeighted,
			SharedNamespaces: []string{"kube-system"}})
		cents(t, "shop shared", got["shop"].SharedCost, 3.456)
		cents(t, "shop idle", got["shop"].IdleCost, 57.024)
		cents(t, "web idle", got["web"].IdleCost, 17.28)
		if _, ok := got["kube-system"]; ok {
			t.Error("shared namespace listed")
		}
	})

	t.Run("labels via pod_metadata", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: "label:app", LabelKey: "app"}}})
		cents(t, "app=api", got["api"].TotalCost(), 34.56+0.144)
		cents(t, "app=worker", got["worker"].TotalCost(), 8.64)
		cents(t, "app=web", got["web"].TotalCost(), 69.12)
		cents(t, "unlabelled", got[Unallocated].TotalCost(), 3.456)
		f := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimCluster}},
			Filters: []Filter{{Dim: Dim{Name: "label:app", LabelKey: "app"}, Values: []string{"web"}}}})
		cents(t, "label filter", f["c2"].TotalCost(), 69.12)
	})

	t.Run("pod and node grain read pod_cost_1s", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimNode}, {Name: DimPod}}})
		cents(t, "n1/api pod", got["n1/api-7d9f8c6b5-bcdfg"].TotalCost(), 17.28+0.072)
		cents(t, "n2/worker-0", got["n2/worker-0"].Cost, 8.64)
	})

	t.Run("container split by request share", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimWorkload}, {Name: DimContainer}},
			Filters: []Filter{{Dim: Dim{Name: DimWorkload}, Values: []string{"api"}}}})
		cents(t, "app", got["api/app"].Cost, 34.56*0.8)
		cents(t, "sidecar", got["api/sidecar"].Cost, 34.56*0.2)
	})

	t.Run("controller, team and daily steps", func(t *testing.T) {
		got := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimController}}})
		cents(t, "StatefulSet/worker", got["StatefulSet/worker"].Cost, 8.64)
		teams := run(t, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimTeam}}})
		cents(t, "unallocated team", teams[Unallocated].Cost, 8.64)
		sets, err := e.Allocate(ctx, AllocationQuery{Window: aligned, Dims: ns, Step: timewin.Day})
		if err != nil || len(sets) != 2 {
			t.Fatalf("steps: %v %d", err, len(sets))
		}
		cents(t, "day 1 web", byName(sets[0])["web"].Cost, 34.56)
		cents(t, "day 2 web", byName(sets[1])["web"].Cost, 34.56)
	})

	t.Run("row cap", func(t *testing.T) {
		small := &Engine{CH: db, MaxRows: 2}
		_, err := small.Allocate(ctx, AllocationQuery{Window: aligned, Dims: []Dim{{Name: DimPod}}})
		if !errors.Is(err, ErrTooManyRows) {
			t.Fatalf("want ErrTooManyRows, got %v", err)
		}
	})
}

func keys(m map[string]*Alloc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
