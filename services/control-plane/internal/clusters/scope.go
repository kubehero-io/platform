// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clusters

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

// Scope enforces least privilege for callers authenticated with a
// per-cluster enrollment token (a collector or operator): they may read
// their own cluster only. It returns the cluster the request should be
// restricted to — the token's cluster when the request named none —
// and PermissionDenied when the request names another cluster. Users
// and API keys (no ClusterID) pass through unchanged.
func Scope(ctx context.Context, snap *Snapshot, requested string) (string, error) {
	own := auth.PrincipalFromContext(ctx).ClusterID
	if own == "" {
		return requested, nil
	}
	if requested == "" {
		return own, nil
	}
	for _, a := range snap.Aliases(own) {
		if a == requested {
			return requested, nil
		}
	}
	if c, ok := snap.Lookup(requested); ok && c.ID == own {
		return requested, nil
	}
	return "", connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("cluster-scoped credentials may only read cluster %s", own))
}

// RequireFleet rejects cluster-scoped credentials on fleet-wide reads
// that have no cluster parameter to narrow them.
func RequireFleet(ctx context.Context) error {
	if own := auth.PrincipalFromContext(ctx).ClusterID; own != "" {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("fleet-wide read not allowed for cluster-scoped credentials (cluster %s)", own))
	}
	return nil
}
