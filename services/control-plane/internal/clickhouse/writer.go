// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/batcher"
)

// PodCostWriter stores what IngestPodCost receives: pod cost samples
// (pod_cost_1s), each reporting node's own price and allocation
// (node_cost_1s, the denominator for idle), and pod labels when the
// collector sends them (pod_metadata, for label:<key> allocation).
//
// Every collector posts a small batch every scan interval (~5s), so a
// 500-node fleet is ~100 requests/s. One INSERT per request per table
// would create hundreds of parts per second — far past what ClickHouse
// merges comfortably ("too many parts"). Rows therefore go through
// group-commit batchers: a request waits (at most Linger) for the
// shared INSERT that carries its rows and gets that insert's result,
// so the insert rate stays at a few per second per table regardless of
// fleet size while written/dropped remain honest — nothing is
// acknowledged before it is stored.
//
// The writer keeps database/sql (what main wires it with): a prepared
// INSERT inside a transaction is a single native batch insert in
// clickhouse-go, and pod-cost volumes are small next to logs.
type PodCostWriter struct {
	DB *sql.DB
	// Linger bounds how long rows wait to share an insert. Default 250ms.
	Linger time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time

	once  sync.Once
	pods  *batcher.Batcher[Sample]
	nodes *batcher.Batcher[NodeSample]
	meta  *batcher.Batcher[PodMetadataRow]
}

// Sample mirrors the proto's PodCostSample but is plain Go so callers
// don't need to import protobuf to write rows. The RPC handler
// translates incoming protos into this type (resolving the cluster)
// before calling Write.
type Sample struct {
	OrgID         string // resolved by the handler before insertion
	ClusterID     string
	Node          string
	Namespace     string
	Pod           string
	Team          string
	CostCenter    string
	Nodepool      string
	Cloud         string
	Region        string
	Zone          string
	SKU           string
	Lifecycle     string
	GPUKind       string
	Workload      string
	WorkloadKind  string
	CPUMilli      uint32
	MemBytes      uint64
	CPUUsageMilli uint32
	MemUsageBytes uint64
	GPUUtilPct    float32
	GPUCount      uint32
	CostUSDSec    float64
	RecoverUSD    float64
	CPUCostUSDSec float64
	RAMCostUSDSec float64
	GPUCostUSDSec float64
	// IntervalSec is the wall clock this sample covers; spend in the
	// interval is CostUSDSec × IntervalSec. 0 reads as 5 (the
	// historical scan interval).
	IntervalSec float32
	TsUnixMS    int64
	// Labels are the pod's labels, sent when the collector first sees
	// the pod or they change. Non-empty → a pod_metadata row.
	Labels map[string]string
}

// NodeSample is one node's price and allocation at a tick.
type NodeSample struct {
	OrgID         string
	ClusterID     string
	Node          string
	Nodepool      string
	Cloud         string
	Region        string
	Zone          string
	SKU           string
	Lifecycle     string
	GPUKind       string
	GPUCount      uint32
	PricePerHour  float64
	PriceSource   string
	CPUAllocMilli uint32
	MemAllocBytes uint64
	CPUReqMilli   uint32
	MemReqBytes   uint64
	CPUUsedMilli  uint32
	MemUsedBytes  uint64
	CostUSDSec    float64
	IdleUSDSec    float64
	IntervalSec   float32
	TsUnixMS      int64
}

// PodMetadataRow is one pod's labels (table pod_metadata).
type PodMetadataRow struct {
	OrgID        string
	ClusterID    string
	Namespace    string
	Pod          string
	Workload     string
	WorkloadKind string
	Node         string
	Team         string
	CostCenter   string
	Labels       map[string]string
	UpdatedAt    time.Time
}

// WriteResult counts rows: Written were stored, Dropped failed
// validation (never retryable, so the caller should not resend them).
type WriteResult struct {
	Written int
	Dropped int
}

// Ingest guard rails shared with the telemetry write path.
const (
	// MaxFutureSkew rejects rows stamped further in the future than
	// this: a skewed node clock would otherwise plant rows the UI
	// shows as "now" for hours.
	MaxFutureSkew = 10 * time.Minute
	// CostRetention matches pod_cost_1s / node_cost_1s TTLs: older rows
	// would be dropped by the next merge anyway.
	CostRetention = 90 * 24 * time.Hour
	// defaultIntervalSec is what interval_sec 0 means (the collector's
	// historical scan interval).
	defaultIntervalSec = 5
	// maxIntervalSec caps one sample's coverage; anything longer is a
	// collector bug, not a pod that ran unobserved for days.
	maxIntervalSec = 3600
	// Pod label caps (Kubernetes' own limits: 63-char names with a
	// ≤253-char prefix, 63-char values; we allow slack).
	maxPodLabels     = 64
	maxPodLabelKey   = 317
	maxPodLabelValue = 1024
)

func (w *PodCostWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *PodCostWriter) init() {
	w.once.Do(func() {
		linger := w.Linger
		if linger <= 0 {
			linger = 250 * time.Millisecond
		}
		cfg := func(name string) batcher.Config {
			return batcher.Config{Name: name, FlushRows: 20_000, FlushInterval: linger, MaxQueueRows: 400_000}
		}
		w.pods = batcher.New(cfg("pod_cost_1s"), w.writePods, nil)
		w.nodes = batcher.New(cfg("node_cost_1s"), w.writeNodes, nil)
		w.meta = batcher.New(cfg("pod_metadata"), w.writeMeta, nil)
		// The writer lives as long as the process (main owns no
		// shutdown hook for it); callers wait on their own rows, so
		// nothing acknowledged is ever left behind in the queue.
		bg := context.Background()
		w.pods.Run(bg)
		w.nodes.Run(bg)
		w.meta.Run(bg)
	})
}

// Insert writes pod samples only; kept for callers that predate node
// samples. Returns (written, dropped, err).
func (w *PodCostWriter) Insert(ctx context.Context, samples []Sample) (int, int, error) {
	r, err := w.Write(ctx, samples, nil)
	return r.Written, r.Dropped, err
}

// Write validates and stores one IngestPodCost request. It returns
// once the rows are in ClickHouse (or failed). Errors wrap
// batcher.ErrFull / batcher.ErrClosed when the queue refused them.
func (w *PodCostWriter) Write(ctx context.Context, samples []Sample, nodes []NodeSample) (WriteResult, error) {
	if w == nil || w.DB == nil {
		return WriteResult{}, errors.New("ClickHouse writer not configured")
	}
	pods, meta, nodeRows, dropped := PreparePodCost(samples, nodes, w.now())
	res := WriteResult{Dropped: dropped}
	if len(pods)+len(nodeRows) == 0 {
		return res, nil
	}
	w.init()

	// Labels first: they are idempotent (ReplacingMergeTree), so if the
	// cost rows then fail and the collector retries, nothing doubles —
	// and if labels fail we stop before writing non-idempotent rows.
	if len(meta) > 0 {
		if err := w.meta.AddWait(ctx, meta); err != nil {
			return res, fmt.Errorf("pod_metadata: %w", err)
		}
	}
	podDone, err := w.pods.Submit(pods)
	if err != nil {
		return res, fmt.Errorf("pod_cost_1s: %w", err)
	}
	nodeDone, err := w.nodes.Submit(nodeRows)
	if err != nil {
		// Pods are already queued and will be written; report them.
		if perr := batcher.Wait(ctx, podDone); perr == nil {
			res.Written = len(pods)
		}
		return res, fmt.Errorf("node_cost_1s: %w", err)
	}
	perr := batcher.Wait(ctx, podDone)
	nerr := batcher.Wait(ctx, nodeDone)
	if perr == nil {
		res.Written += len(pods)
	}
	if nerr == nil {
		res.Written += len(nodeRows)
	}
	switch {
	case perr != nil:
		return res, fmt.Errorf("pod_cost_1s: %w", perr)
	case nerr != nil:
		return res, fmt.Errorf("node_cost_1s: %w", nerr)
	}
	return res, nil
}

// PreparePodCost validates and normalises one request's rows. Invalid
// rows are counted in dropped:
//   - a pod sample needs a cluster, a pod and a finite, non-negative
//     cost; a node sample needs a cluster and a node name;
//   - timestamps: 0 is stamped with now; more than MaxFutureSkew ahead
//     or older than CostRetention is dropped;
//   - interval_sec: 0 / negative / NaN → 5, capped at 1h;
//   - negative or non-finite component costs are zeroed.
func PreparePodCost(samples []Sample, nodes []NodeSample, now time.Time) (pods []Sample, meta []PodMetadataRow, nodeRows []NodeSample, dropped int) {
	nowMS := now.UnixMilli()
	minMS := now.Add(-CostRetention).UnixMilli()
	maxMS := now.Add(MaxFutureSkew).UnixMilli()
	stamp := func(ts int64) (int64, bool) {
		if ts == 0 {
			return nowMS, true
		}
		return ts, ts >= minMS && ts <= maxMS
	}

	pods = make([]Sample, 0, len(samples))
	for _, s := range samples {
		ts, ok := stamp(s.TsUnixMS)
		if !ok || s.ClusterID == "" || s.Pod == "" || !finiteNonNeg(s.CostUSDSec) {
			dropped++
			continue
		}
		s.TsUnixMS = ts
		s.IntervalSec = normInterval(s.IntervalSec)
		s.RecoverUSD = clampCost(s.RecoverUSD)
		s.CPUCostUSDSec = clampCost(s.CPUCostUSDSec)
		s.RAMCostUSDSec = clampCost(s.RAMCostUSDSec)
		s.GPUCostUSDSec = clampCost(s.GPUCostUSDSec)
		if math.IsNaN(float64(s.GPUUtilPct)) || s.GPUUtilPct < 0 {
			s.GPUUtilPct = 0
		}
		if len(s.Labels) > 0 {
			meta = append(meta, PodMetadataRow{
				OrgID: s.OrgID, ClusterID: s.ClusterID, Namespace: s.Namespace, Pod: s.Pod,
				Workload: s.Workload, WorkloadKind: s.WorkloadKind, Node: s.Node,
				Team: s.Team, CostCenter: s.CostCenter,
				Labels:    capPodLabels(s.Labels),
				UpdatedAt: time.UnixMilli(ts).UTC(),
			})
		}
		pods = append(pods, s)
	}

	nodeRows = make([]NodeSample, 0, len(nodes))
	for _, n := range nodes {
		ts, ok := stamp(n.TsUnixMS)
		if !ok || n.ClusterID == "" || n.Node == "" || !finiteNonNeg(n.CostUSDSec) || !finiteNonNeg(n.PricePerHour) {
			dropped++
			continue
		}
		n.TsUnixMS = ts
		n.IntervalSec = normInterval(n.IntervalSec)
		n.IdleUSDSec = clampCost(n.IdleUSDSec)
		nodeRows = append(nodeRows, n)
	}
	return pods, meta, nodeRows, dropped
}

func finiteNonNeg(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

func clampCost(v float64) float64 {
	if !finiteNonNeg(v) {
		return 0
	}
	return v
}

func normInterval(v float32) float32 {
	switch {
	case v != v || v <= 0: // NaN or unset
		return defaultIntervalSec
	case v > maxIntervalSec:
		return maxIntervalSec
	}
	return v
}

func capPodLabels(in map[string]string) map[string]string {
	out := make(map[string]string, min(len(in), maxPodLabels))
	for k, v := range in {
		if len(out) == maxPodLabels {
			break
		}
		if k == "" || len(k) > maxPodLabelKey {
			continue
		}
		if len(v) > maxPodLabelValue {
			v = v[:maxPodLabelValue]
		}
		out[k] = v
	}
	return out
}

func gpuCount(n uint32) uint8 {
	if n > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(n)
}

// execBatch runs one prepared INSERT for n rows inside a transaction —
// clickhouse-go turns that into a single native batch insert.
func execBatch(ctx context.Context, db *sql.DB, query string, n int, args func(i int) []any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close() //nolint:errcheck
	for i := 0; i < n; i++ {
		if _, err := stmt.ExecContext(ctx, args(i)...); err != nil {
			return fmt.Errorf("append row %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (w *PodCostWriter) writePods(ctx context.Context, rows []Sample) error {
	return execBatch(ctx, w.DB, `
		INSERT INTO pod_cost_1s (
			ts, org_id, cluster_id, node, namespace, pod,
			team, cost_center, nodepool, cloud, region, sku,
			lifecycle, gpu_kind,
			cpu_millicores, mem_bytes, gpu_util_pct,
			cost_usd_sec, recoverable_usd_sec,
			interval_sec, workload, workload_kind, zone,
			cpu_usage_millicores, mem_usage_bytes,
			cpu_cost_usd_sec, ram_cost_usd_sec, gpu_cost_usd_sec, gpu_count
		)`, len(rows), func(i int) []any {
		s := &rows[i]
		return []any{
			s.TsUnixMS, s.OrgID, s.ClusterID, s.Node, s.Namespace, s.Pod,
			s.Team, s.CostCenter, s.Nodepool, s.Cloud, s.Region, s.SKU,
			s.Lifecycle, s.GPUKind,
			s.CPUMilli, s.MemBytes, s.GPUUtilPct,
			s.CostUSDSec, s.RecoverUSD,
			s.IntervalSec, s.Workload, s.WorkloadKind, s.Zone,
			s.CPUUsageMilli, s.MemUsageBytes,
			s.CPUCostUSDSec, s.RAMCostUSDSec, s.GPUCostUSDSec, gpuCount(s.GPUCount),
		}
	})
}

func (w *PodCostWriter) writeNodes(ctx context.Context, rows []NodeSample) error {
	return execBatch(ctx, w.DB, `
		INSERT INTO node_cost_1s (
			ts, org_id, cluster_id, node, nodepool, cloud, region, zone, sku,
			lifecycle, gpu_kind, gpu_count, interval_sec, price_per_hour, price_source,
			cpu_allocatable_millicores, mem_allocatable_bytes,
			cpu_requested_millicores, mem_requested_bytes,
			cpu_used_millicores, mem_used_bytes,
			cost_usd_sec, idle_usd_sec
		)`, len(rows), func(i int) []any {
		n := &rows[i]
		return []any{
			n.TsUnixMS, n.OrgID, n.ClusterID, n.Node, n.Nodepool, n.Cloud, n.Region, n.Zone, n.SKU,
			n.Lifecycle, n.GPUKind, gpuCount(n.GPUCount), n.IntervalSec, n.PricePerHour, n.PriceSource,
			n.CPUAllocMilli, n.MemAllocBytes,
			n.CPUReqMilli, n.MemReqBytes,
			n.CPUUsedMilli, n.MemUsedBytes,
			n.CostUSDSec, n.IdleUSDSec,
		}
	})
}

func (w *PodCostWriter) writeMeta(ctx context.Context, rows []PodMetadataRow) error {
	return execBatch(ctx, w.DB, `
		INSERT INTO pod_metadata (
			org_id, cluster_id, namespace, pod, workload, workload_kind,
			node, team, cost_center, labels, updated_at
		)`, len(rows), func(i int) []any {
		m := &rows[i]
		return []any{
			m.OrgID, m.ClusterID, m.Namespace, m.Pod, m.Workload, m.WorkloadKind,
			m.Node, m.Team, m.CostCenter, m.Labels, m.UpdatedAt,
		}
	})
}
