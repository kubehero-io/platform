// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package clusters reconciles the two names a cluster goes by. Postgres
// knows every registered cluster by UUID + slug; ClickHouse rows carry
// whatever the collector stamped — the UUID when it authenticates with
// its enrollment token, the slug when an operator configured one by
// hand. Read APIs therefore (a) expand a caller's cluster filter to
// every alias before querying, and (b) display the human slug instead
// of a UUID.
//
// The mapping is cached (it changes only on RegisterCluster) and the
// resolver degrades to identity when Postgres is absent or failing —
// a name cache must never fail a cost query.
package clusters

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// Info is one registered cluster.
type Info struct {
	ID, Slug, Name, Cloud, Region string
}

// Snapshot is an immutable alias table.
type Snapshot struct {
	byAlias map[string]*Info // uuid, slug and name → cluster
	list    []Info
}

// Aliases returns every identifier ClickHouse rows may carry for the
// cluster the caller named (uuid, slug or display name). Unknown
// identifiers pass through unchanged.
func (s *Snapshot) Aliases(id string) []string {
	if id == "" {
		return nil
	}
	if s != nil {
		if c, ok := s.byAlias[id]; ok {
			if c.Slug == "" || c.Slug == c.ID {
				return []string{c.ID}
			}
			return []string{c.ID, c.Slug}
		}
	}
	return []string{id}
}

// Display maps a ClickHouse cluster_id to the name users know (the
// slug); unknown ids are shown as-is.
func (s *Snapshot) Display(id string) string {
	if s != nil {
		if c, ok := s.byAlias[id]; ok && c.Slug != "" {
			return c.Slug
		}
	}
	return id
}

// Lookup returns the registered cluster for any alias.
func (s *Snapshot) Lookup(id string) (Info, bool) {
	if s == nil {
		return Info{}, false
	}
	c, ok := s.byAlias[id]
	if !ok {
		return Info{}, false
	}
	return *c, true
}

// List returns every registered cluster.
func (s *Snapshot) List() []Info {
	if s == nil {
		return nil
	}
	return append([]Info(nil), s.list...)
}

// Resolver loads and caches the alias table from Postgres.
type Resolver struct {
	DB  *sql.DB // nil → identity resolver
	TTL time.Duration
	Log *slog.Logger

	mu      sync.Mutex
	snap    *Snapshot
	fetched time.Time
}

// NewResolver returns a resolver with a 60s cache.
func NewResolver(db *sql.DB, log *slog.Logger) *Resolver {
	return &Resolver{DB: db, TTL: time.Minute, Log: log}
}

// maxClusters bounds the table a single query loads.
const maxClusters = 5000

// Snapshot returns the current alias table, refreshing when stale. On
// refresh failure the previous table (or identity) is returned.
func (r *Resolver) Snapshot(ctx context.Context) *Snapshot {
	if r == nil || r.DB == nil {
		return &Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ttl := r.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	if r.snap != nil && time.Since(r.fetched) < ttl {
		return r.snap
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snap, err := load(qctx, r.DB)
	if err != nil {
		if r.Log != nil {
			r.Log.Warn("cluster alias refresh failed; using cached names", "err", err)
		}
		// Back off: don't hammer a failing Postgres on every request.
		r.fetched = time.Now()
		if r.snap == nil {
			r.snap = &Snapshot{}
		}
		return r.snap
	}
	r.snap, r.fetched = snap, time.Now()
	return snap
}

func load(ctx context.Context, db *sql.DB) (*Snapshot, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id::text, slug, name, cloud, region
		FROM clusters ORDER BY created_at LIMIT $1`, maxClusters)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var list []Info
	for rows.Next() {
		var c Info
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.Cloud, &c.Region); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return index(list), nil
}

// index builds the alias map. Names are the weakest alias and never
// shadow another cluster's id or slug.
func index(list []Info) *Snapshot {
	s := &Snapshot{byAlias: map[string]*Info{}, list: list}
	for i := range s.list {
		c := &s.list[i]
		if c.Name != "" {
			if _, taken := s.byAlias[c.Name]; !taken {
				s.byAlias[c.Name] = c
			}
		}
	}
	for i := range s.list {
		c := &s.list[i]
		if c.Slug != "" {
			s.byAlias[c.Slug] = c
		}
		s.byAlias[c.ID] = c
	}
	return s
}

// NewSnapshot builds a table from explicit entries (tests, fixtures).
func NewSnapshot(list []Info) *Snapshot { return index(append([]Info(nil), list...)) }
