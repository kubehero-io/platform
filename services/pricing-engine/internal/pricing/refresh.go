// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Refresher keeps popular SKUs warm in a Catalog. It is the in-process
// stand-in for the "nightly cron" from ARCHITECTURE.md §3: a plain
// ticker goroutine with jitter, cancelled via context on shutdown.
type Refresher struct {
	Catalog  *Catalog
	Interval time.Duration // <= 0 disables the loop entirely
	Jitter   time.Duration // random extra delay added to each tick
	Targets  []QuoteKey    // usually Catalog.PopularSKUs()
	Log      *slog.Logger
}

// Run prefetches every target once, then re-resolves them every
// Interval (+ up to Jitter). Blocks until ctx is cancelled; call it in
// a goroutine.
func (r *Refresher) Run(ctx context.Context) {
	if r.Interval <= 0 || r.Catalog == nil {
		return
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("pricing refresh loop started",
		"interval", r.Interval.String(), "jitter", r.Jitter.String(), "targets", len(r.Targets))

	r.refresh(ctx, log)
	timer := time.NewTimer(r.next())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("pricing refresh loop stopped")
			return
		case <-timer.C:
			r.refresh(ctx, log)
			timer.Reset(r.next())
		}
	}
}

func (r *Refresher) next() time.Duration {
	d := r.Interval
	if r.Jitter > 0 {
		d += rand.N(r.Jitter)
	}
	return d
}

func (r *Refresher) refresh(ctx context.Context, log *slog.Logger) {
	var ok, failed int
	for _, key := range r.Targets {
		if ctx.Err() != nil {
			return
		}
		if _, err := r.Catalog.Refresh(ctx, key); err != nil {
			failed++
			log.Warn("pricing refresh failed", "key", key.String(), "err", err)
			continue
		}
		ok++
	}
	log.Info("pricing refresh pass complete", "ok", ok, "failed", failed)
}
