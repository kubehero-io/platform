// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// gpuPriceShare mirrors packages/cost-model: on GPU nodes the
// accelerator carries most of the price.
const gpuPriceShare = 0.7

type podState struct {
	w       *workload
	p       *pod
	cores   float64
	memGiB  float64
	running bool
}

// snapshot evaluates every pod of a cluster at t.
func snapshot(c *cluster, t, start time.Time, r *rand.Rand) []podState {
	var out []podState
	for _, w := range c.Workloads {
		active := w.replicasAt(t, start)
		for i, p := range w.pods {
			st := podState{w: w, p: p, running: w.running(t) && i < active}
			if st.running {
				st.cores, st.memGiB = w.usage(t, start, r)
			}
			out = append(out, st)
		}
	}
	return out
}

func ref(c *cluster, w *workload, p *pod) *kuberov1.PodRef {
	return &kuberov1.PodRef{
		Namespace: w.Namespace, Pod: p.Name, Container: w.Name,
		Workload: w.Name, WorkloadKind: w.Kind, Node: p.Node.Name,
		Team: w.Team, Zone: p.Node.Zone, PodUid: p.UID,
	}
}

// costBatch prices every running pod and every node for one interval.
func costBatch(c *cluster, states []podState, t time.Time, interval time.Duration) ([]*kuberov1.PodCostSample, []*kuberov1.NodeCostSample) {
	type nodeAgg struct {
		reqCPU, reqMem, useCPU, useMem, podCost float64
	}
	agg := map[*node]*nodeAgg{}
	var pods []*kuberov1.PodCostSample
	sec := float32(interval.Seconds())
	for _, st := range states {
		if !st.running {
			continue
		}
		w, n := st.w, st.p.Node
		pool := n.Pool
		billCPU := math.Max(w.CPUReq, st.cores)
		billMem := math.Max(w.MemReqGiB, st.memGiB)
		cpuShare := billCPU / pool.CPU
		memShare := billMem / pool.MemGiB
		var cpuH, ramH, gpuH float64
		if pool.GPUs > 0 {
			gpuH = pool.PricePerHour * gpuPriceShare * float64(w.GPUs) / float64(pool.GPUs)
			rest := pool.PricePerHour * (1 - gpuPriceShare)
			cpuH, ramH = rest*0.5*cpuShare, rest*0.5*memShare
		} else {
			cpuH, ramH = pool.PricePerHour*0.5*cpuShare, pool.PricePerHour*0.5*memShare
		}
		// Recoverable: the requested-but-unused slice of CPU + RAM.
		idleCPU := math.Max(0, w.CPUReq-st.cores) / pool.CPU
		idleMem := math.Max(0, w.MemReqGiB-st.memGiB) / pool.MemGiB
		restPrice := pool.PricePerHour
		if pool.GPUs > 0 {
			restPrice *= 1 - gpuPriceShare
		}
		recoverH := restPrice * 0.5 * (idleCPU + idleMem)
		gpuUtil := float32(0)
		if w.GPUs > 0 {
			gpuUtil = float32(w.GPUUtil)
		}
		pods = append(pods, &kuberov1.PodCostSample{
			Cluster: c.ID, Namespace: w.Namespace, Pod: st.p.Name, TsUnixMs: t.UnixMilli(),
			Team: w.Team, CostCenter: w.CostCenter, Nodepool: pool.Name, Node: n.Name,
			Region: c.Region, Sku: pool.SKU, Lifecycle: pool.Lifecycle, GpuKind: pool.GPUKind,
			CpuMillicores: uint32(w.CPUReq * 1000), MemBytes: gib(w.MemReqGiB), GpuUtilPct: gpuUtil,
			CostUsdSec:         (cpuH + ramH + gpuH) / 3600,
			RecoverableUsdSec:  recoverH / 3600,
			CpuUsageMillicores: uint32(st.cores * 1000), MemUsageBytes: gib(st.memGiB),
			CpuCostUsdSec: cpuH / 3600, RamCostUsdSec: ramH / 3600, GpuCostUsdSec: gpuH / 3600,
			Workload: w.Name, WorkloadKind: w.Kind, Zone: n.Zone, Cloud: c.Cloud,
			GpuCount: uint32(w.GPUs), IntervalSec: sec,
		})
		a := agg[n]
		if a == nil {
			a = &nodeAgg{}
			agg[n] = a
		}
		a.reqCPU += w.CPUReq
		a.reqMem += w.MemReqGiB
		a.useCPU += st.cores
		a.useMem += st.memGiB
		a.podCost += (cpuH + ramH + gpuH) / 3600
	}
	var nodes []*kuberov1.NodeCostSample
	for _, n := range c.Nodes {
		a := agg[n]
		if a == nil {
			a = &nodeAgg{}
		}
		nodeSec := n.Pool.PricePerHour / 3600
		nodes = append(nodes, &kuberov1.NodeCostSample{
			Node: n.Name, TsUnixMs: t.UnixMilli(), Nodepool: n.Pool.Name, Region: c.Region,
			Zone: n.Zone, Sku: n.Pool.SKU, Lifecycle: n.Pool.Lifecycle, Cloud: c.Cloud,
			PricePerHour:             n.Pool.PricePerHour,
			CpuAllocatableMillicores: uint32(n.Pool.CPU * 1000), MemAllocatableBytes: gib(n.Pool.MemGiB),
			CpuRequestedMillicores: uint32(a.reqCPU * 1000), MemRequestedBytes: gib(a.reqMem),
			CpuUsedMillicores: uint32(a.useCPU * 1000), MemUsedBytes: gib(a.useMem),
			GpuCount: uint32(n.Pool.GPUs), GpuKind: n.Pool.GPUKind,
			CostUsdSec: nodeSec, IdleUsdSec: math.Max(0, nodeSec-a.podCost),
			PriceSource: "pricing-engine", IntervalSec: sec,
		})
	}
	return pods, nodes
}

// usageBatch: one container sample per running pod.
func usageBatch(c *cluster, states []podState, t time.Time) []*kuberov1.ContainerUsage {
	var out []*kuberov1.ContainerUsage
	for _, st := range states {
		if !st.running {
			continue
		}
		w := st.w
		u := &kuberov1.ContainerUsage{
			TsUnixMs: t.UnixMilli(), Source: ref(c, w, st.p),
			CpuUsageCores: st.cores, MemWorkingSetBytes: gib(st.memGiB),
			CpuRequestCores: w.CPUReq, MemRequestBytes: gib(w.MemReqGiB),
			CpuLimitCores: w.CPULimit, MemLimitBytes: gib(w.MemLimitGiB),
			Restarts: st.p.Restarts,
		}
		if w.OOMs && st.p.Restarts > 0 {
			u.LastTerminationReason = "OOMKilled"
		}
		out = append(out, u)
	}
	return out
}

// eventBatch: OOM kills on the sawtooth reset, the unschedulable backfill
// job, and node pressure during the GKE ETL window.
func eventBatch(c *cluster, states []podState, t time.Time, step time.Duration) []*kuberov1.ClusterEvent {
	var out []*kuberov1.ClusterEvent
	for _, st := range states {
		w := st.w
		if w.OOMs && st.running {
			// The sawtooth resets every 2h (offset per pod): emit the kill
			// when a reset falls inside this step.
			period := int64(7200)
			offset := int64(len(st.p.Name)*97) % period
			prev := (t.Add(-step).Unix() + offset) / period
			cur := (t.Unix() + offset) / period
			if cur > prev {
				st.p.Restarts++
				out = append(out, &kuberov1.ClusterEvent{
					TsUnixMs: t.UnixMilli(), Kind: "oom_killed", Severity: "warn",
					Source: ref(c, w, st.p), Reason: "OOMKilled",
					Message: fmt.Sprintf("Container %s exceeded its memory limit (%.2fGi) and was OOM-killed", w.Name, w.MemLimitGiB),
					Attributes: map[string]string{"restart_count": fmt.Sprint(st.p.Restarts), "limit_bytes": fmt.Sprint(gib(w.MemLimitGiB))},
					Count: 1,
				})
			}
		}
	}
	for _, w := range c.Workloads {
		if !w.Unschedulable {
			continue
		}
		out = append(out, &kuberov1.ClusterEvent{
			TsUnixMs: t.UnixMilli(), Kind: "unschedulable", Severity: "warn",
			Source: &kuberov1.PodRef{Namespace: w.Namespace, Pod: w.pods[0].Name, Workload: w.Name, WorkloadKind: w.Kind, Team: w.Team},
			Reason: "FailedScheduling",
			Message: fmt.Sprintf("0/%d nodes are available: %d Insufficient cpu, %d Insufficient memory.", len(c.Nodes), len(c.Nodes), len(c.Nodes)),
			Attributes: map[string]string{
				"cpu_millicores": fmt.Sprint(int(w.CPUReq * 1000)), "mem_bytes": fmt.Sprint(gib(w.MemReqGiB)),
				"gpu": "0", "workload": w.Name, "workload_kind": w.Kind, "pending_pods": fmt.Sprint(w.Replicas),
				"age_sec": fmt.Sprint(int(9 * time.Hour / time.Second)),
			},
			Count: int32(w.Replicas),
		})
	}
	if c.ID == "gke-euw4-batch" {
		if h := t.UTC().Hour(); h >= 1 && h < 2 && t.Minute() < int(step.Minutes())+1 {
			n := c.Nodes[0]
			out = append(out, &kuberov1.ClusterEvent{
				TsUnixMs: t.UnixMilli(), Kind: "node_pressure", Severity: "warn",
				Source: &kuberov1.PodRef{Node: n.Name, Zone: n.Zone}, Reason: "MemoryPressure",
				Message: "Node condition MemoryPressure is now: True", Count: 1,
			})
		}
	}
	return out
}

// ─── logs ────────────────────────────────────────────────────────────────

type logLine struct{ level, body string }

func hexID(r *rand.Rand, n int) string {
	const h = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = h[r.IntN(16)]
	}
	return string(b)
}

// linesPerSecond per pod; the incident multiplies checkout's.
func logRate(w *workload, incident bool) float64 {
	switch w.Name {
	case "frontend-gateway":
		return 6
	case "checkout-api":
		if incident {
			return 9
		}
		return 2
	case "payments-worker":
		return 1
	case "postgres":
		return 0.3
	case "model-server-a100":
		return 3
	default:
		return 0.5
	}
}

func logFor(w *workload, incident bool, oomPressure bool, r *rand.Rand) logLine {
	trace := hexID(r, 32)
	switch w.Name {
	case "frontend-gateway":
		paths := []string{"/api/cart", "/api/checkout", "/api/products", "/api/search", "/healthz"}
		p := paths[r.IntN(len(paths))]
		status, lvl := 200, "info"
		switch x := r.Float64(); {
		case incident && p == "/api/checkout" && x < 0.25:
			status, lvl = 504, "error"
		case x < 0.004:
			status, lvl = 502, "error"
		case x < 0.02:
			status, lvl = 499, "warn"
		}
		return logLine{lvl, fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"request","method":"GET","path":"%s","status":%d,"duration_ms":%d,"trace_id":"%s"}`,
			lvl, time.Now().UTC().Format(time.RFC3339Nano), p, status, 3+r.IntN(80), trace)}
	case "checkout-api":
		order := "ord_" + hexID(r, 8)
		if incident {
			switch x := r.Float64(); {
			case x < 0.55:
				return logLine{"warn", fmt.Sprintf(`level=warn msg="payment gateway timeout, retrying" provider=stripe attempt=%d order_id=%s timeout_ms=3000 trace_id=%s`, 1+r.IntN(4), order, trace)}
			case x < 0.72:
				return logLine{"error", fmt.Sprintf(`level=error msg="payment failed after retries" provider=stripe attempts=5 order_id=%s err="context deadline exceeded" trace_id=%s`, order, trace)}
			}
		}
		return logLine{"info", fmt.Sprintf(`level=info msg="order placed" order_id=%s amount_usd=%.2f items=%d duration_ms=%d trace_id=%s`, order, 5+r.Float64()*180, 1+r.IntN(6), 40+r.IntN(160), trace)}
	case "payments-worker":
		if oomPressure && r.Float64() < 0.5 {
			return logLine{"warn", fmt.Sprintf(`{"level":"warn","msg":"heap usage high","heap_pct":%d,"batch_size":%d}`, 88+r.IntN(11), 400+r.IntN(200))}
		}
		return logLine{"info", fmt.Sprintf(`{"level":"info","msg":"settled batch","batch_id":"b_%s","count":%d,"duration_ms":%d}`, hexID(r, 6), 50+r.IntN(400), 200+r.IntN(900))}
	case "postgres":
		if r.Float64() < 0.2 {
			return logLine{"warn", fmt.Sprintf(`LOG:  duration: %d.%03d ms  statement: SELECT * FROM payments WHERE order_id = $1`, 800+r.IntN(2400), r.IntN(1000))}
		}
		return logLine{"info", fmt.Sprintf(`LOG:  checkpoint complete: wrote %d buffers (%.1f%%); 0 WAL file(s) added`, 200+r.IntN(4000), r.Float64()*9)}
	case "model-server-a100":
		if r.Float64() < 0.05 {
			return logLine{"warn", fmt.Sprintf(`WARNING  gpu utilization %d%% over the last 5m — batch size 1, consider batching or a smaller GPU`, 15+r.IntN(15))}
		}
		return logLine{"info", fmt.Sprintf(`INFO:     10.2.%d.%d:%d - "POST /v1/predict HTTP/1.1" 200 OK`, r.IntN(255), r.IntN(255), 30000+r.IntN(30000))}
	case "retrieval-indexer":
		if r.Float64() < 0.08 {
			return logLine{"warn", `{"level":"warn","msg":"embedding request throttled","retry_after_ms":250}`}
		}
		return logLine{"info", fmt.Sprintf(`{"level":"info","msg":"indexed documents","count":%d,"shard":%d}`, 100+r.IntN(900), r.IntN(32))}
	default:
		return logLine{"info", fmt.Sprintf(`level=info msg="%s heartbeat" component=%s uptime_s=%d`, w.Name, w.Name, r.IntN(86400))}
	}
}

// logBatch emits up to maxPerPod lines per pod for the interval.
func logBatch(c *cluster, states []podState, t, start time.Time, step time.Duration, maxPerPod int, r *rand.Rand) []*kuberov1.LogEntry {
	var out []*kuberov1.LogEntry
	incident := inIncident(t, start)
	for _, st := range states {
		if !st.running {
			continue
		}
		w := st.w
		n := int(math.Round(logRate(w, incident && w.RetryStorm) * step.Seconds()))
		if n > maxPerPod {
			n = maxPerPod
		}
		oomPressure := w.OOMs && st.memGiB > w.MemLimitGiB*0.92
		for i := 0; i < n; i++ {
			ts := t.Add(-step).Add(time.Duration(r.Int64N(int64(step))))
			l := logFor(w, incident && w.RetryStorm, oomPressure, r)
			stream := "stdout"
			if l.level == "error" || l.level == "warn" && strings.HasPrefix(l.body, "LOG") {
				stream = "stderr"
			}
			out = append(out, &kuberov1.LogEntry{
				TsUnixNano: ts.UnixNano(), Source: ref(c, w, st.p), Stream: stream,
				Level: l.level, Body: l.body,
				Labels: map[string]string{"app": w.Name},
			})
		}
	}
	return out
}

// ─── profiles ────────────────────────────────────────────────────────────

type weighted struct {
	stack  string
	weight float64
}

var goServe = "runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP"

func profileStacks(w *workload, incident bool) []weighted {
	switch w.Name {
	case "checkout-api":
		base := goServe + ";github.com/acme/checkout/internal/api.(*Server).PlaceOrder"
		s := []weighted{
			{base + ";github.com/acme/checkout/internal/store.(*Orders).Insert;database/sql.(*DB).ExecContext;github.com/jackc/pgx/v5.(*Conn).Exec", 0.16},
			{base + ";encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue", 0.14},
			{base + ";github.com/acme/checkout/internal/payments.(*Client).Charge;net/http.(*Client).Do;net/http.(*Transport).roundTrip;net/http.(*persistConn).writeLoop", 0.12},
			{goServe + ";github.com/acme/checkout/internal/api.(*Server).GetCart;github.com/redis/go-redis/v9.(*Client).Get", 0.08},
			{"runtime.goexit;runtime.gcBgMarkWorker;runtime.gcDrain;runtime.scanobject", 0.12},
			{"runtime.mcall;runtime.park_m;runtime.schedule;runtime.findRunnable;runtime.netpoll", 0.06},
		}
		if incident {
			// The regression: every retry dials a fresh TLS connection.
			s = append(s,
				weighted{base + ";github.com/acme/checkout/internal/payments.(*Client).chargeWithRetry;github.com/acme/checkout/internal/payments.(*Client).Charge;net/http.(*Transport).dialConn;crypto/tls.(*Conn).HandshakeContext;crypto/tls.(*Conn).clientHandshake;crypto/tls.(*clientHandshakeStateTLS13).handshake;crypto/ecdh.(*nistCurve).ecdh", 0.34},
				weighted{base + ";github.com/acme/checkout/internal/payments.(*Client).chargeWithRetry;github.com/acme/checkout/internal/payments.(*Client).Charge;net/http.(*Transport).dialConn;crypto/tls.(*Conn).HandshakeContext;crypto/x509.(*Certificate).Verify;crypto/x509.(*Certificate).buildChains", 0.11},
			)
		}
		return s
	case "frontend-gateway":
		return []weighted{
			{goServe + ";github.com/acme/gateway/internal/proxy.(*Proxy).ServeHTTP;net/http/httputil.(*ReverseProxy).ServeHTTP;compress/gzip.(*Writer).Write;compress/flate.(*compressor).deflate", 0.34},
			{goServe + ";github.com/acme/gateway/internal/proxy.(*Proxy).ServeHTTP;net/http/httputil.(*ReverseProxy).ServeHTTP;net/http.(*Transport).RoundTrip", 0.18},
			{goServe + ";github.com/acme/gateway/internal/auth.(*Verifier).Verify;github.com/golang-jwt/jwt/v5.(*Parser).ParseWithClaims;crypto/rsa.VerifyPKCS1v15", 0.16},
			{"runtime.goexit;runtime.gcBgMarkWorker;runtime.gcDrain;runtime.scanobject", 0.14},
			{"runtime.goexit;crypto/tls.(*Conn).serverHandshake;crypto/tls.(*serverHandshakeStateTLS13).handshake", 0.10},
		}
	case "payments-worker":
		return []weighted{
			{"runtime.goexit;github.com/acme/payments/internal/worker.(*Worker).Run;github.com/acme/payments/internal/settle.Batch;encoding/json.Unmarshal;encoding/json.(*decodeState).object", 0.36},
			{"runtime.goexit;github.com/acme/payments/internal/worker.(*Worker).Run;github.com/acme/payments/internal/settle.Batch;github.com/acme/payments/internal/ledger.(*Ledger).Append", 0.22},
			{"runtime.goexit;runtime.gcBgMarkWorker;runtime.gcDrain;runtime.scanobject;runtime.greyobject", 0.30},
		}
	default:
		return nil
	}
}

// profileBatch emits one CPU profile per profiled pod for the window.
func profileBatch(c *cluster, states []podState, t, start time.Time, window time.Duration) []*kuberov1.Profile {
	var out []*kuberov1.Profile
	for _, st := range states {
		if !st.running {
			continue
		}
		stacks := profileStacks(st.w, st.w.RetryStorm && inIncident(t, start))
		if stacks == nil {
			continue
		}
		var total float64
		for _, s := range stacks {
			total += s.weight
		}
		cpuNanos := st.cores * float64(window.Nanoseconds())
		var samples []*kuberov1.StackSample
		for _, s := range stacks {
			samples = append(samples, &kuberov1.StackSample{
				Frames: strings.Split(s.stack, ";"),
				Value:  int64(cpuNanos * s.weight / total),
			})
		}
		out = append(out, &kuberov1.Profile{
			TsUnixNano: t.Add(-window).UnixNano(), DurationNano: window.Nanoseconds(),
			Type: "cpu", Unit: "nanoseconds", Service: st.w.Name, Source: ref(c, st.w, st.p),
			Samples: samples, Origin: "ebpf", Period: int64(time.Second / 49),
		})
	}
	return out
}

// ─── flows ───────────────────────────────────────────────────────────────

type edge struct {
	from, to string // workload names; to may be "ext:<host>"
	port         int32
	bytesPerSec  float64
}

var clusterEdges = map[string][]edge{
	"eks-use1-prod": {
		{"frontend-gateway", "checkout-api", 8080, 4.5e6},
		{"checkout-api", "payments-worker", 9090, 0.9e6},
		{"payments-worker", "postgres", 5432, 2.2e6},
		{"checkout-api", "postgres", 5432, 0.6e6},
		{"checkout-api", "ext:api.stripe.com", 443, 0.25e6},
		{"recommendation-svc", "ext:s3.amazonaws.com", 443, 1.1e6},
	},
	"gke-euw4-batch": {
		{"retrieval-indexer", "ext:storage.googleapis.com", 443, 1.4e6},
		{"etl-nightly", "ext:bigquery.googleapis.com", 443, 18e6},
	},
	"aks-westeu-prod-01": {
		{"frontend-gateway", "model-server-a100", 8000, 2.1e6},
		{"model-server-a100", "feature-store", 6379, 1.3e6},
		{"feature-store", "ext:kubeherodemo.blob.core.windows.net", 443, 0.6e6},
	},
}

func podIP(c *cluster, p *pod) string {
	h := 0
	for _, ch := range c.ID + p.Name {
		h = h*31 + int(ch)
	}
	if h < 0 {
		h = -h
	}
	return fmt.Sprintf("10.%d.%d.%d", 40+len(c.ID)%20, (h/251)%250+1, h%251+2)
}

func endpointFor(c *cluster, st podState) *kuberov1.FlowEndpoint {
	return &kuberov1.FlowEndpoint{Ip: podIP(c, st.p), Kind: "pod", Pod: ref(c, st.w, st.p), Zone: st.p.Node.Zone, Name: st.w.Name}
}

// flowBatch spreads each edge's traffic over the running pods on both
// ends and reports it the way the eBPF agent does: pod↔pod pairs twice
// (egress at the sender, ingress at the receiver), egress to the
// internet once.
func flowBatch(c *cluster, states []podState, t, start time.Time, window time.Duration, r *rand.Rand) []*kuberov1.Flow {
	byName := map[string][]podState{}
	for _, st := range states {
		if st.running {
			byName[st.w.Name] = append(byName[st.w.Name], st)
		}
	}
	var out []*kuberov1.Flow
	incident := inIncident(t, start)
	for _, e := range clusterEdges[c.ID] {
		srcs := byName[e.from]
		if len(srcs) == 0 {
			continue
		}
		rate := e.bytesPerSec
		retrans := 0.0
		if incident && e.from == "checkout-api" && strings.HasPrefix(e.to, "ext:") {
			rate *= 6
			retrans = 0.04
		}
		total := rate * window.Seconds() * (0.85 + 0.3*r.Float64())
		if strings.HasPrefix(e.to, "ext:") {
			host := strings.TrimPrefix(e.to, "ext:")
			for _, s := range srcs {
				b := uint64(total / float64(len(srcs)))
				out = append(out, &kuberov1.Flow{
					TsUnixMs: t.UnixMilli(), WindowSec: int32(window.Seconds()),
					Src: endpointFor(c, s),
					Dst: &kuberov1.FlowEndpoint{Ip: "203.0.113." + fmt.Sprint(10+len(host)%200), Kind: "external", Name: host},
					Port: e.port, Protocol: "tcp", Bytes: b, Packets: b / 1200,
					Direction: "egress", Retransmits: uint32(float64(b/1200) * retrans),
				})
			}
			continue
		}
		dsts := byName[e.to]
		if len(dsts) == 0 {
			continue
		}
		per := total / float64(len(srcs)*len(dsts))
		for _, s := range srcs {
			for _, d := range dsts {
				b := uint64(per)
				for _, dir := range []string{"egress", "ingress"} {
					out = append(out, &kuberov1.Flow{
						TsUnixMs: t.UnixMilli(), WindowSec: int32(window.Seconds()),
						Src: endpointFor(c, s), Dst: endpointFor(c, d),
						Port: e.port, Protocol: "tcp", Bytes: b, Packets: b / 1400, Direction: dir,
					})
				}
			}
		}
	}
	return out
}
