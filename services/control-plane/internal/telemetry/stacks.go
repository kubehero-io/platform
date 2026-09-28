// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package telemetry

import (
	"sync"
	"time"

	"github.com/go-faster/city"
)

// StackHash content-addresses a stack: ClickHouse's cityHash64 of the
// frames joined by NUL (frames never contain NUL — ingest strips it —
// so the join is unambiguous). Because go-faster/city's CH64 is
// byte-for-byte ClickHouse's cityHash64, the same id can be recomputed
// in SQL: cityHash64(arrayStringConcat(frames, char(0))).
func StackHash(frames []string) uint64 {
	n := len(frames)
	for _, f := range frames {
		n += len(f)
	}
	buf := make([]byte, 0, n)
	for i, f := range frames {
		if i > 0 {
			buf = append(buf, 0)
		}
		buf = append(buf, f...)
	}
	return city.CH64(buf)
}

// StackCache remembers which stacks were written recently so the hot
// stacks a service emits every few seconds are not rewritten into
// profile_stacks each time. It is a two-generation approximation of an
// LRU — bounded at max entries, O(1), no per-entry list pointers.
//
// Entries expire after Refresh so a stack that stays hot keeps having
// its last_seen bumped: profile_stacks has a TTL on last_seen, and a
// stack must outlive every sample that references it.
type StackCache struct {
	max     int
	refresh time.Duration

	mu       sync.Mutex
	cur, old map[uint64]int64 // hash → unix seconds when written
}

// NewStackCache returns a cache holding up to max hashes; refresh is
// how long a written stack is trusted before it is written again.
func NewStackCache(max int, refresh time.Duration) *StackCache {
	if max < 2 {
		max = 2
	}
	return &StackCache{max: max, refresh: refresh, cur: make(map[uint64]int64), old: map[uint64]int64{}}
}

// ShouldWrite reports whether the stack must be written now, and if so
// records it as written.
func (c *StackCache) ShouldWrite(h uint64, now time.Time) bool {
	ts := now.Unix()
	c.mu.Lock()
	defer c.mu.Unlock()
	written, ok := c.cur[h]
	if !ok {
		if written, ok = c.old[h]; ok {
			c.put(h, written) // promote
		}
	}
	if ok && ts-written < int64(c.refresh/time.Second) {
		return false
	}
	c.put(h, ts)
	return true
}

// Forget drops hashes whose write failed so they are written again on
// their next sighting.
func (c *StackCache) Forget(hashes []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range hashes {
		delete(c.cur, h)
		delete(c.old, h)
	}
}

// Len is the number of remembered hashes (both generations).
func (c *StackCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.cur) + len(c.old)
}

func (c *StackCache) put(h uint64, ts int64) {
	if len(c.cur) >= c.max/2 {
		c.old, c.cur = c.cur, make(map[uint64]int64, c.max/2)
	}
	c.cur[h] = ts
}
