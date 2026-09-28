// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rightsizing

import (
	"math"
	"strings"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// A week of 5-minute buckets = 2016.
const week = 7 * 24 * 12

func baseUsage() Usage {
	return Usage{
		Cluster: "c1", Namespace: "shop", Workload: "api", WorkloadKind: "Deployment", Container: "app",
		CPU:        Quantiles{P50: 0.20, P90: 0.28, P95: 0.31, P99: 0.40, Max: 0.55},
		Mem:        Quantiles{P50: 300 * MiB, P90: 350 * MiB, P95: 380 * MiB, P99: 400 * MiB, Max: 412 * MiB},
		CPURequest: 2, CPULimit: 4, MemRequest: 1 * GiB, MemLimit: 2 * GiB,
		Samples: week * 20, Buckets: week,
	}
}

var price = Price{CPUCoreHour: 0.04, RAMGiBHour: 0.005, Replicas: 3, Cloud: "aws"}

func TestRecommendRules(t *testing.T) {
	cases := []struct {
		name     string
		mut      func(*Usage)
		oom      int
		opts     Options
		cpu, mem float64
		dir      string
		conf     string
	}{
		{
			// 0.31 × 1.15 = 0.3565 → ceil 5m → 0.360; mem 412Mi × 1.15 = 473.8 → 474Mi.
			name: "downsize with defaults", cpu: 0.360, mem: 474 * MiB, dir: Downsize, conf: "high",
		},
		{
			name: "p99 + 30% headroom", opts: Options{CPUPercentile: "p99", HeadroomPct: 30},
			cpu: 0.520, mem: math.Ceil(412*1.3) * MiB, dir: Downsize, conf: "high",
		},
		{
			name: "max percentile", opts: Options{CPUPercentile: "max"},
			cpu: 0.635, mem: 474 * MiB, dir: Downsize, conf: "high",
		},
		{
			name: "floors: idle container gets 10m / 32Mi",
			mut: func(u *Usage) {
				u.CPU = Quantiles{P95: 0.001, Max: 0.002}
				u.Mem = Quantiles{P99: 4 * MiB, Max: 5 * MiB}
			},
			cpu: 0.010, mem: 32 * MiB, dir: Downsize, conf: "high",
		},
		{
			name: "memory never below observed max even when p99 is lower",
			mut:  func(u *Usage) { u.Mem = Quantiles{P50: 100 * MiB, P99: 200 * MiB, Max: 900 * MiB} },
			cpu:  0.360, mem: math.Ceil(900*1.15) * MiB, dir: Downsize, conf: "high",
		},
		{
			// OOM: max(474Mi, request 1Gi, 1.25 × max(412Mi, limit 2Gi) = 2.5Gi) = 2.5Gi.
			name: "OOM kill forces an upsize", oom: 2,
			cpu: 0.360, mem: 2.5 * GiB, dir: Upsize, conf: "high",
		},
		{
			name: "OOM without a limit: 1.25 × observed max, never below request", oom: 1,
			mut: func(u *Usage) { u.MemLimit = 0; u.MemRequest = 256 * MiB },
			cpu: 0.360, mem: math.Ceil(412*1.25) * MiB, dir: Upsize, conf: "high",
		},
		{
			name: "under-provisioned cpu", mut: func(u *Usage) { u.CPURequest = 0.1 },
			cpu: 0.360, mem: 474 * MiB, dir: Upsize, conf: "high",
		},
		{
			name: "no requests set is an upsize", mut: func(u *Usage) { u.CPURequest, u.MemRequest = 0, 0 },
			cpu: 0.360, mem: 474 * MiB, dir: Upsize, conf: "high",
		},
		{
			name: "within ±10% is ok", mut: func(u *Usage) { u.CPURequest = 0.35; u.MemRequest = 480 * MiB },
			cpu: 0.360, mem: 474 * MiB, dir: OK, conf: "high",
		},
		{name: "12h of history is low confidence", mut: func(u *Usage) { u.Buckets = 144 }, cpu: 0.360, mem: 474 * MiB, dir: Downsize, conf: "low"},
		{name: "3 days is medium", mut: func(u *Usage) { u.Buckets = 3 * 288 }, cpu: 0.360, mem: 474 * MiB, dir: Downsize, conf: "medium"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := baseUsage()
			if c.mut != nil {
				c.mut(&u)
			}
			r := Recommend(u, c.oom, price, c.opts)
			if !approx(r.CPURecommended, c.cpu) {
				t.Errorf("cpu = %v, want %v", r.CPURecommended, c.cpu)
			}
			if !approx(r.MemRecommended, c.mem) {
				t.Errorf("mem = %v MiB, want %v MiB", r.MemRecommended/MiB, c.mem/MiB)
			}
			if r.Direction != c.dir {
				t.Errorf("direction = %s, want %s", r.Direction, c.dir)
			}
			if r.Confidence != c.conf {
				t.Errorf("confidence = %s, want %s", r.Confidence, c.conf)
			}
			if r.MemRecommended < u.Mem.Max {
				t.Error("memory recommendation below observed max")
			}
			if c.oom > 0 && r.MemRecommended < u.MemRequest {
				t.Error("memory lowered after an OOM kill")
			}
			if math.Mod(math.Round(r.CPURecommended*1e6), 5000) != 0 {
				t.Errorf("cpu %v not on the 5m grid", r.CPURecommended)
			}
			if math.Mod(r.MemRecommended, MiB) != 0 {
				t.Errorf("mem %v not on the 1Mi grid", r.MemRecommended)
			}
			if r.Reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestRecommendCostImpact(t *testing.T) {
	r := Recommend(baseUsage(), 0, price, Options{})
	// current: 2 cores × $0.04 × 3 replicas × 720h = 172.80
	//          1 GiB × $0.005 × 3 × 720        = 10.80
	// recommended: 0.36 × 0.04 × 3 × 720 = 31.104; 474Mi → 0.462890625 GiB × 0.005 × 3 × 720 = 4.99921875
	if !approx(r.CurrentCostMonth, 183.6) {
		t.Fatalf("current = %v", r.CurrentCostMonth)
	}
	wantRec := 31.104 + 474.0/1024*0.005*3*720
	if !approx(r.RecommendedCost, wantRec) {
		t.Fatalf("recommended = %v, want %v", r.RecommendedCost, wantRec)
	}
	if !approx(r.SavingsMonth, 183.6-wantRec) || !approx(r.CPUSavingsMonth+r.MemSavingsMonth, r.SavingsMonth) {
		t.Fatalf("savings = %v (cpu %v mem %v)", r.SavingsMonth, r.CPUSavingsMonth, r.MemSavingsMonth)
	}
	if r.ID != "c1/shop/api/app" || r.Cloud != "aws" {
		t.Fatalf("identity: %q %q", r.ID, r.Cloud)
	}

	// Usage above the request bills the usage: savings from "raising"
	// a request below the median are not negative dollars that don't exist.
	u := baseUsage()
	u.CPURequest = 0.05
	up := Recommend(u, 0, price, Options{})
	if up.Direction != Upsize {
		t.Fatal("want upsize")
	}
	curCPU := 0.20 * 0.04 * 3 * 720 // billed at p50, not at the 0.05 request
	if !approx(up.CPUSavingsMonth, curCPU-0.36*0.04*3*720) {
		t.Fatalf("cpu savings for under-requested container = %v", up.CPUSavingsMonth)
	}
}

func TestRecommendReasonMentionsRisks(t *testing.T) {
	u := baseUsage()
	u.ThrottleRisk = 0.12
	u.Buckets = 100
	r := Recommend(u, 3, price, Options{})
	for _, want := range []string{"3 OOM kill", "throttling risk", "low confidence", "CPU P95"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason missing %q: %s", want, r.Reason)
		}
	}
}

func TestValidPercentile(t *testing.T) {
	for in, want := range map[string]string{"": "p95", "P90": "p90", "p99": "p99", " max ": "max"} {
		if got, err := ValidPercentile(in); err != nil || got != want {
			t.Errorf("ValidPercentile(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ValidPercentile("p50"); err == nil {
		t.Error("p50 is not a sizing percentile")
	}
}

func TestFilterSortsAndTotals(t *testing.T) {
	recs := []Recommendation{
		{ID: "a", SavingsMonth: 10, Direction: Downsize},
		{ID: "b", SavingsMonth: 100, Direction: Downsize},
		{ID: "c", SavingsMonth: -20, Direction: Upsize},
		{ID: "d", SavingsMonth: 0, Direction: OK},
	}
	out, total := Filter(recs, 0, 0)
	if len(out) != 3 || out[0].ID != "b" || out[2].ID != "c" || total != 110 {
		t.Fatalf("Filter = %+v total %v", out, total)
	}
	out, total = Filter(recs, 50, 0)
	if len(out) != 1 || out[0].ID != "b" || total != 100 {
		t.Fatalf("min savings: %+v %v", out, total)
	}
	out, total = Filter(recs, 0, 1)
	if len(out) != 1 || total != 110 {
		t.Fatalf("limit keeps the pre-limit total: %+v %v", out, total)
	}
}

func TestQuantilesClampToMax(t *testing.T) {
	q := quantiles([]float32{1, 2, 3, 9}, 5)
	if q.P99 != 5 || q.P50 != 1 {
		t.Fatalf("%+v", q)
	}
	if z := quantiles(nil, 0); z.P95 != 0 {
		t.Fatal("missing digest must read as zero")
	}
}

func TestDemoRunsThroughTheRealRules(t *testing.T) {
	recs := Demo(Options{})
	if len(recs) < 6 {
		t.Fatalf("demo recs = %d", len(recs))
	}
	var sawOOM, sawUp, sawDown bool
	for _, r := range recs {
		if r.MemRecommended < r.MemMax {
			t.Errorf("%s: memory below observed max", r.ID)
		}
		if r.OOMKills > 0 {
			sawOOM = true
			if r.MemRecommended < r.MemRequest {
				t.Errorf("%s: memory lowered after OOM", r.ID)
			}
		}
		switch r.Direction {
		case Upsize:
			sawUp = true
		case Downsize:
			sawDown = true
		}
	}
	if !sawOOM || !sawUp || !sawDown {
		t.Fatalf("demo should show OOM, upsize and downsize cases (%v %v %v)", sawOOM, sawUp, sawDown)
	}
}
