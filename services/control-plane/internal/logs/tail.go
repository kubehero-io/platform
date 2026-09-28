// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"hash/fnv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
)

// TailParams starts a live tail.
type TailParams struct {
	Query     string
	Start     time.Time // resume point; zero = now - 10s
	ClusterID string
}

// TailBatch is one push: new lines (oldest first) and how many lines
// were skipped because the tail fell too far behind.
type TailBatch struct {
	Lines   []Line
	Dropped int64
}

type tailKey struct {
	ts   int64
	pod  string
	body uint64
}

// maxTailLag is how far behind real time a tail may fall before it
// skips ahead (and reports what it skipped).
const maxTailLag = 30 * time.Second

// Tail polls for new matching lines every TailPoll and calls send with
// each batch until ctx ends (returns nil), the tail has run for
// TailMaxDuration (returns nil — clients reconnect), or send fails.
//
// Lines can land in ClickHouse a little after their timestamp (agent
// and ingest batching), so every poll re-reads the last TailOverlap
// before the cursor and drops lines it already sent, keyed on
// (ts, pod, body hash).
func (e *Engine) Tail(ctx context.Context, p TailParams, send func(TailBatch) error) error {
	if err := e.ready(); err != nil {
		return err
	}
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return badRequest("query is required")
	}
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return err
	}
	if err := scopeCluster(sel, p.ClusterID); err != nil {
		return err
	}
	now := e.now()
	cursor := p.Start
	if cursor.IsZero() {
		cursor = now.Add(-10 * time.Second)
	}
	if now.Sub(cursor) > time.Hour {
		return badRequest("tail can resume at most 1h back")
	}
	plan := logql.PlanSelector(sel)
	pipe := logql.NewPipeline(plan.Stages)
	overlap := int64(e.opts.TailOverlap)
	sent := map[tailKey]struct{}{}
	deadline := time.NewTimer(e.opts.TailMaxDuration)
	defer deadline.Stop()
	ticker := time.NewTicker(e.opts.TailPoll)
	defer ticker.Stop()

	cur := cursor.UnixNano()
	full := false
	for {
		// After a full poll the tail is catching up on a burst: resume at
		// the cursor itself, since re-reading the overlap (all already
		// sent) would eat the whole per-poll budget.
		back := overlap
		if full {
			back = 0
		}
		batch, next, scanned, err := e.tailPoll(ctx, sel, plan, pipe, cur, back, sent)
		full = scanned >= e.opts.TailMaxPerPoll
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		cur = next
		for len(batch.Lines) > 0 || batch.Dropped > 0 {
			chunk := batch
			if len(chunk.Lines) > e.opts.TailMaxPerPush {
				chunk.Lines = batch.Lines[:e.opts.TailMaxPerPush]
			}
			if err := send(chunk); err != nil {
				return err
			}
			batch.Lines = batch.Lines[len(chunk.Lines):]
			batch.Dropped = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-ticker.C:
		}
	}
}

// tailPoll reads [cursor - back, now + skew) once and returns the new
// lines, the next cursor and how many rows it read.
func (e *Engine) tailPoll(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, pipe *logql.Pipeline,
	cursor, back int64, sent map[tailKey]struct{}) (TailBatch, int64, int, error) {
	var batch TailBatch
	overlap := int64(e.opts.TailOverlap)
	now := e.now().UnixNano()
	if lag := now - cursor; lag > int64(maxTailLag) {
		// Fell behind: count what is skipped and jump ahead.
		skipTo := now - int64(e.opts.TailOverlap)
		n, err := e.store.Count(ctx, plan, sel, cursor+1, skipTo)
		if err != nil {
			return batch, cursor, 0, err
		}
		batch.Dropped = n
		cursor = skipTo
		back = overlap
	}
	from := cursor - back + 1
	if back == 0 {
		from = cursor // lines sharing the cursor's timestamp; sent ones are deduped
	}
	// Lines stamped slightly in the future (skewed node clocks, which
	// ingest accepts) are shown when they arrive; they never move the
	// cursor past now.
	to := now + int64(clickhouse.MaxFutureSkew)
	max := e.opts.TailMaxPerPoll
	scanned := 0
	next := cursor
	_, err := e.store.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true, Max: max}, func(r Row) bool {
		scanned++
		k := tailKey{ts: r.TS, pod: r.Labels["pod"], body: hashString(r.Body)}
		if _, dup := sent[k]; dup {
			return true
		}
		sent[k] = struct{}{}
		if r.TS > next && r.TS <= now {
			next = r.TS
		}
		line, labels, ok := pipe.Process(r.TS, r.Body, r.Labels)
		if ok {
			batch.Lines = append(batch.Lines, Line{TS: r.TS, Body: line, Labels: logql.SeriesLabels(labels, nil), TraceID: r.TraceID})
		}
		return true
	})
	if err != nil {
		return batch, cursor, scanned, err
	}
	if scanned < max && next < now-overlap {
		// Nothing newer than the cursor arrived: advance to "now minus
		// overlap" so an idle stream does not re-read an ever-growing
		// window.
		next = now - overlap
	}
	// Forget sent keys that can no longer be re-read.
	floor := next - overlap
	for k := range sent {
		if k.ts <= floor {
			delete(sent, k)
		}
	}
	if len(sent) > 200_000 { // pathological burst on one timestamp window
		clear(sent)
	}
	return batch, next, scanned, nil
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
