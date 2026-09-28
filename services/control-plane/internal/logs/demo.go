// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"time"
)

// Demo data: five services on a "demo" cluster emitting realistic
// plain-text, logfmt, JSON and access-log lines, with a checkout error
// spike during minutes 40–49 of every hour. Every line carries
// source="demo" so it can never pass for real data. Lines are a pure
// function of their second (seeded per service and second), so any
// window renders identically on every call and replica. Only the last
// DemoWindow is populated, which bounds the work per request.

// DemoWindow is how far back demo data reaches.
const DemoWindow = 3 * time.Hour

// DemoCluster is the cluster id demo lines carry.
const DemoCluster = "demo"

type demoService struct {
	namespace, workload, kind, container, team, app, node string
	pods                                                  []string
	rate                                                  float64 // lines per second
	line                                                  func(r *rand.Rand, t time.Time, spike bool) (level, stream, body, trace string)
}

var demoServices = []demoService{
	{
		namespace: "shop", workload: "frontend", kind: "Deployment", container: "web", team: "web", app: "frontend",
		node: "demo-node-a", pods: []string{"frontend-7d9c5b-2xk4q", "frontend-7d9c5b-9mzt1"}, rate: 1.2,
		line: func(r *rand.Rand, _ time.Time, _ bool) (string, string, string, string) {
			ms := 5 + r.Intn(60)
			switch x := r.Intn(100); {
			case x < 8:
				return "warn", "stdout", fmt.Sprintf("GET /product/%d 404 %dms", 1000+r.Intn(9000), ms), ""
			case x < 12:
				return "warn", "stdout", fmt.Sprintf("upstream checkout slow: %dms", 800+r.Intn(1500)), ""
			case x < 60:
				return "info", "stdout", fmt.Sprintf("GET /product/%d 200 %dms", 1000+r.Intn(9000), ms), ""
			default:
				return "info", "stdout", fmt.Sprintf("GET /cart 200 %dms", ms), ""
			}
		},
	},
	{
		namespace: "shop", workload: "checkout", kind: "Deployment", container: "app", team: "payments", app: "checkout",
		node: "demo-node-b", pods: []string{"checkout-6f7c9d-abcde", "checkout-6f7c9d-fghij"}, rate: 0.7,
		line: func(r *rand.Rand, _ time.Time, spike bool) (string, string, string, string) {
			order := fmt.Sprintf("ord-%06x", r.Intn(1<<24))
			if spike || r.Intn(100) < 2 {
				return "error", "stderr", fmt.Sprintf(`level=error msg="payment gateway timeout" upstream=payments-api timeout=5000ms order_id=%s`, order), ""
			}
			if r.Intn(100) < 10 {
				return "warn", "stdout", fmt.Sprintf(`level=warn msg="retrying payment" attempt=%d order_id=%s`, 2+r.Intn(2), order), ""
			}
			return "info", "stdout", fmt.Sprintf(`level=info msg="order placed" order_id=%s amount=%d.%02d duration=%dms`,
				order, 5+r.Intn(200), r.Intn(100), 40+r.Intn(400)), ""
		},
	},
	{
		namespace: "payments", workload: "payments-api", kind: "Deployment", container: "api", team: "payments", app: "payments-api",
		node: "demo-node-b", pods: []string{"payments-api-5c8f7-q2w3e"}, rate: 0.6,
		line: func(r *rand.Rand, _ time.Time, spike bool) (string, string, string, string) {
			trace := fmt.Sprintf("%016x%016x", r.Uint64(), r.Uint64())
			if spike && r.Intn(3) == 0 || r.Intn(100) < 3 {
				return "error", "stderr", fmt.Sprintf(`{"level":"error","msg":"card declined","code":"insufficient_funds","trace_id":"%s"}`, trace), trace
			}
			return "info", "stdout", fmt.Sprintf(`{"level":"info","msg":"charge created","amount":%d.%02d,"currency":"USD","status":200,"trace_id":"%s"}`,
				5+r.Intn(200), r.Intn(100), trace), trace
		},
	},
	{
		namespace: "platform", workload: "ingress-nginx", kind: "DaemonSet", container: "controller", team: "platform", app: "ingress-nginx",
		node: "demo-node-a", pods: []string{"ingress-nginx-controller-4x7zp"}, rate: 1.5,
		line: func(r *rand.Rand, t time.Time, _ bool) (string, string, string, string) {
			status, size := 200, 200+r.Intn(4000)
			path := []string{"/", "/api/v1/items", "/cart", "/checkout", "/healthz"}[r.Intn(5)]
			switch x := r.Intn(100); {
			case x < 3:
				status = 502
			case x < 8:
				status = 404
			}
			level := "info"
			if status >= 500 {
				level = "error"
			}
			return level, "stdout", fmt.Sprintf(`10.0.%d.%d - - [%s] "GET %s HTTP/1.1" %d %d "-" "Mozilla/5.0"`,
				r.Intn(4), 2+r.Intn(250), t.UTC().Format("02/Jan/2006:15:04:05 -0700"), path, status, size), ""
		},
	},
	{
		namespace: "data", workload: "etl-worker", kind: "CronJob", container: "worker", team: "data", app: "etl-worker",
		node: "demo-node-c", pods: []string{"etl-worker-28771440-k2j9d"}, rate: 0.3,
		line: func(r *rand.Rand, _ time.Time, _ bool) (string, string, string, string) {
			if r.Intn(4) == 0 {
				return "debug", "stdout", fmt.Sprintf(`level=debug msg="fetched page" page=%d rows=%d`, r.Intn(500), 500+r.Intn(500)), ""
			}
			return "info", "stdout", fmt.Sprintf(`level=info msg="batch processed" rows=%d duration=%d.%ds`, 1000+r.Intn(9000), 1+r.Intn(5), r.Intn(10)), ""
		},
	},
}

func demoSeed(service int, sec int64) int64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d/%d", service, sec)
	return int64(h.Sum64() & (1<<63 - 1))
}

// DemoRows returns the demo lines with ts in [from, to), oldest first;
// now bounds the populated window to [now - DemoWindow, now].
func DemoRows(from, to int64, now time.Time) []Row {
	lo := now.Add(-DemoWindow).UnixNano()
	hi := now.UnixNano() + 1
	if from < lo {
		from = lo
	}
	if to > hi {
		to = hi
	}
	if from >= to {
		return nil
	}
	var rows []Row
	for sec := from / int64(time.Second); sec <= (to-1)/int64(time.Second); sec++ {
		t := time.Unix(sec, 0).UTC()
		spike := t.Minute() >= 40 && t.Minute() < 50
		for i, svc := range demoServices {
			r := rand.New(rand.NewSource(demoSeed(i, sec)))
			rate := svc.rate
			if spike && svc.workload == "checkout" {
				rate *= 4
			}
			n := int(rate)
			if r.Float64() < rate-float64(n) {
				n++
			}
			for j := 0; j < n; j++ {
				ts := sec*int64(time.Second) + int64(r.Intn(int(time.Second)))
				level, stream, body, trace := svc.line(r, time.Unix(0, ts), spike)
				if ts < from || ts >= to {
					continue
				}
				pod := svc.pods[r.Intn(len(svc.pods))]
				rows = append(rows, Row{
					TS: ts,
					Labels: map[string]string{
						"cluster": DemoCluster, "namespace": svc.namespace, "workload": svc.workload,
						"workload_kind": svc.kind, "pod": pod, "container": svc.container, "node": svc.node,
						"team": svc.team, "stream": stream, "level": level,
						"app": svc.app, "source": "demo",
					},
					TraceID: trace,
					Body:    body,
				})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS < rows[j].TS })
	return rows
}

// NewDemoStore serves DemoRows.
func NewDemoStore(now func() time.Time) *MemStore {
	return &MemStore{Origin: "demo", Rows: func(from, to int64) []Row { return DemoRows(from, to, now()) }}
}
