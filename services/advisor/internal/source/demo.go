// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package source

import (
	"context"
	"time"

	"github.com/kubehero-io/platform/services/advisor/internal/backend"
)

// Demo is the offline source: the same snapshot assembly as the live
// source, run over backend.Demo's fixtures. Used when CONTROL_PLANE_URL
// is unset and as the degrade path when the control plane is
// unreachable; every snapshot says origin "demo".
type Demo struct{}

func (Demo) Fetch(ctx context.Context, clusterID, window string) (*Snapshot, error) {
	return (&ControlPlane{Backend: backend.Demo{}}).Fetch(ctx, clusterID, window)
}

// demoAnchor pins DemoSnapshot's clock so tests see stable numbers.
var demoAnchor = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// DemoSnapshot returns a fresh copy of the demo fixture (24h window) so
// callers can mutate it freely (tests do).
func DemoSnapshot() *Snapshot {
	now := func() time.Time { return demoAnchor }
	c := &ControlPlane{Backend: backend.Demo{Now: now}, Now: now}
	snap, err := c.Fetch(context.Background(), "", "24h")
	if err != nil {
		// backend.Demo never fails; a failure here is a programming error.
		panic("demo snapshot: " + err.Error())
	}
	return snap
}
