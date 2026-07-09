// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

// parseAnomalyWindow maps the request's window string ("24h" | "7d" |
// "30d", per the proto contract) to the baseline duration the
// detector compares the last hour against. Empty defaults to 24h.
// Plain Go durations also parse so ad-hoc callers can say "48h".
func parseAnomalyWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 24 * time.Hour, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("parse window %q: %w", s, err)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("parse window %q: %w", s, err)
		}
	}
	if d < 2*time.Hour || d > 90*24*time.Hour {
		return 0, fmt.Errorf("window %q out of range: need 2h–90d", s)
	}
	return d, nil
}

// spendAnomalyToProto renders one detector hit into the wire shape the
// dashboard already understands (same fields the demo fixtures fill).
func spendAnomalyToProto(a clickhouse.SpendAnomaly, threshold float64) *kuberov1.Anomaly {
	subject := a.Namespace + "/" + a.Workload

	severity := "warn"
	switch {
	case a.Z >= 2*threshold:
		severity = "critical"
	case a.Z <= -threshold:
		// Spend drops are worth a card (a fleet that quietly stops
		// spending usually stopped serving) but never page-worthy.
		severity = "info"
	}

	direction := "spike"
	if a.Z < 0 {
		direction = "drop"
	}

	// Stable id per (cluster, namespace, workload): re-listing while an
	// anomaly persists yields the same card, so the dashboard can
	// de-duplicate across refreshes.
	sum := sha256.Sum256([]byte(a.ClusterID + "|" + a.Namespace + "|" + a.Workload))
	id := "anom-" + hex.EncodeToString(sum[:4])

	return &kuberov1.Anomaly{
		Id:      id,
		Kind:    "spend",
		Title:   fmt.Sprintf("%s spend %s %+.0f%% vs baseline", subject, direction, a.DeltaPct),
		Subject: subject,
		Detail: fmt.Sprintf("last hour $%.2f vs baseline $%.2f/h ±$%.2f (z=%.1f)",
			a.CurrentUSDHour, a.MeanUSDHour, a.StdDevUSDHour, a.Z),
		DeltaPct:       a.DeltaPct,
		ImpactUsdMonth: math.Abs(a.ImpactUSDMonth),
		Severity:       severity,
		Source:         "clickhouse",
		LinkPath:       fmt.Sprintf("/workloads/%s/%s/%s", a.ClusterID, a.Namespace, a.Workload),
	}
}
