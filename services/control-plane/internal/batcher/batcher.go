// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package batcher turns many small writes into few large ones.
//
// ClickHouse wants inserts of thousands of rows at most about once a
// second per table; the collectors send small batches every few seconds
// from every node. A Batcher sits between the two: RPC handlers Add
// their rows and return immediately, and a background worker writes
// everything pending when enough rows have accumulated or the flush
// interval elapses.
//
// Two admission modes share one queue:
//
//   - Add (async): the caller's rows are acknowledged as soon as they
//     are queued. A write that still fails after retries is logged and
//     counted in Stats.Failed — the only way accepted rows can be lost,
//     so it is loud.
//   - AddWait (group commit): the caller blocks until the batch holding
//     its rows is written and gets that write's error. Concurrent
//     callers share one insert, so it batches without ever
//     acknowledging rows that are not stored.
//
// The queue is bounded by rows and bytes (pending + in flight); when a
// batch would exceed either bound Add returns ErrFull so the RPC layer
// can answer ResourceExhausted and the client backs off. When the Run
// context ends the batcher stops admitting rows (ErrClosed), writes
// everything still queued within DrainTimeout, and closes Done.
package batcher

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrFull means admitting the rows would exceed the queue bounds.
	ErrFull = errors.New("ingest queue full")
	// ErrClosed means the batcher is draining for shutdown.
	ErrClosed = errors.New("ingest queue closed (shutting down)")
)

// WriteFunc stores one batch. It must be safe for concurrent use when
// Workers > 1.
type WriteFunc[T any] func(ctx context.Context, rows []T) error

// Config tunes one batcher. Zero values take the documented defaults.
type Config struct {
	// Name labels logs and stats (usually the table name).
	Name string
	// FlushRows starts a write as soon as this many rows are pending,
	// and caps rows per write (a single Add is never split, so one
	// write can exceed it by at most one Add's rows). Default 50 000.
	FlushRows int
	// FlushInterval bounds how long a row waits before it is written.
	// Default 1s.
	FlushInterval time.Duration
	// MaxQueueRows / MaxQueueBytes bound pending + in-flight rows.
	// Defaults: 20 × FlushRows rows; bytes unbounded unless SizeOf is
	// set, then 128 MiB.
	MaxQueueRows  int
	MaxQueueBytes int64
	// Workers is the number of concurrent writers. Default 1.
	Workers int
	// WriteTimeout bounds one write attempt. Default 30s.
	WriteTimeout time.Duration
	// Retries is how many times a failed write is retried, with
	// exponential backoff from RetryBackoff (default 250ms).
	// Default 3; negative means none.
	Retries      int
	RetryBackoff time.Duration
	// DrainTimeout bounds the final flush after Run's context ends.
	// Default 10s.
	DrainTimeout time.Duration
	Log          *slog.Logger
}

func (c Config) withDefaults(sized bool) Config {
	if c.FlushRows <= 0 {
		c.FlushRows = 50_000
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = time.Second
	}
	if c.MaxQueueRows <= 0 {
		c.MaxQueueRows = 20 * c.FlushRows
	}
	if c.MaxQueueBytes <= 0 && sized {
		c.MaxQueueBytes = 128 << 20
	}
	if c.Workers <= 0 {
		c.Workers = 1
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.Retries == 0 {
		c.Retries = 3
	} else if c.Retries < 0 {
		c.Retries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 250 * time.Millisecond
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 10 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return c
}

// Stats is a point-in-time snapshot of one batcher's counters.
type Stats struct {
	Name        string
	QueuedRows  int
	QueuedBytes int64
	Written     uint64 // rows stored
	Batches     uint64 // successful writes
	Failed      uint64 // rows lost after all retries
	Rejected    uint64 // rows refused at admission (full / closed)
	LastError   string
}

// segment is one Add call's rows; it is never split across writes so a
// group-commit waiter gets exactly one result.
type segment[T any] struct {
	rows  []T
	bytes int64
	done  chan error // nil for async adds
}

// Batcher buffers rows of one table. Create with New, start with Run.
type Batcher[T any] struct {
	cfg    Config
	write  WriteFunc[T]
	sizeOf func(T) int

	mu          sync.Mutex
	pending     []segment[T]
	pendingRows int
	queuedRows  int   // pending + in flight
	queuedBytes int64 // pending + in flight
	inflight    int   // writes in progress
	closed      bool
	lastErr     string

	wake chan struct{}
	done chan struct{}

	written  atomic.Uint64
	batches  atomic.Uint64
	failed   atomic.Uint64
	rejected atomic.Uint64
}

// New returns a batcher writing through w. sizeOf (optional) estimates
// a row's memory so MaxQueueBytes can bound large rows such as log
// lines.
func New[T any](cfg Config, w WriteFunc[T], sizeOf func(T) int) *Batcher[T] {
	return &Batcher[T]{
		cfg:    cfg.withDefaults(sizeOf != nil),
		write:  w,
		sizeOf: sizeOf,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// Run starts the workers; it returns immediately. When ctx ends the
// batcher drains and then closes Done.
func (b *Batcher[T]) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < b.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.loop(ctx)
		}()
	}
	go func() {
		wg.Wait()
		b.drain()
		close(b.done)
	}()
}

// Done is closed once the batcher has drained after shutdown.
func (b *Batcher[T]) Done() <-chan struct{} { return b.done }

// Add queues rows and returns without waiting for the write.
func (b *Batcher[T]) Add(rows []T) error {
	_, err := b.enqueue(rows, false)
	return err
}

// AddWait queues rows and blocks until the batch containing them has
// been written, returning that write's error. If ctx ends first the
// rows stay queued (they will still be written) and ctx.Err() is
// returned.
func (b *Batcher[T]) AddWait(ctx context.Context, rows []T) error {
	done, err := b.Submit(rows)
	if err != nil {
		return err
	}
	return Wait(ctx, done)
}

// Submit queues rows like AddWait but returns at once; the channel
// receives the result of the write that carries them. Use it to wait
// on several batchers concurrently (see Wait).
func (b *Batcher[T]) Submit(rows []T) (<-chan error, error) {
	done, err := b.enqueue(rows, true)
	if err != nil {
		return nil, err
	}
	if done == nil { // nothing to write
		ok := make(chan error, 1)
		ok <- nil
		return ok, nil
	}
	return done, nil
}

// Wait blocks until done yields a result or ctx ends.
func Wait(ctx context.Context, done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Batcher[T]) enqueue(rows []T, wait bool) (chan error, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var size int64
	if b.sizeOf != nil {
		for i := range rows {
			size += int64(b.sizeOf(rows[i]))
		}
	}
	seg := segment[T]{rows: rows, bytes: size}
	if wait {
		seg.done = make(chan error, 1)
	}

	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		b.rejected.Add(uint64(len(rows)))
		return nil, ErrClosed
	case b.queuedRows+len(rows) > b.cfg.MaxQueueRows,
		b.cfg.MaxQueueBytes > 0 && b.queuedBytes+size > b.cfg.MaxQueueBytes:
		b.mu.Unlock()
		b.rejected.Add(uint64(len(rows)))
		return nil, ErrFull
	}
	b.pending = append(b.pending, seg)
	b.pendingRows += len(rows)
	b.queuedRows += len(rows)
	b.queuedBytes += size
	full := b.pendingRows >= b.cfg.FlushRows
	b.mu.Unlock()

	if full {
		b.signal()
	}
	return seg.done, nil
}

func (b *Batcher[T]) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Batcher[T]) loop(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()
	// A write already in progress when shutdown starts must finish, not
	// be cancelled half-way: writes get a context that ignores ctx's
	// cancellation (each attempt still has WriteTimeout).
	wctx := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.closed = true
			b.mu.Unlock()
			return
		case <-b.wake:
			// Only full batches: a partial one keeps accumulating until
			// the ticker, which is what keeps inserts large.
			for ctx.Err() == nil && b.flushOne(wctx, true) {
			}
		case <-ticker.C:
			for ctx.Err() == nil && b.flushOne(wctx, false) {
			}
		}
	}
}

// take removes up to FlushRows rows (whole segments) from the queue.
// With onlyFull it takes nothing unless a full batch is pending.
func (b *Batcher[T]) take(onlyFull bool) []segment[T] {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 || (onlyFull && b.pendingRows < b.cfg.FlushRows) {
		return nil
	}
	n, rows := 0, 0
	for n < len(b.pending) && (n == 0 || rows+len(b.pending[n].rows) <= b.cfg.FlushRows) {
		rows += len(b.pending[n].rows)
		n++
	}
	segs := make([]segment[T], n)
	copy(segs, b.pending[:n])
	// Shift instead of reslicing so the backing array doesn't pin
	// written rows in memory.
	rest := copy(b.pending, b.pending[n:])
	clear(b.pending[rest:])
	b.pending = b.pending[:rest]
	b.pendingRows -= rows
	b.inflight++
	if b.pendingRows >= b.cfg.FlushRows {
		// Another full batch is ready: let a sibling worker take it.
		defer b.signal()
	}
	return segs
}

// flushOne writes one batch; it reports whether it wrote anything.
func (b *Batcher[T]) flushOne(ctx context.Context, onlyFull bool) bool {
	segs := b.take(onlyFull)
	if segs == nil {
		return false
	}
	b.writeSegments(ctx, segs)
	return true
}

func (b *Batcher[T]) writeSegments(ctx context.Context, segs []segment[T]) {
	total := 0
	var bytes int64
	for _, s := range segs {
		total += len(s.rows)
		bytes += s.bytes
	}
	rows := segs[0].rows
	if len(segs) > 1 {
		rows = make([]T, 0, total)
		for _, s := range segs {
			rows = append(rows, s.rows...)
		}
	}

	err := b.writeWithRetry(ctx, rows)

	b.mu.Lock()
	b.inflight--
	b.queuedRows -= total
	b.queuedBytes -= bytes
	if err != nil {
		b.lastErr = err.Error()
	}
	b.mu.Unlock()

	async := 0
	for _, s := range segs {
		if s.done != nil {
			s.done <- err
		} else {
			async += len(s.rows)
		}
	}
	if err != nil {
		if async > 0 {
			b.failed.Add(uint64(async))
			b.cfg.Log.Error("ingest write failed after retries; accepted rows lost",
				"table", b.cfg.Name, "rows", async, "err", err)
		}
		return
	}
	b.written.Add(uint64(total))
	b.batches.Add(1)
}

func (b *Batcher[T]) writeWithRetry(ctx context.Context, rows []T) error {
	backoff := b.cfg.RetryBackoff
	var err error
	for attempt := 0; ; attempt++ {
		wctx, cancel := context.WithTimeout(ctx, b.cfg.WriteTimeout)
		err = b.write(wctx, rows)
		cancel()
		if err == nil || attempt >= b.cfg.Retries || ctx.Err() != nil {
			return err
		}
		b.cfg.Log.Warn("ingest write failed, retrying",
			"table", b.cfg.Name, "rows", len(rows), "attempt", attempt+1, "in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// drain writes everything still queued after the workers stopped.
// It uses a fresh context: the Run context is already cancelled.
func (b *Batcher[T]) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.DrainTimeout)
	defer cancel()
	for b.flushOne(ctx, false) {
	}
}

// Flush writes everything pending now and waits until no write is in
// flight. Used by tests and by callers that need read-your-writes.
func (b *Batcher[T]) Flush(ctx context.Context) error {
	for b.flushOne(ctx, false) {
	}
	for {
		b.mu.Lock()
		idle := b.inflight == 0 && len(b.pending) == 0
		b.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		for b.flushOne(ctx, false) {
		}
	}
}

// Stats returns a snapshot of the counters.
func (b *Batcher[T]) Stats() Stats {
	b.mu.Lock()
	s := Stats{
		Name:        b.cfg.Name,
		QueuedRows:  b.queuedRows,
		QueuedBytes: b.queuedBytes,
		LastError:   b.lastErr,
	}
	b.mu.Unlock()
	s.Written = b.written.Load()
	s.Batches = b.batches.Load()
	s.Failed = b.failed.Load()
	s.Rejected = b.rejected.Load()
	return s
}
