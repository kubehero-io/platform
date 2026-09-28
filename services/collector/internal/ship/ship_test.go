// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ship

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// fakeTelemetry records what arrives and can fail the first N calls.
type fakeTelemetry struct {
	kuberov1connect.UnimplementedTelemetryServiceHandler
	mu       sync.Mutex
	failN    int
	failCode connect.Code
	events   []*kuberov1.IngestEventsRequest
	headers  []http.Header
}

func (f *fakeTelemetry) IngestEvents(_ context.Context, req *connect.Request[kuberov1.IngestEventsRequest]) (*connect.Response[kuberov1.IngestEventsResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, req.Header().Clone())
	if f.failN > 0 {
		f.failN--
		return nil, connect.NewError(f.failCode, errors.New("injected"))
	}
	f.events = append(f.events, req.Msg)
	return connect.NewResponse(&kuberov1.IngestEventsResponse{Accepted: int32(len(req.Msg.GetEvents())) - 1, Dropped: 1}), nil
}

type fakeControlPlane struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	mu   sync.Mutex
	reqs []*kuberov1.IngestPodCostRequest
}

func (f *fakeControlPlane) IngestPodCost(_ context.Context, req *connect.Request[kuberov1.IngestPodCostRequest]) (*connect.Response[kuberov1.IngestPodCostResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	return connect.NewResponse(&kuberov1.IngestPodCostResponse{Written: int32(len(req.Msg.GetSamples()))}), nil
}

type wire struct {
	contentType, encoding, auth, ua string
}

func newServer(t *testing.T, tel *fakeTelemetry, cp *fakeControlPlane) (*httptest.Server, *[]wire, *sync.Mutex) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewTelemetryServiceHandler(tel))
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(cp))
	var mu sync.Mutex
	var seen []wire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, wire{
			contentType: r.Header.Get("Content-Type"),
			encoding:    r.Header.Get("Content-Encoding"),
			auth:        r.Header.Get("Authorization"),
			ua:          r.Header.Get("User-Agent"),
		})
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen, &mu
}

func TestClientSendsProtoGzipBearer(t *testing.T) {
	tel, cp := &fakeTelemetry{}, &fakeControlPlane{}
	srv, seen, mu := newServer(t, tel, cp)
	c, err := NewClient(Options{URL: srv.URL + "/", Token: "s3cr3t", UserAgent: "kubehero-collector/test"})
	if err != nil {
		t.Fatal(err)
	}
	want := &kuberov1.IngestPodCostRequest{
		ClusterId: "eks-use1-prod",
		Samples: []*kuberov1.PodCostSample{{
			Namespace: "shop", Pod: "checkout-1", CostUsdSec: 0.0001, CpuCostUsdSec: 0.00006, RamCostUsdSec: 0.00004,
			Workload: "checkout", WorkloadKind: "Deployment", IntervalSec: 5, Labels: map[string]string{"app": "checkout"},
		}},
		Nodes: []*kuberov1.NodeCostSample{{Node: "n1", PricePerHour: 0.192, PriceSource: "estimate"}},
	}
	accepted, dropped, err := c.SendPodCost(context.Background(), want)
	if err != nil || accepted != 1 || dropped != 0 {
		t.Fatalf("SendPodCost = %d, %d, %v", accepted, dropped, err)
	}
	cp.mu.Lock()
	got := cp.reqs[0]
	cp.mu.Unlock()
	if !proto.Equal(got, want) {
		t.Fatalf("server received %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	w := (*seen)[0]
	if w.contentType != "application/proto" {
		t.Errorf("Content-Type = %q, want binary protobuf", w.contentType)
	}
	if w.encoding != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", w.encoding)
	}
	if w.auth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q", w.auth)
	}
	if w.ua != "kubehero-collector/test" {
		t.Errorf("User-Agent = %q", w.ua)
	}
}

func TestNewClientValidatesURL(t *testing.T) {
	for _, u := range []string{"", "cp:8080", "ftp://x"} {
		if _, err := NewClient(Options{URL: u}); err == nil {
			t.Errorf("NewClient(%q) must fail", u)
		}
	}
}

// End to end: the queue retries through a flapping control plane and
// accounts server-side drops.
func TestQueueRetriesThroughConnect(t *testing.T) {
	tel := &fakeTelemetry{failN: 2, failCode: connect.CodeUnavailable}
	srv, _, _ := newServer(t, tel, &fakeControlPlane{})
	c, err := NewClient(Options{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	q := NewQueue(QueueConfig{Signal: "events_e2e", InitialBackoff: time.Millisecond}, c.SendEvents, nil)
	var outcome atomic.Int32
	outcome.Store(-1)
	done := make(chan struct{})
	q.Enqueue(Batch[*kuberov1.IngestEventsRequest]{
		Req:   &kuberov1.IngestEventsRequest{Events: []*kuberov1.ClusterEvent{{Kind: "oom_killed"}, {Kind: "restarted"}}},
		Items: 2, Bytes: 10,
		Done: func(o Outcome) { outcome.Store(int32(o)); close(done) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never completed")
	}
	if Outcome(outcome.Load()) != Sent {
		t.Fatalf("outcome = %d, want Sent", outcome.Load())
	}
	tel.mu.Lock()
	defer tel.mu.Unlock()
	if len(tel.headers) != 3 || len(tel.events) != 1 {
		t.Fatalf("calls = %d, successes = %d; want 3 calls, 1 success", len(tel.headers), len(tel.events))
	}
	if got := metrics.ItemsSent.With("events_e2e").Get(); got != 1 {
		t.Errorf("items sent = %v, want 1 (server accepted 1)", got)
	}
	if got := metrics.ItemsDropped.With("events_e2e", "server").Get(); got != 1 {
		t.Errorf("server drops = %v, want 1", got)
	}
	if got := metrics.SendErrors.With("events_e2e", "unavailable").Get(); got != 2 {
		t.Errorf("send errors = %v, want 2", got)
	}
}

// ── queue semantics with a scripted sender ────────────────────────────

type script struct {
	mu    sync.Mutex
	errs  []error // consumed per call; nil = success
	calls []string
}

func (s *script) send(_ context.Context, req string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if len(s.errs) == 0 {
		return 1, 0, nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return 1, 0, err
}

func unavailable() error { return connect.NewError(connect.CodeUnavailable, errors.New("down")) }

func newTestQueue(signal string, s *script, cfg QueueConfig) (*Queue[string], *[]time.Duration) {
	cfg.Signal = signal
	q := NewQueue(cfg, s.send, nil)
	var sleeps []time.Duration
	q.sleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return ctx.Err() == nil
	}
	return q, &sleeps
}

func runUntilIdle(q *Queue[string]) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	for {
		if n, _ := q.Len(); n == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the in-flight batch finish
	cancel()
	<-done
}

func TestQueueOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		errs     []error
		want     Outcome
		calls    int
		reason   string
		maxTries int
	}{
		{"success", nil, Sent, 1, "", 0},
		{"retry then success", []error{unavailable(), unavailable()}, Sent, 3, "", 0},
		{"non-retryable rejection", []error{connect.NewError(connect.CodeInvalidArgument, errors.New("bad"))}, Dropped, 1, "rejected", 0},
		{"unauthenticated is not retried", []error{connect.NewError(connect.CodeUnauthenticated, errors.New("no"))}, Dropped, 1, "rejected", 0},
		{"retries exhausted", []error{unavailable(), unavailable(), unavailable()}, Dropped, 3, "retries_exhausted", 3},
		{"plain network error retried", []error{errors.New("connection refused")}, Sent, 2, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			signal := "t_" + c.name
			s := &script{errs: c.errs}
			q, sleeps := newTestQueue(signal, s, QueueConfig{MaxAttempts: c.maxTries})
			var got Outcome = -1
			q.Enqueue(Batch[string]{Req: "b1", Items: 4, Bytes: 100, Done: func(o Outcome) { got = o }})
			runUntilIdle(q)
			if got != c.want {
				t.Fatalf("outcome = %d, want %d", got, c.want)
			}
			if len(s.calls) != c.calls {
				t.Fatalf("calls = %d, want %d", len(s.calls), c.calls)
			}
			if len(*sleeps) != max(0, c.calls-1) && c.want == Sent {
				t.Fatalf("sleeps = %d between %d calls", len(*sleeps), c.calls)
			}
			if c.reason != "" {
				if got := metrics.ItemsDropped.With(signal, c.reason).Get(); got != 4 {
					t.Fatalf("dropped{%s} = %v, want 4", c.reason, got)
				}
			}
		})
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	s := &script{}
	q, _ := newTestQueue("t_overflow", s, QueueConfig{MaxItems: 10, MaxBytes: 1000})
	outcomes := map[string]Outcome{}
	for _, name := range []string{"a", "b", "c", "d"} {
		q.Enqueue(Batch[string]{Req: name, Items: 4, Bytes: 10, Done: func(o Outcome) { outcomes[name] = o }})
	}
	// 4 batches × 4 items against a 10-item budget: a, b dropped.
	if n, b := q.Len(); n != 8 || b != 20 {
		t.Fatalf("queue = %d items / %d bytes, want 8 / 20", n, b)
	}
	if outcomes["a"] != Dropped || outcomes["b"] != Dropped {
		t.Fatalf("oldest must be dropped first: %v", outcomes)
	}
	if got := metrics.ItemsDropped.With("t_overflow", "queue_full").Get(); got != 8 {
		t.Fatalf("queue_full drops = %v, want 8", got)
	}
	// A batch bigger than the whole budget is dropped itself.
	var huge Outcome = -1
	q.Enqueue(Batch[string]{Req: "huge", Items: 11, Bytes: 10, Done: func(o Outcome) { huge = o }})
	if huge != Dropped {
		t.Fatal("oversized batch must be dropped")
	}
	runUntilIdle(q)
	if len(s.calls) != 2 || s.calls[0] != "c" || s.calls[1] != "d" {
		t.Fatalf("sent %v, want [c d] in order", s.calls)
	}
	if n, b := q.Len(); n != 0 || b != 0 {
		t.Fatalf("queue not drained: %d/%d", n, b)
	}
	if metrics.QueueItems.With("t_overflow").Get() != 0 {
		t.Fatal("queue gauge not reset")
	}
}

func TestQueueByteBudget(t *testing.T) {
	q, _ := newTestQueue("t_bytes", &script{}, QueueConfig{MaxItems: 1000, MaxBytes: 100})
	for range 5 {
		q.Enqueue(Batch[string]{Req: "x", Items: 1, Bytes: 40})
	}
	if _, b := q.Len(); b != 80 {
		t.Fatalf("bytes = %d, want 80 (two 40-byte batches fit in 100)", b)
	}
}

func TestQueueShutdownFlushAndAbandon(t *testing.T) {
	// Control plane down: Run is cancelled mid-retry, the batch goes
	// back to the head, Flush tries once, then abandons.
	s := &script{errs: []error{unavailable(), unavailable(), unavailable(), unavailable()}}
	q, _ := newTestQueue("t_shutdown", s, QueueConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	q.sleep = func(context.Context, time.Duration) bool { cancel(); return false }
	var o1, o2 Outcome = -1, -1
	q.Enqueue(Batch[string]{Req: "one", Items: 1, Bytes: 1, Done: func(o Outcome) { o1 = o }})
	q.Enqueue(Batch[string]{Req: "two", Items: 1, Bytes: 1, Done: func(o Outcome) { o2 = o }})
	q.Run(ctx)
	if n, _ := q.Len(); n != 2 {
		t.Fatalf("interrupted batch must be requeued, queue has %d", n)
	}
	fctx, fcancel := context.WithTimeout(context.Background(), time.Second)
	defer fcancel()
	q.Flush(fctx)
	// Each batch gets one flush attempt; a retryable failure at
	// shutdown is Abandoned (replayable), never silently Dropped.
	if o1 != Abandoned || o2 != Abandoned {
		t.Fatalf("outcomes = %d %d, want Abandoned for both", o1, o2)
	}
	if metrics.ItemsDropped.With("t_shutdown", "shutdown").Get() != 2 {
		t.Fatal("abandoned items must be counted as shutdown drops")
	}
	// With the control plane back, Flush ships everything.
	s2 := &script{}
	q2, _ := newTestQueue("t_shutdown_ok", s2, QueueConfig{})
	var ok Outcome = -1
	q2.Enqueue(Batch[string]{Req: "x", Items: 1, Bytes: 1, Done: func(o Outcome) { ok = o }})
	q2.Flush(fctx)
	if ok != Sent {
		t.Fatalf("flush outcome = %d", ok)
	}
	// An expired flush context abandons (producers may replay).
	q3, _ := newTestQueue("t_shutdown_expired", &script{}, QueueConfig{})
	var ab Outcome = -1
	q3.Enqueue(Batch[string]{Req: "x", Items: 1, Bytes: 1, Done: func(o Outcome) { ab = o }})
	expired, c3 := context.WithCancel(context.Background())
	c3()
	q3.Flush(expired)
	if ab != Abandoned {
		t.Fatalf("expired flush outcome = %d, want Abandoned", ab)
	}
	if metrics.ItemsDropped.With("t_shutdown_expired", "shutdown").Get() != 1 {
		t.Fatal("abandoned items must be counted")
	}
}

func TestBackoffBounds(t *testing.T) {
	q, _ := newTestQueue("t_backoff", &script{}, QueueConfig{InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	for attempt := 1; attempt <= 30; attempt++ {
		d := q.backoff(attempt)
		ceiling := min(100*time.Millisecond<<min(attempt-1, 20), time.Second)
		if d < ceiling/2 || d > ceiling {
			t.Fatalf("attempt %d: backoff %v outside [%v, %v]", attempt, d, ceiling/2, ceiling)
		}
	}
}

func TestRetryable(t *testing.T) {
	for code, want := range map[connect.Code]bool{
		connect.CodeUnavailable: true, connect.CodeDeadlineExceeded: true, connect.CodeResourceExhausted: true,
		connect.CodeInternal: true, connect.CodeUnknown: true, connect.CodeAborted: true,
		connect.CodeInvalidArgument: false, connect.CodeUnauthenticated: false, connect.CodePermissionDenied: false,
		connect.CodeUnimplemented: false, connect.CodeFailedPrecondition: false, connect.CodeNotFound: false,
	} {
		if retryable(code) != want {
			t.Errorf("retryable(%v) = %v", code, !want)
		}
	}
}
