// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
	"github.com/kubehero-io/platform/services/collector/internal/ship"
)

// ── harness ───────────────────────────────────────────────────────────

type fakePods map[string]*corev1.Pod

func (f fakePods) PodByUID(uid string) (*corev1.Pod, bool) { p, ok := f[uid]; return p, ok }
func (f fakePods) Node(string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "z1"}}}
}

type fakeOwners struct{}

func (fakeOwners) Resolve(_ context.Context, p *corev1.Pod) kube.Workload {
	return kube.Workload{Name: strings.TrimSuffix(p.Name, "-0"), Kind: "StatefulSet"}
}

type harness struct {
	t         *testing.T
	root      string
	positions string
	pods      fakePods

	mu       sync.Mutex
	entries  []*kuberov1.LogEntry
	requests int
	hold     bool
	held     []func(ship.Outcome)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, root: filepath.Join(dir, "pods"), positions: filepath.Join(dir, "state", "log-positions.json"), pods: fakePods{}}
	if err := os.MkdirAll(h.root, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) config() Config {
	return Config{
		Root: h.root, PositionsFile: h.positions, ClusterID: "c1", NodeName: "node-a",
		PodLabels:    []string{"app", "app.kubernetes.io/name"},
		PollInterval: 5 * time.Millisecond, RescanInterval: 30 * time.Millisecond,
		CheckpointInterval: 20 * time.Millisecond, BatchInterval: 10 * time.Millisecond,
		RateLimit: 1e6, Burst: 1e6,
		Logger: slog.New(slog.DiscardHandler),
	}
}

func (h *harness) emit(req *kuberov1.IngestLogsRequest, done func(ship.Outcome)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests++
	if req.ClusterId != "c1" {
		h.t.Errorf("cluster id = %q", req.ClusterId)
	}
	h.entries = append(h.entries, req.Entries...)
	if h.hold {
		h.held = append(h.held, done)
		return
	}
	done(ship.Sent)
}

// start runs a tailer; stop mimics the app's shutdown: cancel, wait for
// the final flush, settle held batches, then checkpoint.
func (h *harness) start(cfg Config) (stop func(settle ship.Outcome)) {
	tl := New(cfg, h.pods, fakeOwners{}, h.emit)
	tl.missingGrace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tl.Run(ctx); close(done) }()
	// Let the initial scan happen before the test appends.
	time.Sleep(30 * time.Millisecond)
	return func(settle ship.Outcome) {
		cancel()
		<-done
		h.mu.Lock()
		held := h.held
		h.held = nil
		h.mu.Unlock()
		for _, d := range held {
			d(settle)
		}
		tl.Checkpoint()
	}
}

func (h *harness) bodies() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.entries))
	for i, e := range h.entries {
		out[i] = e.Body
	}
	return out
}

func (h *harness) reset() {
	h.mu.Lock()
	h.entries, h.requests = nil, 0
	h.mu.Unlock()
}

func (h *harness) waitBodies(n int) []string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b := h.bodies(); len(b) >= n {
			time.Sleep(40 * time.Millisecond) // catch any extra (duplicate) lines
			return h.bodies()
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out: got %d lines, want %d: %q", len(h.bodies()), n, h.bodies())
	return nil
}

func (h *harness) containerDir(ns, pod, uid, container string) string {
	d := filepath.Join(h.root, ns+"_"+pod+"_"+uid, container)
	if err := os.MkdirAll(d, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) addPod(ns, name, uid string, labels map[string]string) {
	h.pods[uid] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid), Labels: labels},
		Spec: corev1.PodSpec{NodeName: "node-a"}}
}

func cri(stream, tag, msg string) string {
	return time.Now().UTC().Format(time.RFC3339Nano) + " " + stream + " " + tag + " " + msg + "\n"
}

func appendFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func seq(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, cri("stdout", "F", fmt.Sprintf("%s-%d", prefix, i)))
	}
	return out
}

func wantBodies(t *testing.T, got []string, prefix string, from, to int) {
	t.Helper()
	var want []string
	for i := from; i <= to; i++ {
		want = append(want, fmt.Sprintf("%s-%d", prefix, i))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lines:\n got  %q\n want %q", got, want)
	}
}

// ── tests ─────────────────────────────────────────────────────────────

func TestFirstStartTailsFromEndWithMetadata(t *testing.T) {
	h := newHarness(t)
	h.addPod("shop", "cart-0", "uid-1", map[string]string{"app": "cart", "app.kubernetes.io/name": "cart", "team": "ignored"})
	log := filepath.Join(h.containerDir("shop", "cart-0", "uid-1", "app"), "0.log")
	appendFile(t, log, seq("old", 1, 3)...)
	// The runtime is mid-write when we start: tailing "from the end"
	// must begin at that line's start, never in its middle.
	appendFile(t, log, "2026-09-28T10:00:00Z stdout F in-progress")

	stop := h.start(h.config())
	appendFile(t, log, " line\n")
	appendFile(t, log, cri("stderr", "F", `{"level":"error","msg":"boom","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`))
	got := h.waitBodies(2)
	stop(ship.Sent)
	if len(got) != 2 || got[0] != "in-progress line" {
		t.Fatalf("existing content must be skipped on first start, got %q", got)
	}
	h.mu.Lock()
	e := h.entries[1]
	h.mu.Unlock()
	if e.Stream != "stderr" || e.Level != LevelError || e.TraceId != "4bf92f3577b34da6a3ce929d0e0e4736" || e.TsUnixNano == 0 {
		t.Fatalf("entry = %+v", e)
	}
	src := e.GetSource()
	if src.GetNamespace() != "shop" || src.GetPod() != "cart-0" || src.GetContainer() != "app" || src.GetWorkload() != "cart" ||
		src.GetWorkloadKind() != "StatefulSet" || src.GetNode() != "node-a" || src.GetZone() != "z1" || src.GetPodUid() != "uid-1" {
		t.Fatalf("source = %+v", src)
	}
	if e.Labels["app"] != "cart" || e.Labels["app_kubernetes_io_name"] != "cart" || len(e.Labels) != 2 {
		t.Fatalf("labels = %v (allow-listed and sanitised only)", e.Labels)
	}
}

func TestFromStartAndLaterFiles(t *testing.T) {
	h := newHarness(t)
	h.addPod("shop", "api-0", "uid-1", nil)
	log := filepath.Join(h.containerDir("shop", "api-0", "uid-1", "api"), "0.log")
	appendFile(t, log, seq("a", 1, 3)...)
	cfg := h.config()
	cfg.FromStart = true
	stop := h.start(cfg)
	wantBodies(t, h.waitBodies(3), "a", 1, 3)

	// A container that appears later is read from its first line, even
	// without FromStart, and even if its pod isn't in the cache yet.
	h.reset()
	late := filepath.Join(h.containerDir("shop", "late-0", "uid-2", "worker"), "0.log")
	appendFile(t, late, seq("late", 1, 4)...)
	got := h.waitBodies(4)
	stop(ship.Sent)
	wantBodies(t, got, "late", 1, 4)
	h.mu.Lock()
	src := h.entries[0].GetSource()
	h.mu.Unlock()
	if src.GetPod() != "late-0" || src.GetContainer() != "worker" || src.GetWorkload() != "" || src.GetNode() != "node-a" {
		t.Fatalf("unresolved pod source = %+v", src)
	}
}

func TestPartialLinesAndCap(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log) // exists at start (empty)
	cfg := h.config()
	cfg.MaxLineBytes = 16
	stop := h.start(cfg)
	appendFile(t, log,
		cri("stdout", "P", "hello "),
		cri("stderr", "F", "interleaved stderr"), // other stream: independent
		cri("stdout", "P", "big "),
		cri("stdout", "F", "world"),
		cri("stdout", "P", "0123456789"),
		cri("stdout", "F", "0123456789-overflow"),
		`{"log":"docker partial ","stream":"stdout","time":"2026-09-28T10:00:00Z"}`+"\n",
		`{"log":"done\n","stream":"stdout","time":"2026-09-28T10:00:01Z"}`+"\n",
	)
	got := h.waitBodies(4)
	stop(ship.Sent)
	want := []string{"interleaved stde", "hello big world", "0123456789012345", "docker partial d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Rotation: the kubelet renames 0.log aside, the runtime keeps writing
// to it until it reopens, then writes to a new 0.log. Every line once,
// in order.
func TestRotationNoLossNoDupes(t *testing.T) {
	h := newHarness(t)
	dir := h.containerDir("ns", "p", "u", "c")
	log := filepath.Join(dir, "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("l", 1, 5)...)
	h.waitBodies(5)
	rotated := log + ".20260928-100000"
	if err := os.Rename(log, rotated); err != nil {
		t.Fatal(err)
	}
	appendFile(t, rotated, seq("l", 6, 8)...) // runtime not reopened yet
	time.Sleep(20 * time.Millisecond)
	appendFile(t, log, seq("l", 9, 12)...) // reopened: new inode at the path
	got := h.waitBodies(12)
	stop(ship.Sent)
	wantBodies(t, got, "l", 1, 12)
	if metrics.LogRotations.With("rotated").Get() < 1 {
		t.Fatal("rotation not counted")
	}
}

func TestTruncationRestartsAtZero(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("before", 1, 3)...)
	h.waitBodies(3)
	if err := os.Truncate(log, 0); err != nil { // copytruncate
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the tailer notice before the file regrows
	appendFile(t, log, seq("after", 1, 2)...)
	got := h.waitBodies(5)
	stop(ship.Sent)
	if strings.Join(got[3:], ",") != "after-1,after-2" {
		t.Fatalf("after truncation got %q", got)
	}
}

func TestRestartFromCheckpoint(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("run1", 1, 5)...)
	h.waitBodies(5)
	stop(ship.Sent)

	// Written while the collector is down.
	appendFile(t, log, seq("down", 1, 3)...)
	h.reset()
	stop = h.start(h.config())
	appendFile(t, log, seq("run2", 1, 2)...)
	got := h.waitBodies(5)
	stop(ship.Sent)
	want := "down-1,down-2,down-3,run2-1,run2-2"
	if strings.Join(got, ",") != want {
		t.Fatalf("after restart got %q, want %q (no dupes, no loss)", got, want)
	}
}

// Rotated while down: the tail of the renamed file is resumed by inode
// and the new file is read from its start.
func TestRestartAfterRotationWhileDown(t *testing.T) {
	h := newHarness(t)
	dir := h.containerDir("ns", "p", "u", "c")
	log := filepath.Join(dir, "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("x", 1, 3)...)
	h.waitBodies(3)
	stop(ship.Sent)

	appendFile(t, log, seq("x", 4, 5)...)
	if err := os.Rename(log, log+".20260928-120000"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, log, seq("x", 6, 7)...)
	h.reset()
	stop = h.start(h.config())
	got := h.waitBodies(4)
	stop(ship.Sent)
	wantBodies(t, got, "x", 4, 7)
}

// Batches still queued at shutdown (control plane unreachable) are
// Abandoned: their lines must be read again after restart.
func TestAbandonedBatchesAreReplayed(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("ok", 1, 2)...)
	h.waitBodies(2)
	h.mu.Lock()
	h.hold = true
	h.mu.Unlock()
	appendFile(t, log, seq("stuck", 1, 3)...)
	h.waitBodies(5)
	stop(ship.Abandoned)

	h.reset()
	h.mu.Lock()
	h.hold = false
	h.mu.Unlock()
	stop = h.start(h.config())
	got := h.waitBodies(3)
	stop(ship.Sent)
	wantBodies(t, got, "stuck", 1, 3)
}

func TestPositionsFileIsAtomicJSON(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log)
	stop := h.start(h.config())
	appendFile(t, log, seq("p", 1, 2)...)
	h.waitBodies(2)
	stop(ship.Sent)
	byPath, existed, err := loadPositions(h.positions)
	if err != nil || !existed {
		t.Fatalf("load: %v existed=%v", err, existed)
	}
	fi, _ := os.Stat(log)
	ps := byPath[log]
	if len(ps) != 1 || ps[0].Offset != fi.Size() || ps[0].Inode != inode(fi) {
		t.Fatalf("positions = %+v (size %d)", ps, fi.Size())
	}
	if entries, _ := os.ReadDir(filepath.Dir(h.positions)); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestRateLimitDropsAndCounts(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "noisy", "u", "c"), "0.log")
	appendFile(t, log)
	cfg := h.config()
	cfg.RateLimit, cfg.Burst = 1, 5
	before := metrics.LogLinesDropped.With("rate_limited").Get()
	stop := h.start(cfg)
	appendFile(t, log, seq("n", 1, 100)...)
	time.Sleep(200 * time.Millisecond)
	stop(ship.Sent)
	got := h.bodies()
	if len(got) < 5 || len(got) > 7 {
		t.Fatalf("shipped %d lines with burst 5 at 1/s", len(got))
	}
	if dropped := metrics.LogLinesDropped.With("rate_limited").Get() - before; dropped < 90 {
		t.Fatalf("dropped counter = %v", dropped)
	}
}

func TestExcludedNamespaceAndBadDirs(t *testing.T) {
	h := newHarness(t)
	appendFile(t, filepath.Join(h.containerDir("kubehero", "collector-x", "u1", "collector"), "0.log"))
	appendFile(t, filepath.Join(h.containerDir("shop", "api", "u2", "api"), "0.log"))
	if err := os.MkdirAll(filepath.Join(h.root, "not-a-pod-dir", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := h.config()
	cfg.ExcludeNamespaces = []string{"kubehero"}
	stop := h.start(cfg)
	appendFile(t, filepath.Join(h.root, "kubehero_collector-x_u1", "collector", "0.log"), seq("self", 1, 3)...)
	appendFile(t, filepath.Join(h.root, "shop_api_u2", "api", "0.log"), seq("app", 1, 1)...)
	got := h.waitBodies(1)
	stop(ship.Sent)
	wantBodies(t, got, "app", 1, 1)
}

func TestDeletedPodFileIsClosed(t *testing.T) {
	h := newHarness(t)
	podDir := filepath.Join(h.root, "ns_gone_u9")
	log := filepath.Join(h.containerDir("ns", "gone", "u9", "c"), "0.log")
	appendFile(t, log)
	cfg := h.config()
	tl := New(cfg, h.pods, fakeOwners{}, h.emit)
	tl.missingGrace = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tl.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	appendFile(t, log, seq("last", 1, 2)...)
	h.waitBodies(2)
	if err := os.RemoveAll(podDir); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && tl.commits.len() > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n := tl.commits.len(); n != 0 {
		t.Fatalf("deleted file still tracked (%d)", n)
	}
	wantBodies(t, h.bodies(), "last", 1, 2)
}

func TestBatchesSplitBySize(t *testing.T) {
	h := newHarness(t)
	log := filepath.Join(h.containerDir("ns", "p", "u", "c"), "0.log")
	appendFile(t, log)
	cfg := h.config()
	cfg.BatchBytes = 300 // 4 entries of ~99 bytes (body + per-entry overhead) cross it
	cfg.BatchInterval = time.Hour
	stop := h.start(cfg)
	appendFile(t, log, seq("b", 1, 9)...)
	h.waitBodies(8)
	stop(ship.Sent) // shutdown flushes the remainder
	h.mu.Lock()
	reqs := h.requests
	h.mu.Unlock()
	if len(h.bodies()) != 9 || reqs < 3 {
		t.Fatalf("lines %d in %d requests", len(h.bodies()), reqs)
	}
}

func TestSanitizeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"app":                       "app",
		"app.kubernetes.io/name":    "app_kubernetes_io_name",
		"9lives":                    "_lives",
		"version":                   "version",
		"helm.sh/chart":             "helm_sh_chart",
		"topology.kubernetes.io/zo": "topology_kubernetes_io_zo",
	} {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePodDir(t *testing.T) {
	if ns, pod, uid, ok := parsePodDir("kube-system_coredns-5d78c9869d-abcde_0b9a6d9e-1111-2222-3333-444455556666"); !ok ||
		ns != "kube-system" || pod != "coredns-5d78c9869d-abcde" || uid != "0b9a6d9e-1111-2222-3333-444455556666" {
		t.Fatalf("parsePodDir = %q %q %q %v", ns, pod, uid, ok)
	}
	for _, bad := range []string{"a_b", "a_b_c_d", "_b_c", "plain"} {
		if _, _, _, ok := parsePodDir(bad); ok {
			t.Errorf("parsePodDir(%q) accepted", bad)
		}
	}
}
