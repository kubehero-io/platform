// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
)

// recorder is a fake control plane: both services, recording requests
// and the auth header they carried.
type recorder struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	kuberov1connect.UnimplementedTelemetryServiceHandler
	mu     sync.Mutex
	cost   []*kuberov1.IngestPodCostRequest
	usage  []*kuberov1.IngestUsageRequest
	events []*kuberov1.IngestEventsRequest
	logs   []*kuberov1.IngestLogsRequest
	auth   map[string]bool
}

func (r *recorder) note(h http.Header) {
	r.auth[h.Get("Authorization")] = true
}

func (r *recorder) IngestPodCost(_ context.Context, req *connect.Request[kuberov1.IngestPodCostRequest]) (*connect.Response[kuberov1.IngestPodCostResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note(req.Header())
	r.cost = append(r.cost, req.Msg)
	return connect.NewResponse(&kuberov1.IngestPodCostResponse{Written: int32(len(req.Msg.Samples))}), nil
}

func (r *recorder) IngestUsage(_ context.Context, req *connect.Request[kuberov1.IngestUsageRequest]) (*connect.Response[kuberov1.IngestUsageResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note(req.Header())
	r.usage = append(r.usage, req.Msg)
	return connect.NewResponse(&kuberov1.IngestUsageResponse{Accepted: int32(len(req.Msg.Usage))}), nil
}

func (r *recorder) IngestEvents(_ context.Context, req *connect.Request[kuberov1.IngestEventsRequest]) (*connect.Response[kuberov1.IngestEventsResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note(req.Header())
	r.events = append(r.events, req.Msg)
	return connect.NewResponse(&kuberov1.IngestEventsResponse{Accepted: int32(len(req.Msg.Events))}), nil
}

func (r *recorder) IngestLogs(_ context.Context, req *connect.Request[kuberov1.IngestLogsRequest]) (*connect.Response[kuberov1.IngestLogsResponse], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.note(req.Header())
	r.logs = append(r.logs, req.Msg)
	return connect.NewResponse(&kuberov1.IngestLogsResponse{Accepted: int32(len(req.Msg.Entries))}), nil
}

type stats struct{}

func (stats) Summary(context.Context, string) (*kubeletstats.Summary, error) {
	return &kubeletstats.Summary{
		Node: kubeletstats.NodeStats{CPUMillicores: 2500, MemoryWorkingSetBytes: 8 << 30, HasCPU: true, HasMemory: true},
		Pods: map[kubeletstats.PodKey]kubeletstats.PodStats{
			{Namespace: "shop", Name: "api-0"}: {UID: "u-api", Usage: kubeletstats.Usage{CPUMillicores: 300, MemoryWorkingSetBytes: 200 << 20},
				Containers: map[string]kubeletstats.ContainerUsage{"api": {CPUNanoCores: 300_000_000, MemoryWorkingSetBytes: 200 << 20, HasCPU: true, HasMemory: true}}},
		},
	}, nil
}

func runningPod(node, name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: typesUID(uid)},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "api", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api"}}},
	}
}

func TestAppEndToEnd(t *testing.T) {
	rec := &recorder{auth: map[string]bool{}}
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(rec))
	mux.Handle(kuberov1connect.NewTelemetryServiceHandler(rec))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Annotations: map[string]string{"kubehero.io/node-hourly-usd": "0.40"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"),
		}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "train-0", UID: "u-train", CreationTimestamp: metav1.Now()},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "t"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes are available",
		}}},
	}
	cs := fake.NewSimpleClientset(node, runningPod("node-a", "api-0", "u-api"), runningPod("node-b", "remote-0", "u-remote"), pending)

	dir := t.TempDir()
	logDir := filepath.Join(dir, "pods", "shop_api-0_u-api", "api")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(logDir, "0.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Version: "test", Addr: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler),
		NodeName: "node-a", PodName: "collector-a", PodNamespace: "kubehero", ClusterID: "kind-dev",
		ControlPlaneURL: srv.URL, ControlPlaneToken: "tok",
		ScanInterval: 50 * time.Millisecond, UsageInterval: 50 * time.Millisecond,
		Events: true, LeaderElect: true,
		Logs:            LogsConfig{Enabled: true, Root: filepath.Join(dir, "pods"), PositionsFile: filepath.Join(dir, "state", "pos.json"), From: "end", RateLimit: 1000, Burst: 1000},
		EBPF:            EBPFConfig{Enabled: true, Netflow: true, Profiler: true}, // unsupported here → must degrade
		ShutdownTimeout: 5 * time.Second,
		Client:          cs,
		Stats:           stats{},
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()

	waitFor(t, "cache sync", func() bool { return a.ready.Load() })
	time.Sleep(100 * time.Millisecond) // let the tailer's first scan run
	f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + " stdout F level=warn msg=\"hello from api\"\n")
	_ = f.Close()

	waitFor(t, "every signal shipped", func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		hasPending := false
		for _, r := range rec.events {
			for _, e := range r.Events {
				if e.Kind == "unschedulable" && e.GetSource().GetPod() == "train-0" {
					hasPending = true
				}
			}
		}
		return len(rec.cost) > 0 && len(rec.usage) > 0 && len(rec.logs) > 0 && hasPending
	})

	// /metrics: real chargeback series + self-telemetry.
	w := httptest.NewRecorder()
	a.serveMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`kubehero_pod_cost_usd_per_second{cloud="",cluster="kind-dev",cost_center="",namespace="shop",nodepool="",pod="api-0"`,
		`kubehero_node_cost_usd_per_hour{`,
		`kubehero_collector_items_sent_total{signal="cost"}`,
		`kubehero_collector_leader 1`,
		`# TYPE kubehero_collector_queue_items gauge`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if strings.Contains(body, `source="demo"`) {
		t.Error("demo series must be off by default")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	// Node locality: only node-a's pod is priced, plus exactly one node sample.
	req := rec.cost[0]
	if req.ClusterId != "kind-dev" || len(req.Samples) != 1 || req.Samples[0].Pod != "api-0" || len(req.Nodes) != 1 || req.Nodes[0].Node != "node-a" {
		t.Fatalf("cost request = %v", req)
	}
	if s := req.Samples[0]; s.CpuUsageMillicores != 300 || s.CostUsdSec <= 0 || req.Nodes[0].PriceSource != "annotation" {
		t.Fatalf("cost sample = %+v / node %+v", s, req.Nodes[0])
	}
	if u := rec.usage[0].Usage; len(u) != 1 || u[0].GetSource().GetContainer() != "api" || u[0].CpuUsageCores != 0.3 {
		t.Fatalf("usage = %v", u)
	}
	e := rec.logs[0].Entries[0]
	if e.Body != `level=warn msg="hello from api"` || e.Level != "warn" || e.GetSource().GetPod() != "api-0" {
		t.Fatalf("log entry = %+v", e)
	}
	if !rec.auth["Bearer tok"] || len(rec.auth) != 1 {
		t.Fatalf("auth headers = %v", rec.auth)
	}
	// Shutdown checkpointed the shipped offset.
	raw, err := os.ReadFile(filepath.Join(dir, "state", "pos.json"))
	if err != nil || !strings.Contains(string(raw), logFile) {
		t.Fatalf("positions after shutdown = %s, %v", raw, err)
	}
}

func TestAppWithoutControlPlane(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, runningPod("node-a", "api-0", "u-api"))
	a, err := newApp(Config{Version: "test", Addr: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler), NodeName: "node-a",
		ScanInterval: 20 * time.Millisecond, Client: cs, Stats: stats{}, Logs: LogsConfig{Enabled: true}, Events: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()
	// Cost is still computed for /metrics.
	waitFor(t, "chargeback series", func() bool { return a.scanner != nil && len(a.scanner.Series()) > 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func typesUID(s string) types.UID { return types.UID(s) }
