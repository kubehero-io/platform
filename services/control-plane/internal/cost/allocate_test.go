// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"math"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

var t0 = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

func oneBucket() []bucketWindow { return []bucketWindow{{Start: t0, End: t0.Add(24 * time.Hour)}} }

func row(cluster, ns, wl string, cost float64, vals ...string) fineRow {
	r := fineRow{Cluster: cluster, Namespace: ns, Workload: wl, Vals: vals}
	r.M.Cost, r.M.CPUCost, r.M.RAMCost = cost, cost/2, cost/2
	r.M.CPUReqCS, r.M.CPUUseCS = 100, 50
	r.M.RAMReqBS, r.M.RAMUseBS = 1000, 250
	return r
}

func byName(set AllocSet) map[string]*Alloc {
	m := map[string]*Alloc{}
	for _, a := range set.Rows {
		m[a.Name] = a
	}
	return m
}

func TestPlanSegments(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)
	h := func(hh, mm int) time.Time { return time.Date(2026, 9, 30, hh, mm, 0, 0, time.UTC) }
	cases := []struct {
		name       string
		start, end time.Time
		forceRaw   bool
		want       []segment
	}{
		{"short window is raw", h(13, 0), now, false, []segment{{srcRaw, h(13, 0), now}}},
		{"forced raw", h(0, 0), now, true, []segment{{srcRaw, h(0, 0), now}}},
		{"aligned start, end=now uses the partial rollup hour", h(0, 0), now, false,
			[]segment{{srcRollup, h(0, 0), h(15, 0)}}},
		{"rolling 24h: raw head then rollup", now.Add(-24 * time.Hour), now, false,
			[]segment{{srcRaw, now.Add(-24 * time.Hour), h(15, 0).Add(-24 * time.Hour)}, {srcRollup, h(15, 0).Add(-24 * time.Hour), h(15, 0)}}},
		{"past unaligned end: raw tail", h(2, 0), h(9, 40), false,
			[]segment{{srcRollup, h(2, 0), h(9, 0)}, {srcRaw, h(9, 0), h(9, 40)}}},
		{"past aligned end", h(2, 0), h(9, 0), false, []segment{{srcRollup, h(2, 0), h(9, 0)}}},
		{"empty", now, now, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planSegments(c.start, c.end, now, c.forceRaw)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i].src != c.want[i].src || !got[i].from.Equal(c.want[i].from) || !got[i].to.Equal(c.want[i].to) {
					t.Fatalf("segment %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
			// Segments must tile [start, end) with no gap or overlap.
			if len(got) > 0 && !got[0].from.Equal(c.start) {
				t.Fatalf("first segment starts at %s", got[0].from)
			}
			for i := 1; i < len(got); i++ {
				if !got[i].from.Equal(got[i-1].to) {
					t.Fatalf("gap/overlap between %d and %d", i-1, i)
				}
			}
		})
	}
}

func TestBuckets(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)
	w, _ := timewin.Parse("7d", now)
	q := AllocationQuery{Window: w, Step: timewin.Day}
	bws, bk, err := q.buckets()
	if err != nil {
		t.Fatal(err)
	}
	if len(bws) != 7 || bk.stepSec != 86400 || bk.startSec != w.Start.Unix() {
		t.Fatalf("buckets = %d, %+v", len(bws), bk)
	}
	if !bws[6].End.Equal(w.End) || !bws[0].Start.Equal(w.Start) {
		t.Fatalf("edges: %+v … %+v", bws[0], bws[6])
	}
	q.Step = time.Hour
	w30, _ := timewin.Parse("60d", now)
	q.Window = w30
	if _, _, err := q.buckets(); err == nil {
		t.Fatal("want too-many-steps error")
	}
	q.Step = 0
	one, bk0, _ := q.buckets()
	if len(one) != 1 || bk0.stepSec != 0 {
		t.Fatal("no step = one bucket")
	}
	if e, a := bk0.expr("x"); e != "toInt64(0)" || a != nil {
		t.Fatalf("single-bucket expr = %q %v", e, a)
	}
}

// Two clusters, idle kept separate: idle = node $ − allocated $, one
// row per cluster, totals = node $.
func TestAggregateSeparateIdle(t *testing.T) {
	dims := []Dim{{Name: DimNamespace}}
	rows := []fineRow{
		row("c1", "prod", "api", 60, "prod"),
		row("c1", "dev", "web", 20, "dev"),
		row("c2", "prod", "api", 30, "prod"),
	}
	sets := aggregate(aggInput{
		Dims: dims, Rows: rows, Buckets: oneBucket(), IncludeIdle: true,
		NodeCost: map[clusterKey]float64{{0, "c1"}: 100, {0, "c2"}: 30},
	})
	s := sets[0]
	got := byName(s)
	if len(got) != 3 {
		t.Fatalf("rows: %v", got)
	}
	if !near(got["prod"].TotalCost(), 90) || !near(got["dev"].TotalCost(), 20) {
		t.Fatalf("prod %v dev %v", got["prod"].TotalCost(), got["dev"].TotalCost())
	}
	idle := got["c1/"+IdleName]
	if idle == nil || !idle.Idle || !near(idle.IdleCost, 20) {
		t.Fatalf("c1 idle row: %+v", idle)
	}
	if _, ok := got["c2/"+IdleName]; ok {
		t.Fatal("fully allocated cluster must have no idle row")
	}
	if !near(s.Total.TotalCost(), 130) {
		t.Fatalf("total = %v, want node cost 130", s.Total.TotalCost())
	}
	// prod spans two clusters → cluster property must be dropped.
	if _, ok := got["prod"].Props[DimCluster]; ok {
		t.Fatal("cluster prop must be omitted when ambiguous")
	}
	if got["dev"].Props[DimCluster] != "c1" || got["dev"].Props[DimNamespace] != "dev" {
		t.Fatalf("dev props %v", got["dev"].Props)
	}
	if s.Rows[0].Name != "prod" {
		t.Fatalf("rows must sort by total desc, first = %s", s.Rows[0].Name)
	}
}

func TestAggregateWeightedAndEvenIdle(t *testing.T) {
	dims := []Dim{{Name: DimWorkload}}
	rows := []fineRow{row("c1", "a", "big", 75, "big"), row("c1", "a", "small", 25, "small")}
	node := map[clusterKey]float64{{0, "c1"}: 140}

	w := byName(aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(), ShareIdle: ShareIdleWeighted, NodeCost: node})[0])
	if !near(w["big"].IdleCost, 30) || !near(w["small"].IdleCost, 10) {
		t.Fatalf("weighted: big %v small %v", w["big"].IdleCost, w["small"].IdleCost)
	}
	if _, ok := w[IdleName]; ok {
		t.Fatal("shared idle must not also appear as a row")
	}

	e := byName(aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(), ShareIdle: ShareIdleEven, NodeCost: node})[0])
	if !near(e["big"].IdleCost, 20) || !near(e["small"].IdleCost, 20) {
		t.Fatalf("even: big %v small %v", e["big"].IdleCost, e["small"].IdleCost)
	}
	// Allocation can exceed node cost (usage bursts above requests):
	// idle is clamped at zero, never negative.
	neg := aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(), IncludeIdle: true,
		NodeCost: map[clusterKey]float64{{0, "c1"}: 80}})[0]
	if len(neg.Rows) != 2 || !near(neg.Total.TotalCost(), 100) {
		t.Fatalf("over-allocated cluster: %d rows, total %v", len(neg.Rows), neg.Total.TotalCost())
	}
}

func TestAggregateSharedNamespaces(t *testing.T) {
	dims := []Dim{{Name: DimNamespace}}
	rows := []fineRow{
		row("c1", "prod", "api", 60, "prod"),
		row("c1", "dev", "web", 20, "dev"),
		row("c1", "kube-system", "coredns", 8, "kube-system"),
		row("c2", "kube-system", "coredns", 5, "kube-system"), // nobody to share with in c2
	}
	s := aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(),
		Shared: map[string]bool{"kube-system": true}})[0]
	got := byName(s)
	if _, ok := got["kube-system"]; ok {
		t.Fatal("shared namespace must not appear as its own row")
	}
	if !near(got["prod"].SharedCost, 6) || !near(got["dev"].SharedCost, 2) {
		t.Fatalf("shared: prod %v dev %v", got["prod"].SharedCost, got["dev"].SharedCost)
	}
	orphan := got["c2/"+SharedName]
	if orphan == nil || !near(orphan.SharedCost, 5) {
		t.Fatalf("orphan shared cost row: %+v", orphan)
	}
	if !near(s.Total.TotalCost(), 93) {
		t.Fatalf("total %v, want every dollar (93)", s.Total.TotalCost())
	}
}

// With filters the unfiltered denominators come from Totals: a filtered
// row gets its proportional share of idle and shared cost, and the
// separate idle row shows only the filtered share.
func TestAggregateFilteredUsesUnfilteredDenominators(t *testing.T) {
	dims := []Dim{{Name: DimNamespace}}
	rows := []fineRow{row("c1", "prod", "api", 40, "prod")} // prod is 40 of 80 non-shared
	totals := map[clusterKey]*clusterTotals{{0, "c1"}: {NonShared: 80, Shared: 20}}
	node := map[clusterKey]float64{{0, "c1"}: 160} // idle = 60

	s := aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(), Totals: totals, NodeCost: node,
		IncludeIdle: true, Shared: map[string]bool{"kube-system": true}})[0]
	got := byName(s)
	if !near(got["prod"].SharedCost, 10) {
		t.Fatalf("shared share = %v, want 20×40/80 = 10", got["prod"].SharedCost)
	}
	if idle := got[IdleName]; idle == nil || !near(idle.IdleCost, 30) {
		t.Fatalf("filtered idle row = %+v, want 60×40/80 = 30", idle)
	}

	w := byName(aggregate(aggInput{Dims: dims, Rows: rows, Buckets: oneBucket(), Totals: totals, NodeCost: node,
		ShareIdle: ShareIdleWeighted})[0])
	if !near(w["prod"].IdleCost, 30) || len(w) != 1 {
		t.Fatalf("weighted filtered idle = %v (%d rows)", w["prod"].IdleCost, len(w))
	}
}

func TestAggregateBucketsAndSpan(t *testing.T) {
	b := []bucketWindow{{Start: t0, End: t0.Add(24 * time.Hour)}, {Start: t0.Add(24 * time.Hour), End: t0.Add(30 * time.Hour)}}
	r1 := row("c1", "a", "x", 10, "a")
	r1.Start, r1.End = t0.Add(time.Hour).Unix(), t0.Add(3*time.Hour).Unix()
	r2 := row("c1", "a", "x", 5, "a")
	r2.Bucket = 1
	r2.Start, r2.End = t0.Add(20*time.Hour).Unix(), t0.Add(40*time.Hour).Unix() // clamped
	sets := aggregate(aggInput{Dims: []Dim{{Name: DimNamespace}}, Rows: []fineRow{r1, r2}, Buckets: b})
	if len(sets) != 2 || len(sets[0].Rows) != 1 || len(sets[1].Rows) != 1 {
		t.Fatalf("sets: %+v", sets)
	}
	if got := sets[0].Rows[0].Minutes(); got != 120 {
		t.Fatalf("minutes = %v", got)
	}
	a := sets[1].Rows[0]
	if !a.Start.Equal(b[1].Start) || !a.End.Equal(b[1].End) {
		t.Fatalf("span not clamped to its step: %s – %s", a.Start, a.End)
	}
}

func TestMetricsEfficiencyAndSplit(t *testing.T) {
	m := metrics{Cost: 10, CPUCost: 6, RAMCost: 4, CPUReqCS: 100, CPUUseCS: 25, RAMReqBS: 100, RAMUseBS: 50}
	if !near(m.CPUEfficiency(), 0.25) || !near(m.RAMEfficiency(), 0.5) {
		t.Fatal("efficiency")
	}
	// (0.25·6 + 0.5·4) / 10 = 0.35
	if !near(m.TotalEfficiency(), 0.35) {
		t.Fatalf("total efficiency %v", m.TotalEfficiency())
	}
	if efficiency(5, 0) != 1 || efficiency(0, 0) != 0 {
		t.Fatal("OpenCost edge cases")
	}
	unsplit := metrics{Cost: 10}
	unsplit.normalizeSplit()
	if !near(unsplit.CPUCost, 5) || !near(unsplit.RAMCost, 5) {
		t.Fatalf("unsplit remainder: %+v", unsplit)
	}
	over := metrics{Cost: 10, CPUCost: 8, RAMCost: 4}
	over.normalizeSplit()
	if !near(over.CPUCost+over.RAMCost, 10) || !near(over.CPUCost/over.RAMCost, 2) {
		t.Fatalf("over-split: %+v", over)
	}
}

func TestSplitContainers(t *testing.T) {
	r := row("c1", "prod", "api", 10, "")
	r.M.CPUCost, r.M.RAMCost = 6, 4
	r.M.NetCost = 2
	shares := map[wlID][]containerShare{{"c1", "prod", "api"}: {
		{Name: "app", CPU: 0.75, RAM: 0.5},
		{Name: "sidecar", CPU: 0.25, RAM: 0.5},
	}}
	out := splitContainers([]fineRow{r, row("c1", "prod", "unknown", 3, "")}, shares, 0, nil)
	if len(out) != 3 {
		t.Fatalf("rows = %d", len(out))
	}
	var sum metrics
	for _, o := range out {
		sum.add(&o.M)
	}
	if !near(sum.Cost, 13) || !near(sum.NetCost, 2) || !near(sum.CPUReqCS, 200) {
		t.Fatalf("split must conserve totals: %+v", sum)
	}
	app := out[0]
	if app.Vals[0] != "app" || !near(app.M.CPUCost, 4.5) || !near(app.M.RAMCost, 2) || !near(app.M.Cost, 6.5) {
		t.Fatalf("app share: %+v %v", app.M, app.Vals)
	}
	if out[2].Vals[0] != "" {
		t.Fatal("workload without container data stays unallocated")
	}
	kept := splitContainers([]fineRow{r}, shares, 0, map[string]bool{"sidecar": true})
	if len(kept) != 1 || kept[0].Vals[0] != "sidecar" {
		t.Fatalf("container filter: %+v", kept)
	}
}

func TestAttributeNetworkAndLogs(t *testing.T) {
	rows := []fineRow{row("c1", "prod", "api", 30, "general"), row("c1", "prod", "api", 10, "spot")}
	net := map[wlKey]*metrics{
		{0, "c1", "prod", "api"}:     {NetCost: 4, NetInternetCost: 4},
		{0, "c1", "prod", "orphans"}: {NetCost: 1, NetCrossZoneCost: 1},
	}
	logs := map[wlKey]float64{{0, "c1", "prod", "api"}: 1e9}
	p := &plan{q: AllocationQuery{Dims: []Dim{{Name: DimNodepool}}}, shared: map[string]bool{}, netSynthOK: true}
	out := attributeNetworkAndLogs(rows, net, logs, p)
	if len(out) != 3 {
		t.Fatalf("want synthetic row for network-only workload, got %d rows", len(out))
	}
	if !near(out[0].M.NetCost, 3) || !near(out[1].M.NetCost, 1) || !near(out[0].M.LogBytes, 0.75e9) {
		t.Fatalf("cost-weighted split: %+v / %+v", out[0].M, out[1].M)
	}
	if out[2].Workload != "orphans" || !near(out[2].M.NetCrossZoneCost, 1) || out[2].Vals[0] != "" {
		t.Fatalf("synthetic row: %+v", out[2])
	}
	p.netSynthOK = false
	if got := attributeNetworkAndLogs(rows[:2:2], net, nil, p); len(got) != 2 {
		t.Fatal("no synthetic rows when a filter could have excluded them")
	}
}

func TestNewPlanClusterIntersection(t *testing.T) {
	snap := clusters.NewSnapshot([]clusters.Info{{ID: "uuid-1", Slug: "prod"}})
	p := newPlan(AllocationQuery{ClusterID: "prod", Filters: []Filter{{Dim: Dim{Name: DimCluster}, Values: []string{"uuid-1", "other"}}}}, snap)
	// Both aliases of the one cluster in common survive; "other" doesn't.
	if len(p.clusterIDs) != 2 || p.clusterIDs[0] != "uuid-1" || p.clusterIDs[1] != "prod" {
		t.Fatalf("intersection = %v", p.clusterIDs)
	}
	if p.filtered {
		t.Fatal("cluster filters don't make the query 'filtered'")
	}
	none := newPlan(AllocationQuery{ClusterID: "a", Filters: []Filter{{Dim: Dim{Name: DimCluster}, Values: []string{"b"}}}}, snap)
	if none.clusterIDs == nil || len(none.clusterIDs) != 0 {
		t.Fatalf("disjoint cluster filters must match nothing, got %v", none.clusterIDs)
	}
	lab := newPlan(AllocationQuery{Dims: []Dim{{Name: "label:app", LabelKey: "app"}},
		Filters: []Filter{{Dim: Dim{Name: DimPod}, Values: []string{"x"}}}}, snap)
	if !lab.forceRaw || !lab.filtered || len(lab.labelKeys) != 1 || lab.netSynthOK {
		t.Fatalf("plan flags: %+v", lab)
	}
}

func TestParseAggregateAndFilters(t *testing.T) {
	d, err := ParseAggregate([]string{"namespace,label:app.kubernetes.io/name", "controller", "namespace"})
	if err != nil || len(d) != 3 || d[1].LabelKey != "app.kubernetes.io/name" {
		t.Fatalf("ParseAggregate: %v %+v", err, d)
	}
	if d, _ := ParseAggregate(nil); len(d) != 1 || d[0].Name != DimNamespace {
		t.Fatal("default aggregate is namespace")
	}
	for _, bad := range [][]string{{"service"}, {"label:"}, {"label:bad key"}, {"a,b,c,d,e,f"},
		{"label:a,label:b,label:c,label:d"}} {
		if _, err := ParseAggregate(bad); err == nil {
			t.Errorf("ParseAggregate(%v): want error", bad)
		}
	}
	f, err := ParseFilters(map[string][]string{"namespace": {"prod", Unallocated, " "}, "team": {""}})
	if err != nil || len(f) != 1 || len(f[0].Values) != 2 || f[0].Values[1] != "" {
		t.Fatalf("ParseFilters: %v %+v", err, f)
	}
	if _, err := ParseFilters(map[string][]string{"nope": {"x"}}); err == nil {
		t.Fatal("unknown filter dim")
	}
	if _, err := ParseShareIdle("sometimes"); err == nil {
		t.Fatal("bad share_idle")
	}
	if v, _ := ParseShareIdle("true"); v != ShareIdleWeighted {
		t.Fatal("OpenCost boolean shareIdle=true means weighted")
	}
}
