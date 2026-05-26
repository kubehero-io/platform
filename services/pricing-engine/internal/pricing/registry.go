// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package pricing

import (
	"context"
	"errors"
	"fmt"
)

// Registry resolves a Quote by trying the live cloud source first, then
// falling back to the Static map when the source is unwired or returns
// ErrUnimplemented / ErrNotFound. Either way the caller gets a real
// number for the SKUs we have rates for.
type Registry struct {
	Live   map[Cloud]Source // optional per-cloud live impls
	Static map[Cloud]*Static
}

// NewDefaultRegistry wires every cloud's static fallback. Live sources
// are added by the caller (main.go reads env to decide).
func NewDefaultRegistry() *Registry {
	return &Registry{
		Live: map[Cloud]Source{},
		Static: map[Cloud]*Static{
			CloudAWS:   NewStatic(CloudAWS),
			CloudGCP:   NewStatic(CloudGCP),
			CloudAzure: NewStatic(CloudAzure),
		},
	}
}

// WithLive registers a live source for the given cloud. Returns the
// receiver for chaining.
func (r *Registry) WithLive(s Source) *Registry {
	if r.Live == nil {
		r.Live = map[Cloud]Source{}
	}
	r.Live[s.Name()] = s
	return r
}

// Quote resolves a price. Algorithm:
//
//   1. Try the live source for `cloud` (skipped if not registered).
//   2. On ErrUnimplemented / ErrNotFound, fall back to Static.
//   3. On any other error, surface it (network failure shouldn't be
//      hidden by the static map).
//
// The lifecycle string is normalised to lowercase before dispatch.
func (r *Registry) Quote(ctx context.Context, cloud Cloud, sku, region, lifecycle string) (Quote, error) {
	if live, ok := r.Live[cloud]; ok {
		q, err := live.Quote(ctx, sku, region, lifecycle)
		if err == nil {
			return q, nil
		}
		if !errors.Is(err, ErrUnimplemented) && !errors.Is(err, ErrNotFound) {
			return Quote{}, fmt.Errorf("%s live quote: %w", cloud, err)
		}
		// fall through to static
	}
	if s, ok := r.Static[cloud]; ok {
		return s.Quote(ctx, sku, region, lifecycle)
	}
	return Quote{}, fmt.Errorf("no pricing source for cloud %q", cloud)
}
