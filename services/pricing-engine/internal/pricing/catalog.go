// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultCacheTTL is how long a resolved quote stays fresh when the
// caller doesn't configure a TTL (PRICING_CACHE_TTL / --cache-ttl).
const DefaultCacheTTL = 12 * time.Hour

// QuoteKey identifies one cacheable price point.
type QuoteKey struct {
	Cloud     Cloud
	SKU       string
	Region    string
	Lifecycle string
}

func (k QuoteKey) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", k.Cloud, k.SKU, k.Region, k.Lifecycle)
}

type cacheEntry struct {
	quote   Quote
	fetched time.Time
}

// flight is one in-progress resolution shared by every concurrent
// caller asking for the same key (single-flight de-duplication).
type flight struct {
	done chan struct{}
	q    Quote
	err  error
}

// Catalog is the serving-path price resolver. Resolution order:
//
//  1. In-memory cache (fresh within TTL).
//  2. Live cloud source, de-duplicated per key so concurrent misses
//     don't stampede a provider.
//  3. On live failure: the stale cache entry if one exists (a real
//     provider price, just old), then the Static table.
//
// A provider outage never fails a Quote the static table can answer;
// the live error only surfaces when every fallback misses too.
//
// Fallback results are cached with the full TTL as well — that keeps a
// flapping provider from being hammered on every request; the
// Refresher re-resolves popular SKUs in the background.
type Catalog struct {
	live   map[Cloud]Source
	static map[Cloud]*Static
	ttl    time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	entries map[QuoteKey]cacheEntry
	flights map[QuoteKey]*flight
}

// NewCatalog wires every cloud's static fallback, mirroring
// NewDefaultRegistry. Live sources are added via WithLive. A ttl <= 0
// selects DefaultCacheTTL; a nil log selects slog.Default().
func NewCatalog(ttl time.Duration, log *slog.Logger) *Catalog {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Catalog{
		live: map[Cloud]Source{},
		static: map[Cloud]*Static{
			CloudAWS:   NewStatic(CloudAWS),
			CloudGCP:   NewStatic(CloudGCP),
			CloudAzure: NewStatic(CloudAzure),
		},
		ttl:     ttl,
		log:     log,
		now:     time.Now,
		entries: map[QuoteKey]cacheEntry{},
		flights: map[QuoteKey]*flight{},
	}
}

// WithLive registers a live source for its cloud. Returns the receiver
// for chaining.
func (c *Catalog) WithLive(s Source) *Catalog {
	c.live[s.Name()] = s
	return c
}

// Supports reports whether the catalog has any source for the cloud.
func (c *Catalog) Supports(cloud Cloud) bool {
	if _, ok := c.live[cloud]; ok {
		return true
	}
	_, ok := c.static[cloud]
	return ok
}

// PopularSKUs returns the hand-curated static entries across all
// clouds — the set the background Refresher keeps warm.
func (c *Catalog) PopularSKUs() []QuoteKey {
	var out []QuoteKey
	for _, s := range c.static {
		out = append(out, s.Entries()...)
	}
	return out
}

// Quote resolves a price through the cache. Safe for concurrent use.
func (c *Catalog) Quote(ctx context.Context, cloud Cloud, sku, region, lifecycle string) (Quote, error) {
	key := QuoteKey{Cloud: cloud, SKU: sku, Region: region, Lifecycle: strings.ToLower(lifecycle)}
	return c.do(ctx, key, false)
}

// Refresh forcibly re-resolves key, bypassing the freshness check but
// keeping the previous entry available as a stale fallback while the
// provider is consulted. Used by the background Refresher.
func (c *Catalog) Refresh(ctx context.Context, key QuoteKey) (Quote, error) {
	key.Lifecycle = strings.ToLower(key.Lifecycle)
	return c.do(ctx, key, true)
}

func (c *Catalog) do(ctx context.Context, key QuoteKey, force bool) (Quote, error) {
	c.mu.Lock()
	if !force {
		if e, ok := c.entries[key]; ok && c.now().Sub(e.fetched) < c.ttl {
			c.mu.Unlock()
			return e.quote, nil
		}
	}
	if f, ok := c.flights[key]; ok {
		// Someone else is already resolving this key — wait for them.
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.q, f.err
		case <-ctx.Done():
			return Quote{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	q, err := c.resolve(ctx, key)
	f.q, f.err = q, err

	c.mu.Lock()
	delete(c.flights, key)
	if err == nil {
		c.entries[key] = cacheEntry{quote: q, fetched: c.now()}
	}
	c.mu.Unlock()
	close(f.done)
	return q, err
}

// resolve runs the live → stale-cache → static chain for one key.
func (c *Catalog) resolve(ctx context.Context, key QuoteKey) (Quote, error) {
	live, hasLive := c.live[key.Cloud]
	static, hasStatic := c.static[key.Cloud]
	if !hasLive && !hasStatic {
		return Quote{}, fmt.Errorf("no pricing source for cloud %q", key.Cloud)
	}

	var liveErr error
	if hasLive {
		q, err := live.Quote(ctx, key.SKU, key.Region, key.Lifecycle)
		if err == nil {
			return q, nil
		}
		liveErr = err
		if !errors.Is(err, ErrUnimplemented) && !errors.Is(err, ErrNotFound) {
			c.log.Warn("live pricing source failed, using fallbacks",
				"cloud", key.Cloud, "sku", key.SKU, "region", key.Region,
				"lifecycle", key.Lifecycle, "err", err)
			// A real provider price that merely expired beats the
			// hand-maintained static number.
			c.mu.Lock()
			stale, ok := c.entries[key]
			c.mu.Unlock()
			if ok {
				c.log.Info("serving stale cached quote", "key", key.String(),
					"age", c.now().Sub(stale.fetched).String())
				return stale.quote, nil
			}
		}
	}
	if hasStatic {
		if q, err := static.Quote(ctx, key.SKU, key.Region, key.Lifecycle); err == nil {
			return q, nil
		}
	}
	if liveErr != nil {
		return Quote{}, liveErr
	}
	return Quote{}, fmt.Errorf("%w: %s", ErrNotFound, key)
}
