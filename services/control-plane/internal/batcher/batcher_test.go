// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package batcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a WriteFunc that remembers every batch it was given.
type recorder struct {
	mu      sync.Mutex
	batches [][]int
	fail    atomic.Int32 // fail this many more calls
	block   chan struct{}
}

func (r *recorder) write(ctx context.Context, rows []int) error {
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.fail.Load() > 0 {
		r.fail.Add(-1)
		return errors.New("boom")
	}
	r.mu.Lock()
	r.batches = append(r.batches, append([]int(nil), rows...))
	r.mu.Unlock()
	return nil
}

func (r *recorder) rows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, b := range r.batches {
		n += len(b)
	}
	return n
}

func seq(from, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = from + i
	}
	return out
}

func TestFlushOnSizeKeepsSegmentsWhole(t *testing.T) {
	rec := &recorder{}
	b := New(Config{Name: "t", FlushRows: 10, FlushInterval: time.Hour}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Run(ctx)

	for i := 0; i < 4; i++ {
		if err := b.Add(seq(i*4, 4)); err != nil {
			t.Fatal(err)
		}
	}
	// 16 rows pending, FlushRows 10: the size trigger writes whole
	// segments (4+4 = 8 fits, 12 would not) and leaves the rest for
	// the interval.
	deadline := time.Now().Add(2 * time.Second)
	for rec.rows() < 8 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	rec.mu.Lock()
	if len(rec.batches) == 0 || len(rec.batches[0]) != 8 {
		t.Fatalf("first batch = %v, want 8 rows (two whole segments)", rec.batches)
	}
	rec.mu.Unlock()
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.rows(); got != 16 {
		t.Fatalf("rows written = %d, want 16", got)
	}
	st := b.Stats()
	if st.Written != 16 || st.QueuedRows != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestFlushOnInterval(t *testing.T) {
	rec := &recorder{}
	b := New(Config{Name: "t", FlushRows: 1000, FlushInterval: 20 * time.Millisecond}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Run(ctx)
	if err := b.Add([]int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for rec.rows() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rec.rows() != 3 {
		t.Fatalf("interval flush did not write: %d rows", rec.rows())
	}
}

func TestBackpressureRowsAndBytes(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		sizeOf func(int) int
		first  int
		second int
		want   error
	}{
		{"rows fit", Config{FlushRows: 100, MaxQueueRows: 10}, nil, 5, 5, nil},
		{"rows overflow", Config{FlushRows: 100, MaxQueueRows: 10}, nil, 5, 6, ErrFull},
		{"bytes overflow", Config{FlushRows: 100, MaxQueueRows: 1000, MaxQueueBytes: 100}, func(int) int { return 10 }, 5, 6, ErrFull},
		{"bytes fit", Config{FlushRows: 100, MaxQueueRows: 1000, MaxQueueBytes: 100}, func(int) int { return 10 }, 5, 5, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			tc.cfg.FlushInterval = time.Hour
			b := New(tc.cfg, rec.write, tc.sizeOf) // not running: nothing drains
			if err := b.Add(seq(0, tc.first)); err != nil {
				t.Fatal(err)
			}
			if err := b.Add(seq(0, tc.second)); !errors.Is(err, tc.want) {
				t.Fatalf("second add err = %v, want %v", err, tc.want)
			}
			if tc.want != nil && b.Stats().Rejected != uint64(tc.second) {
				t.Fatalf("rejected = %d, want %d", b.Stats().Rejected, tc.second)
			}
		})
	}
}

func TestInFlightRowsCountTowardsBound(t *testing.T) {
	rec := &recorder{block: make(chan struct{})}
	b := New(Config{FlushRows: 5, MaxQueueRows: 8, FlushInterval: time.Hour}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Run(ctx)
	if err := b.Add(seq(0, 5)); err != nil { // triggers a write that blocks
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		inflight := b.inflight
		b.mu.Unlock()
		if inflight == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := b.Add(seq(0, 4)); !errors.Is(err, ErrFull) {
		t.Fatalf("add while 5 rows in flight: err = %v, want ErrFull", err)
	}
	close(rec.block)
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(seq(0, 4)); err != nil {
		t.Fatalf("add after the write finished: %v", err)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	rec := &recorder{}
	rec.fail.Store(2)
	b := New(Config{FlushRows: 10, FlushInterval: time.Hour, Retries: 3, RetryBackoff: time.Millisecond}, rec.write, nil)
	if err := b.Add([]int{1}); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := b.Stats()
	if st.Written != 1 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want written=1 failed=0", st)
	}
}

func TestAsyncFailureIsCounted(t *testing.T) {
	rec := &recorder{}
	rec.fail.Store(100)
	b := New(Config{FlushRows: 10, FlushInterval: time.Hour, Retries: 1, RetryBackoff: time.Millisecond}, rec.write, nil)
	if err := b.Add([]int{1, 2}); err != nil {
		t.Fatal(err)
	}
	_ = b.Flush(context.Background())
	st := b.Stats()
	if st.Failed != 2 || st.Written != 0 || st.LastError == "" || st.QueuedRows != 0 {
		t.Fatalf("stats = %+v, want failed=2 and an error", st)
	}
}

func TestAddWaitGroupCommit(t *testing.T) {
	rec := &recorder{}
	b := New(Config{FlushRows: 1000, FlushInterval: 30 * time.Millisecond}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Run(ctx)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- b.AddWait(context.Background(), seq(i*10, 10))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if rec.rows() != 80 {
		t.Fatalf("rows = %d, want 80", rec.rows())
	}
	rec.mu.Lock()
	n := len(rec.batches)
	rec.mu.Unlock()
	if n >= 8 {
		t.Fatalf("group commit wrote %d batches for 8 concurrent callers; want them combined", n)
	}
}

func TestAddWaitReturnsWriteError(t *testing.T) {
	rec := &recorder{}
	rec.fail.Store(100)
	b := New(Config{FlushRows: 10, FlushInterval: 5 * time.Millisecond, Retries: -1}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Run(ctx)
	if err := b.AddWait(context.Background(), []int{1}); err == nil {
		t.Fatal("want the write error")
	}
	if st := b.Stats(); st.Failed != 0 {
		// Group-commit failures are reported to the caller, not
		// counted as silently lost.
		t.Fatalf("failed = %d, want 0", st.Failed)
	}
}

func TestShutdownDrainsAndRejects(t *testing.T) {
	rec := &recorder{}
	b := New(Config{FlushRows: 1000, FlushInterval: time.Hour}, rec.write, nil)
	ctx, cancel := context.WithCancel(context.Background())
	b.Run(ctx)
	if err := b.Add(seq(0, 7)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("batcher did not drain")
	}
	if rec.rows() != 7 {
		t.Fatalf("drained %d rows, want 7", rec.rows())
	}
	if err := b.Add([]int{1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("add after shutdown: %v, want ErrClosed", err)
	}
}
