// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// PricingFromEnv reads the network price overrides:
//
//	KUBEHERO_NET_EGRESS_USD_PER_GB      internet egress $/GB for every
//	                                    cloud (default per cloud: aws
//	                                    0.09, gcp 0.12, azure 0.087,
//	                                    unknown 0.09)
//	KUBEHERO_NET_CROSS_ZONE_USD_PER_GB  cross-zone $/GB (default 0.01 on
//	                                    aws/gcp/unknown, 0 on azure)
//
// Invalid values are logged and ignored.
func PricingFromEnv(getenv func(string) string, log *slog.Logger) NetPricing {
	p := DefaultNetPricing()
	p.EgressOverride = envPrice(getenv, log, "KUBEHERO_NET_EGRESS_USD_PER_GB")
	p.CrossZoneOverride = envPrice(getenv, log, "KUBEHERO_NET_CROSS_ZONE_USD_PER_GB")
	return p
}

func envPrice(getenv func(string) string, log *slog.Logger, key string) float64 {
	raw := strings.TrimSpace(getenv(key))
	if raw == "" {
		return -1
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		if log != nil {
			log.Warn("invalid $/GB price, using per-cloud defaults", "env", key, "value", raw)
		}
		return -1
	}
	return v
}

// LogQueueBytesFromEnv reads KUBEHERO_INGEST_LOG_BUFFER_MB (default 256):
// how many MiB of accepted-but-unwritten log lines the control plane
// may hold before answering ResourceExhausted.
func LogQueueBytesFromEnv(getenv func(string) string, log *slog.Logger) int64 {
	raw := strings.TrimSpace(getenv("KUBEHERO_INGEST_LOG_BUFFER_MB"))
	if raw == "" {
		return 256 << 20
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb < 16 || mb > 16<<10 {
		if log != nil {
			log.Warn("invalid KUBEHERO_INGEST_LOG_BUFFER_MB (want 16..16384), using 256", "value", raw)
		}
		return 256 << 20
	}
	return int64(mb) << 20
}

// ClickHouseCloudLookup resolves a cluster's cloud from what its
// collectors reported most recently (node_cost_1s, then pod_cost_1s).
func ClickHouseCloudLookup(db *sql.DB) CloudLookup {
	return func(ctx context.Context, clusterID string) (string, error) {
		since := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
		for _, table := range []string{"node_cost_1s", "pod_cost_1s"} {
			var cloud string
			// table comes from the constant list above, never from input.
			err := db.QueryRowContext(ctx, `SELECT cloud FROM `+table+`
				WHERE org_id = ? AND cluster_id = ? AND ts >= ? AND cloud != ''
				ORDER BY ts DESC LIMIT 1`, orgID, clusterID, since).Scan(&cloud)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", err
			}
			return strings.ToLower(cloud), nil
		}
		return "", nil
	}
}
