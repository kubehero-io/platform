// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package auth

import (
	"crypto/sha256"
	"sync"
	"time"
)

// tokenCache remembers cluster-token verdicts keyed by the token's
// SHA-256 (the raw token never sits in memory longer than the
// request). Positive and negative results get separate TTLs; the map
// is bounded and simply reset when full — verdicts are cheap to redo.
type tokenCache struct {
	mu      sync.Mutex
	posTTL  time.Duration
	negTTL  time.Duration
	max     int
	entries map[[32]byte]tokenVerdict
	nowFunc func() time.Time
}

type tokenVerdict struct {
	clusterID string
	ok        bool
	expires   time.Time
}

func newTokenCache(posTTL, negTTL time.Duration, max int) *tokenCache {
	return &tokenCache{
		posTTL:  posTTL,
		negTTL:  negTTL,
		max:     max,
		entries: make(map[[32]byte]tokenVerdict),
		nowFunc: time.Now,
	}
}

func (c *tokenCache) get(token string) (clusterID string, ok, cached bool) {
	k := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	v, found := c.entries[k]
	if !found || c.nowFunc().After(v.expires) {
		return "", false, false
	}
	return v.clusterID, v.ok, true
}

func (c *tokenCache) put(token, clusterID string, ok bool) {
	k := sha256.Sum256([]byte(token))
	ttl := c.negTTL
	if ok {
		ttl = c.posTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.entries = make(map[[32]byte]tokenVerdict)
	}
	c.entries[k] = tokenVerdict{clusterID: clusterID, ok: ok, expires: c.nowFunc().Add(ttl)}
}
