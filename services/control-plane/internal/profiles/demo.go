// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package profiles

import (
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// Demo profiles, served only without ClickHouse. The CPU profile is a
// believable Go HTTP service: request handling fans out to JSON
// encoding, a Postgres query through pgx, and gzip; the runtime spends
// its share in GC and the scheduler. The "current" window carries a
// regression — json.Marshal on a reflect-heavy path grew ~3× — which
// the diff flamegraph lights up against the baseline.

const (
	httpPrefix = "runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;net/http.(*ServeMux).ServeHTTP"
	checkout   = httpPrefix + ";main.(*api).handleCheckout"
	cart       = httpPrefix + ";main.(*api).handleCart"
	jsonPath   = ";encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue"
	pgxPath    = ";main.(*store).LoadOrder;github.com/jackc/pgx/v5.(*Conn).Query;github.com/jackc/pgx/v5/pgconn.(*PgConn).ReceiveMessage"
	gzipPath   = ";compress/gzip.(*Writer).Write;compress/flate.(*compressor).write;compress/flate.(*compressor).deflate"
)

// demoStacks: folded stacks (root → leaf) with CPU nanoseconds per
// minute of wall time; regression toggles the json.Marshal growth.
func demoStacks(regression bool) []Stack {
	marshal := int64(9e9)
	if regression {
		marshal = 27e9
	}
	folded := []struct {
		stack string
		v     int64
	}{
		{checkout + jsonPath + ";encoding/json.structEncoder.encode;encoding/json.stringEncoder", marshal},
		{checkout + jsonPath + ";encoding/json.mapEncoder.encode;sort.Slice", marshal / 3},
		{checkout + jsonPath + ";runtime.mallocgc", marshal / 4},
		{checkout + pgxPath + ";internal/poll.(*FD).Read;syscall.Syscall", 7e9},
		{checkout + pgxPath + ";github.com/jackc/pgx/v5/pgproto3.(*Frontend).Receive", 3e9},
		{checkout + gzipPath, 11e9},
		{checkout + ";net/http.(*response).Write;bufio.(*Writer).Flush;internal/poll.(*FD).Write;syscall.Syscall", 4e9},
		{cart + ";github.com/redis/go-redis/v9.(*Client).Get;github.com/redis/go-redis/v9/internal/pool.(*Conn).WithReader", 5e9},
		{cart + jsonPath, 2e9},
		{"runtime.goexit;runtime.gcBgMarkWorker;runtime.gcDrain;runtime.scanobject", 8e9},
		{"runtime.goexit;runtime.gcBgMarkWorker;runtime.gcDrain;runtime.markroot", 2e9},
		{"runtime.mcall;runtime.park_m;runtime.schedule;runtime.findRunnable;runtime.netpoll", 3e9},
		{"runtime.goexit;main.main.func2;time.(*Ticker).C;main.(*metrics).flush", 1e9},
	}
	out := make([]Stack, 0, len(folded))
	for _, f := range folded {
		out = append(out, Stack{Frames: strings.Split(f.stack, ";"), Value: f.v})
	}
	return out
}

// demoCPUCostMonth: what checkout-api's CPU costs per month in the demo.
const demoCPUCostMonth = 1840.0

// demoProfile is the current window (with the regression) or the
// baseline, priced like the live path.
func demoProfile(typ string, regression bool) (*profileData, float64) {
	d := &profileData{stacks: demoStacks(regression), unit: "nanoseconds", samples: 18000}
	for _, s := range d.stacks {
		d.total += s.Value
	}
	if typ != "cpu" {
		d.unit = "bytes"
		return d, 0
	}
	return d, demoCPUCostMonth
}

func demoFlamegraph(typ string, diff bool, maxNodes int) *kuberov1.GetFlamegraphResponse {
	d, cost := demoProfile(typ, true)
	tree := NewTree()
	for _, s := range d.stacks {
		tree.Add(s.Frames, s.Value)
	}
	if diff {
		for _, s := range demoStacks(false) {
			tree.AddBaseline(s.Frames, s.Value)
		}
	}
	return flameResponse(tree, maxNodes, d.unit, typ, d.samples, cost)
}

func demoTargets(namespace string, end time.Time) []*kuberov1.ProfileTarget {
	all := []*kuberov1.ProfileTarget{
		{Service: "checkout-api", Namespace: "checkout", Workload: "checkout", Types: []string{"alloc_space", "cpu", "goroutines", "inuse_space"},
			Origin: "ebpf", CpuCoresAvg: 2.84, CostUsdMonth: 2380},
		{Service: "frontend-gateway", Namespace: "edge", Workload: "frontend-gateway", Types: []string{"cpu"},
			Origin: "ebpf", CpuCoresAvg: 11.6, CostUsdMonth: 10224},
		{Service: "ledger", Namespace: "payments", Workload: "ledger", Types: []string{"alloc_space", "cpu", "mutex"},
			Origin: "pprof-scrape", CpuCoresAvg: 2.1, CostUsdMonth: 1872},
		{Service: "vectordb-ingress", Namespace: "retrieval", Workload: "vectordb-ingress", Types: []string{"cpu"},
			Origin: "ebpf", CpuCoresAvg: 2.46, CostUsdMonth: 13248},
		{Service: "recommender", Namespace: "ml-inference", Workload: "embedder", Types: []string{"cpu", "wall"},
			Origin: "pyroscope", CpuCoresAvg: 12.4, CostUsdMonth: 29520},
	}
	var out []*kuberov1.ProfileTarget
	for _, t := range all {
		if namespace != "" && t.Namespace != namespace {
			continue
		}
		t.LastSeenUnixMs = end.Add(-30 * time.Second).UnixMilli()
		out = append(out, t)
	}
	return out
}
