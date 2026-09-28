// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ship

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// Outcome is how a batch left the queue.
type Outcome int

const (
	// Sent: the control plane accepted the call (it may still have
	// dropped individual rows; those are counted as reason "server").
	Sent Outcome = iota
	// Dropped: given up by policy — queue overflow, a non-retryable
	// rejection, or retries exhausted. The data is gone.
	Dropped
	// Abandoned: still queued when the collector shut down. Producers
	// that can replay (the log tailer re-reads from its checkpoint)
	// should not treat this data as done.
	Abandoned
)

// Batch is one request waiting to ship.
type Batch[Req any] struct {
	Req   Req
	Items int // rows in the request, for accounting
	Bytes int // proto.Size(Req), for the byte budget
	// Done, when set, is called exactly once when the batch leaves the
	// queue. It runs on the queue's goroutine and must not block.
	Done func(Outcome)
}

// Sender performs one RPC and reports (accepted, dropped-by-server).
type Sender[Req any] func(ctx context.Context, req Req) (accepted, dropped int, err error)

// QueueConfig bounds a queue.
type QueueConfig struct {
	// Signal names the queue in metrics and logs (cost, logs, …).
	Signal string
	// MaxItems / MaxBytes cap what may wait in memory; the oldest queued
	// batches are dropped beyond either. Zero means 10k items / 16 MiB.
	MaxItems int
	MaxBytes int
	// Timeout bounds each call. Zero means 10s.
	Timeout time.Duration
	// MaxAttempts bounds retries of one batch (retryable errors only).
	// Zero means 8 (~1 minute with the default backoff).
	MaxAttempts int
	// InitialBackoff / MaxBackoff shape the exponential backoff; each
	// sleep is jittered to [d/2, d) so a fleet of collectors that lost
	// the control plane together doesn't reconnect in lockstep.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

func (c *QueueConfig) defaults() {
	if c.MaxItems <= 0 {
		c.MaxItems = 10_000
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 16 << 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.InitialBackoff <= 0 {
		c.InitialBackoff = 500 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 30 * time.Second
	}
}

// Queue is a bounded FIFO of batches drained by one sender goroutine.
type Queue[Req any] struct {
	cfg  QueueConfig
	send Sender[Req]
	log  *slog.Logger

	mu      sync.Mutex
	pending []*Batch[Req]
	items   int
	bytes   int
	notify  chan struct{}
	sleep   func(ctx context.Context, d time.Duration) bool
	now     func() time.Time
	lastLog map[string]time.Time
	failing bool

	sent, queueItems, queueBytes, lastOK, payload *metrics.Value
}

// NewQueue builds a queue; call Run to start shipping.
func NewQueue[Req any](cfg QueueConfig, send Sender[Req], log *slog.Logger) *Queue[Req] {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	return &Queue[Req]{
		cfg:        cfg,
		send:       send,
		log:        log.With("signal", cfg.Signal),
		notify:     make(chan struct{}, 1),
		sleep:      sleepCtx,
		now:        time.Now,
		lastLog:    map[string]time.Time{},
		sent:       metrics.ItemsSent.With(cfg.Signal),
		queueItems: metrics.QueueItems.With(cfg.Signal),
		queueBytes: metrics.QueueBytes.With(cfg.Signal),
		lastOK:     metrics.LastSuccess.With(cfg.Signal),
		payload:    metrics.PayloadBytes.With(cfg.Signal),
	}
}

// Enqueue adds a batch without blocking. If the batch doesn't fit the
// budget, the oldest queued batches are dropped until it does; a batch
// larger than the whole budget is dropped itself.
func (q *Queue[Req]) Enqueue(b Batch[Req]) {
	if b.Items <= 0 && b.Bytes <= 0 {
		finish(&b, Sent)
		return
	}
	q.mu.Lock()
	var victims []*Batch[Req]
	if b.Items > q.cfg.MaxItems || b.Bytes > q.cfg.MaxBytes {
		q.mu.Unlock()
		q.drop(&b, "queue_full")
		return
	}
	for len(q.pending) > 0 && (q.items+b.Items > q.cfg.MaxItems || q.bytes+b.Bytes > q.cfg.MaxBytes) {
		v := q.pending[0]
		q.pending[0] = nil
		q.pending = q.pending[1:]
		q.items -= v.Items
		q.bytes -= v.Bytes
		victims = append(victims, v)
	}
	q.pending = append(q.pending, &b)
	q.items += b.Items
	q.bytes += b.Bytes
	q.gauges()
	q.mu.Unlock()

	for _, v := range victims {
		q.drop(v, "queue_full")
	}
	if len(victims) > 0 {
		q.logRateLimited("overflow", slog.LevelWarn, "ship queue full — dropping oldest batches (control plane unreachable or too slow)",
			"dropped_batches", len(victims))
	}
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Len reports queued (not in-flight) items and bytes.
func (q *Queue[Req]) Len() (items, bytes int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items, q.bytes
}

// Run ships batches until ctx is cancelled. A batch interrupted by
// cancellation is put back at the head so Flush can still send it.
func (q *Queue[Req]) Run(ctx context.Context) {
	for {
		b := q.pop()
		if b == nil {
			select {
			case <-ctx.Done():
				return
			case <-q.notify:
				continue
			}
		}
		if !q.deliver(ctx, b, q.cfg.MaxAttempts, Dropped) {
			q.pushFront(b)
			return
		}
	}
}

// Flush makes one attempt at every queued batch until ctx expires
// (shutdown path), then abandons whatever is left.
func (q *Queue[Req]) Flush(ctx context.Context) {
	for ctx.Err() == nil {
		b := q.pop()
		if b == nil {
			return
		}
		// A failed last attempt abandons rather than drops: the
		// producer may replay it after restart (log checkpoints).
		if !q.deliver(ctx, b, 1, Abandoned) {
			q.pushFront(b)
			break
		}
	}
	for b := q.pop(); b != nil; b = q.pop() {
		q.count(b, "shutdown")
		finish(b, Abandoned)
	}
}

// deliver sends one batch with retries. When retryable attempts run
// out, the batch ends as `exhausted` (Dropped in normal operation,
// Abandoned during the shutdown flush). It returns false only when ctx
// was cancelled before the batch reached a final outcome.
func (q *Queue[Req]) deliver(ctx context.Context, b *Batch[Req], attempts int, exhausted Outcome) bool {
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		cctx, cancel := context.WithTimeout(ctx, q.cfg.Timeout)
		accepted, serverDropped, err := q.send(cctx, b.Req)
		cancel()
		if err == nil {
			q.onSuccess(b, accepted, serverDropped)
			return true
		}
		if ctx.Err() != nil {
			// Shutting down mid-call: not the server's fault; retry in Flush.
			return false
		}
		code := connect.CodeOf(err)
		metrics.SendErrors.With(q.cfg.Signal, codeLabel(code)).Inc()
		q.failing = true
		if !retryable(code) {
			q.logRateLimited("reject-"+code.String(), slog.LevelError, rejectMessage(code), "code", code.String(), "err", err)
			q.drop(b, "rejected")
			return true
		}
		if attempt >= attempts {
			if exhausted == Abandoned {
				q.count(b, "shutdown")
				finish(b, Abandoned)
				return true
			}
			q.logRateLimited("exhausted", slog.LevelWarn, "control plane unavailable — dropping batch after retries",
				"attempts", attempt, "code", code.String(), "err", err)
			q.drop(b, "retries_exhausted")
			return true
		}
		if !q.sleep(ctx, q.backoff(attempt)) {
			return false
		}
	}
}

func (q *Queue[Req]) onSuccess(b *Batch[Req], accepted, serverDropped int) {
	q.sent.Add(float64(accepted))
	if serverDropped > 0 {
		metrics.ItemsDropped.With(q.cfg.Signal, "server").Add(float64(serverDropped))
	}
	q.payload.Add(float64(b.Bytes))
	q.lastOK.Set(float64(q.now().UnixNano()) / 1e9)
	if q.failing {
		q.failing = false
		q.log.Info("control plane reachable again")
	}
	finish(b, Sent)
}

func (q *Queue[Req]) backoff(attempt int) time.Duration {
	d := q.cfg.InitialBackoff << min(attempt-1, 20)
	if d <= 0 || d > q.cfg.MaxBackoff {
		d = q.cfg.MaxBackoff
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func (q *Queue[Req]) pop() *Batch[Req] {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	b := q.pending[0]
	q.pending[0] = nil
	q.pending = q.pending[1:]
	q.items -= b.Items
	q.bytes -= b.Bytes
	q.gauges()
	return b
}

func (q *Queue[Req]) pushFront(b *Batch[Req]) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append([]*Batch[Req]{b}, q.pending...)
	q.items += b.Items
	q.bytes += b.Bytes
	q.gauges()
}

func (q *Queue[Req]) gauges() {
	q.queueItems.Set(float64(q.items))
	q.queueBytes.Set(float64(q.bytes))
}

func (q *Queue[Req]) drop(b *Batch[Req], reason string) {
	q.count(b, reason)
	finish(b, Dropped)
}

func (q *Queue[Req]) count(b *Batch[Req], reason string) {
	metrics.ItemsDropped.With(q.cfg.Signal, reason).Add(float64(b.Items))
}

func (q *Queue[Req]) logRateLimited(key string, level slog.Level, msg string, args ...any) {
	q.mu.Lock()
	now := q.now()
	last, seen := q.lastLog[key]
	if seen && now.Sub(last) < time.Minute {
		q.mu.Unlock()
		return
	}
	q.lastLog[key] = now
	q.mu.Unlock()
	q.log.Log(context.Background(), level, msg, args...)
}

func finish[Req any](b *Batch[Req], o Outcome) {
	if b.Done != nil {
		b.Done(o)
		b.Done = nil
	}
}

// retryable reports whether a failed call may succeed if repeated.
// Internal is included: the control plane maps storage write failures
// to it, and ingest is idempotent, so a retry is the right call.
func retryable(code connect.Code) bool {
	switch code {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted,
		connect.CodeAborted, connect.CodeInternal, connect.CodeUnknown:
		return true
	}
	return false
}

func rejectMessage(code connect.Code) string {
	switch code {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return "control plane rejected the collector's credentials — check CONTROL_PLANE_TOKEN (needs member role or above)"
	case connect.CodeUnimplemented:
		return "control plane does not implement this RPC (older control plane?) — dropping this signal's batches"
	}
	return "control plane rejected a batch — dropping it"
}

func codeLabel(c connect.Code) string {
	s := c.String()
	if s == "" {
		return "unknown"
	}
	return strings.ToLower(s)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ErrNoSink is returned by producers that have nowhere to send.
var ErrNoSink = errors.New("no control plane configured")
