// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package profiles

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

type inv struct{ pods []*corev1.Pod }

func (i inv) Pods() []*corev1.Pod      { return i.pods }
func (i inv) Node(string) *corev1.Node { return nil }

type owners struct{}

func (owners) Resolve(context.Context, *corev1.Pod) kube.Workload {
	return kube.Workload{Name: "checkout", Kind: "Deployment"}
}

func fn(id uint64, name string) *profile.Function {
	return &profile.Function{ID: id, Name: name, SystemName: name, Filename: name + ".go"}
}

// synthetic builds a CPU profile: main → handle → (json.Marshal | db.Query),
// with db.Query inlined into handle at one location.
func synthetic() *profile.Profile {
	fMain, fHandle, fJSON, fDB := fn(1, "main.main"), fn(2, "app.handle"), fn(3, "encoding/json.Marshal"), fn(4, "app/db.Query")
	lMain := &profile.Location{ID: 1, Address: 0x1000, Line: []profile.Line{{Function: fMain, Line: 10}}}
	lJSON := &profile.Location{ID: 2, Address: 0x2000, Line: []profile.Line{{Function: fJSON, Line: 20}}}
	// Inlined: callee (db.Query) first, caller (handle) last.
	lInl := &profile.Location{ID: 3, Address: 0x3000, Line: []profile.Line{{Function: fDB, Line: 30}, {Function: fHandle, Line: 31}}}
	lHandle := &profile.Location{ID: 4, Address: 0x4000, Line: []profile.Line{{Function: fHandle, Line: 40}}}
	lRaw := &profile.Location{ID: 5, Address: 0xdeadbeef}
	return &profile.Profile{
		SampleType:    []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		PeriodType:    &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:        10_000_000,
		TimeNanos:     1_760_000_000_000_000_000,
		DurationNanos: 15_000_000_000,
		Function:      []*profile.Function{fMain, fHandle, fJSON, fDB},
		Location:      []*profile.Location{lMain, lJSON, lInl, lHandle, lRaw},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{lJSON, lHandle, lMain}, Value: []int64{3, 30_000_000}},
			{Location: []*profile.Location{lJSON, lHandle, lMain}, Value: []int64{2, 20_000_000}}, // merged
			{Location: []*profile.Location{lInl, lMain}, Value: []int64{1, 10_000_000}},
			{Location: []*profile.Location{lRaw, lMain}, Value: []int64{1, 10_000_000}},
			{Location: []*profile.Location{lMain}, Value: []int64{0, 0}}, // dropped
		},
	}
}

func TestConvertCPU(t *testing.T) {
	out := Convert(synthetic(), "cpu", 100)
	if len(out) != 1 {
		t.Fatalf("profiles = %d", len(out))
	}
	p := out[0]
	if p.Type != "cpu" || p.Unit != "nanoseconds" || p.Period != 10_000_000 || p.DurationNano != 15_000_000_000 || p.TsUnixNano != 1_760_000_000_000_000_000 {
		t.Fatalf("profile header = %+v", p)
	}
	got := map[string]int64{}
	for _, s := range p.Samples {
		got[strings.Join(s.Frames, ";")] = s.Value
	}
	want := map[string]int64{
		"main.main;app.handle;encoding/json.Marshal": 50_000_000,
		"main.main;app.handle;app/db.Query":          10_000_000, // inlined frames expanded root→leaf
		"main.main;0xdeadbeef":                       10_000_000,
	}
	if len(got) != len(want) {
		t.Fatalf("stacks = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("stack %q = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	// Heaviest first.
	if p.Samples[0].Value != 50_000_000 {
		t.Fatalf("not sorted by weight: %v", p.Samples[0])
	}
}

func TestConvertTruncatesKeepingTotals(t *testing.T) {
	p := synthetic()
	out := Convert(p, "cpu", 2)[0]
	if len(out.Samples) != 2 || out.Samples[1].Frames[0] != "[truncated]" || out.Samples[1].Value != 20_000_000 {
		t.Fatalf("truncated = %+v", out.Samples)
	}
	var total int64
	for _, s := range out.Samples {
		total += s.Value
	}
	if total != 70_000_000 {
		t.Fatalf("total changed by truncation: %d", total)
	}
}

func TestConvertHeapEmitsInuseAndAlloc(t *testing.T) {
	f := fn(1, "main.alloc")
	loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: f}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"},
			{Type: "inuse_objects", Unit: "count"}, {Type: "inuse_space", Unit: "bytes"}},
		Period:   524288,
		Function: []*profile.Function{f}, Location: []*profile.Location{loc},
		Sample: []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{10, 4096, 2, 1024}}},
	}
	out := Convert(p, "memory", 100)
	if len(out) != 2 || out[0].Type != "inuse_space" || out[0].Samples[0].Value != 1024 || out[1].Type != "alloc_space" ||
		out[1].Samples[0].Value != 4096 || out[0].Unit != "bytes" || out[0].Period != 524288 {
		t.Fatalf("heap = %+v", out)
	}
}

func pod(name string, ann map[string]string, ports ...corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, UID: types.UID("u-" + name), Annotations: ann},
		Spec:       corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "app", Ports: ports}, {Name: "sidecar"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1"},
	}
}

func TestTargets(t *testing.T) {
	stopped := pod("stopped", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-port": "6060"})
	stopped.Status.Phase = corev1.PodSucceeded
	noIP := pod("noip", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-port": "6060"})
	noIP.Status.PodIP = ""
	other := pod("elsewhere", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-port": "6060"})
	other.Spec.NodeName = "node-b"
	s := New(Config{NodeName: "node-a"}, inv{pods: []*corev1.Pod{
		pod("grafana", map[string]string{
			"profiles.grafana.com/cpu.scrape": "true", "profiles.grafana.com/cpu.port_name": "http-metrics",
			"profiles.grafana.com/memory.scrape": "true", "profiles.grafana.com/memory.port": "8080",
			"profiles.grafana.com/memory.path":      "/custom/heap",
			"profiles.grafana.com/goroutine.scrape": "false",
		}, corev1.ContainerPort{Name: "http-metrics", ContainerPort: 9090}, corev1.ContainerPort{Name: "http", ContainerPort: 8080}),
		pod("kh", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-path": "/pprof/"},
			corev1.ContainerPort{Name: "debug", ContainerPort: 6060}),
		pod("badport", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-port": "99999"}),
		pod("badpath", map[string]string{"profiles.grafana.com/cpu.scrape": "true", "profiles.grafana.com/cpu.port": "1",
			"profiles.grafana.com/cpu.path": "/../../etc/passwd"}),
		pod("ambiguous", map[string]string{"kubehero.io/profile": "true"}, corev1.ContainerPort{ContainerPort: 1}, corev1.ContainerPort{ContainerPort: 2}),
		stopped, noIP, other,
	}}, owners{}, nil)
	var got []string
	for _, tg := range s.Targets() {
		got = append(got, tg.pod.Name+" "+tg.kind+" "+tg.url+" "+tg.container)
	}
	want := []string{
		"kh memory http://127.0.0.1:6060/pprof/heap app",
		"kh cpu http://127.0.0.1:6060/pprof/profile?seconds=15 app",
		"kh goroutine http://127.0.0.1:6060/pprof/goroutine app",
		"grafana memory http://127.0.0.1:8080/custom/heap app",
		"grafana cpu http://127.0.0.1:9090/debug/pprof/profile?seconds=15 app",
	}
	if strings.Join(sortedCopy(got), "\n") != strings.Join(sortedCopy(want), "\n") {
		t.Fatalf("targets:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// End to end against a pprof-serving pod.
func TestRoundScrapesAndEmits(t *testing.T) {
	var cpuBuf bytes.Buffer
	if err := synthetic().Write(&cpuBuf); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var cpuQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/profile", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cpuQuery = r.URL.RawQuery
		mu.Unlock()
		_, _ = w.Write(cpuBuf.Bytes())
	})
	mux.HandleFunc("/debug/pprof/goroutine", func(w http.ResponseWriter, _ *http.Request) {
		_ = pprof.Lookup("goroutine").WriteTo(w, 0) // a real runtime profile
	})
	mux.HandleFunc("/debug/pprof/heap", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	p := pod("checkout-abc", map[string]string{"kubehero.io/profile": "true", "kubehero.io/profile-port": portStr, kube.AnnotationService: "checkout-api"},
		corev1.ContainerPort{Name: "debug", ContainerPort: int32(port)})
	var reqs []*kuberov1.IngestProfilesRequest
	s := New(Config{ClusterID: "c1", NodeName: "node-a", CPUSeconds: 1, Logger: slog.New(slog.DiscardHandler)}, inv{pods: []*corev1.Pod{p}}, owners{},
		func(r *kuberov1.IngestProfilesRequest) { reqs = append(reqs, r) })
	errsBefore := metrics.ProfileScrapes.With("memory", "error").Get()
	s.Round(context.Background())

	if len(reqs) != 1 || reqs[0].ClusterId != "c1" {
		t.Fatalf("requests = %v", reqs)
	}
	byType := map[string]*kuberov1.Profile{}
	for _, pr := range reqs[0].Profiles {
		byType[pr.Type] = pr
	}
	if len(byType) != 2 || byType["cpu"] == nil || byType["goroutines"] == nil {
		t.Fatalf("profile types = %v", byType)
	}
	cpu := byType["cpu"]
	if cpu.Service != "checkout-api" || cpu.Origin != "pprof-scrape" || cpu.GetSource().GetContainer() != "app" ||
		cpu.GetSource().GetWorkload() != "checkout" || cpu.GetSource().GetPod() != "checkout-abc" || len(cpu.Samples) != 3 {
		t.Fatalf("cpu profile = %+v", cpu)
	}
	g := byType["goroutines"]
	if g.Unit != "count" || len(g.Samples) == 0 || g.TsUnixNano == 0 {
		t.Fatalf("goroutine profile = %+v", g)
	}
	found := false
	for _, smp := range g.Samples {
		if strings.Contains(strings.Join(smp.Frames, ";"), "testing.tRunner") {
			found = true
		}
	}
	if !found {
		t.Fatal("real goroutine profile should contain testing.tRunner frames")
	}
	mu.Lock()
	defer mu.Unlock()
	if cpuQuery != "seconds=1" {
		t.Fatalf("cpu query = %q", cpuQuery)
	}
	if metrics.ProfileScrapes.With("memory", "error").Get() != errsBefore+1 {
		t.Fatal("failed heap scrape not counted")
	}
}

func TestOversizedProfileRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p := pod("big", map[string]string{"profiles.grafana.com/goroutine.scrape": "true", "profiles.grafana.com/goroutine.port": portStr})
	emitted := 0
	s := New(Config{MaxBodyBytes: 1024, Logger: slog.New(slog.DiscardHandler)}, inv{pods: []*corev1.Pod{p}}, owners{},
		func(*kuberov1.IngestProfilesRequest) { emitted++ })
	before := metrics.ProfileScrapes.With("goroutine", "too_large").Get()
	s.Round(context.Background())
	if emitted != 0 || metrics.ProfileScrapes.With("goroutine", "too_large").Get() != before+1 {
		t.Fatalf("oversized profile: emitted %d", emitted)
	}
}

func TestSplitRespectsLimits(t *testing.T) {
	mk := func(n int) *kuberov1.Profile { return &kuberov1.Profile{Samples: make([]*kuberov1.StackSample, n)} }
	var ps []*kuberov1.Profile
	for range 5 {
		ps = append(ps, mk(9000))
	}
	reqs := Split("c", ps)
	if len(reqs) != 3 || len(reqs[0].Profiles) != 2 || len(reqs[2].Profiles) != 1 {
		t.Fatalf("split into %d requests", len(reqs))
	}
	many := make([]*kuberov1.Profile, 4500)
	for i := range many {
		many[i] = mk(1)
	}
	if reqs := Split("c", many); len(reqs) != 3 || len(reqs[0].Profiles) != maxProfilesPerRequest {
		t.Fatalf("profile-count split = %d", len(reqs))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	s := New(Config{Interval: 5 * time.Millisecond}, inv{}, owners{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
