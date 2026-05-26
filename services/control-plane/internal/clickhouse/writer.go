// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PodCostWriter batches pod-cost samples into ClickHouse pod_cost_1s.
//
// We use a single-statement INSERT with parameterised VALUES rather
// than the native streaming protocol because:
//
//   - The collector emits ~100-500 samples per 5s tick per cluster,
//     well within what a single INSERT can chew on without
//     contention. The native streaming protocol's value is at 100k+
//     samples/s, which we'll reach when eBPF lands and not before.
//   - It keeps the dependency surface thin — clickhouse-go/v2's
//     database/sql interface, no batch builders, no schema reflection.
//   - Each call is one round-trip + acknowledgement; retries are
//     idempotent because the (cluster_id, pod, ts) tuple won't
//     collide on retry.
//
// Returns (written, dropped, err). `dropped` counts samples that
// failed validation (missing pod, zero ts after stamping, etc.) —
// they're not retryable, just discarded with a metric.
type PodCostWriter struct {
	DB *sql.DB
}

// Sample mirrors the proto's PodCostSample but is plain Go so callers
// don't need to import protobuf to write rows. The RPC handler
// translates incoming protos into this type before calling Insert.
type Sample struct {
	OrgID       string // resolved by the handler before insertion
	ClusterID   string
	Node        string
	Namespace   string
	Pod         string
	Team        string
	CostCenter  string
	Nodepool    string
	Cloud       string
	Region      string
	SKU         string
	Lifecycle   string
	GPUKind     string
	CPUMilli    uint32
	MemBytes    uint64
	GPUUtilPct  float32
	CostUSDSec  float64
	RecoverUSD  float64
	TsUnixMS    int64
}

// Insert writes a batch of samples to pod_cost_1s. Server-stamps
// missing timestamps to "now" before the INSERT so the collector
// can omit ts on the wire to save bytes.
//
// Validation rules (silent drops):
//   - cluster_id must be non-empty (otherwise the row is unattributable)
//   - pod name must be non-empty
//   - cost_usd_sec must be >= 0
//
// Returns (written, dropped, err).
func (w *PodCostWriter) Insert(ctx context.Context, samples []Sample) (int, int, error) {
	if w == nil || w.DB == nil {
		return 0, 0, errors.New("ClickHouse writer not configured")
	}
	if len(samples) == 0 {
		return 0, 0, nil
	}

	now := time.Now().UnixMilli()
	dropped := 0

	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck — committed below on success

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO pod_cost_1s (
			ts, org_id, cluster_id, node, namespace, pod,
			team, cost_center, nodepool, cloud, region, sku,
			lifecycle, gpu_kind,
			cpu_millicores, mem_bytes, gpu_util_pct,
			cost_usd_sec, recoverable_usd_sec
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?,
			?, ?, ?,
			?, ?
		)`)
	if err != nil {
		return 0, 0, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close() //nolint:errcheck

	written := 0
	for _, s := range samples {
		if s.ClusterID == "" || s.Pod == "" || s.CostUSDSec < 0 {
			dropped++
			continue
		}
		ts := s.TsUnixMS
		if ts == 0 {
			ts = now
		}
		if _, err := stmt.ExecContext(ctx,
			ts, s.OrgID, s.ClusterID, s.Node, s.Namespace, s.Pod,
			s.Team, s.CostCenter, s.Nodepool, s.Cloud, s.Region, s.SKU,
			s.Lifecycle, s.GPUKind,
			s.CPUMilli, s.MemBytes, s.GPUUtilPct,
			s.CostUSDSec, s.RecoverUSD,
		); err != nil {
			return written, dropped, fmt.Errorf("exec sample %s/%s: %w", s.Namespace, s.Pod, err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return written, dropped, fmt.Errorf("commit: %w", err)
	}
	return written, dropped, nil
}
