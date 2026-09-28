// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package insights

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/anomaly"
	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// OOMBurst is a workload OOM-killed repeatedly in the last hour.
type OOMBurst struct {
	Cluster, Namespace, Workload, Container string
	Kills                                   int
	Last                                    time.Time
	// ExposureUSDMonth is the workload's run-rate spend: what's at
	// stake while it crash-loops, not a loss estimate.
	ExposureUSDMonth float64
}

// DefaultOOMBurst is the kill count in one hour that makes a burst.
const DefaultOOMBurst = 3

// OOMBursts returns workloads with at least minKills OOM kills in the
// hour before now, most kills first.
func (e *Engine) OOMBursts(ctx context.Context, now time.Time, minKills int) ([]OOMBurst, error) {
	if minKills <= 0 {
		minKills = DefaultOOMBurst
	}
	snap := e.Clusters.Snapshot(ctx)
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, `
		SELECT cluster_id, namespace, workload, argMax(container, ts), sum(greatest(count, 1)) AS kills,
		       toInt64(toUnixTimestamp64Milli(max(ts)))
		FROM cluster_events
		WHERE kind = 'oom_killed' AND ts >= fromUnixTimestamp64Milli(toInt64(?)) AND ts < fromUnixTimestamp64Milli(toInt64(?))
		GROUP BY cluster_id, namespace, workload
		HAVING kills >= ?
		ORDER BY kills DESC
		LIMIT 100`, now.Add(-time.Hour).UnixMilli(), now.UnixMilli(), uint64(minKills))
	if err != nil {
		return nil, fmt.Errorf("oom burst query: %w", err)
	}
	var out []OOMBurst
	for rows.Next() {
		var (
			b     OOMBurst
			c     string
			kills uint64
			last  int64
		)
		if err := rows.Scan(&c, &b.Namespace, &b.Workload, &b.Container, &kills, &last); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("oom burst scan: %w", err)
		}
		b.Cluster, b.Kills, b.Last = snap.Display(c), int(kills), time.UnixMilli(last).UTC()
		out = append(out, b)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rates, err := e.SpendRates(ctx, now)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ExposureUSDMonth = rates[WorkloadKey{out[i].Cluster, out[i].Namespace, out[i].Workload}]
	}
	return out, nil
}

// LogSpike is a workload whose error-log rate jumped against its
// trailing baseline.
type LogSpike struct {
	Cluster, Namespace, Workload string
	CurrentPerHour               float64
	MeanPerHour, StdDevPerHour   float64
	Z, DeltaPct                  float64
	ExposureUSDMonth             float64
}

// errorLevels are the log levels counted as errors.
var errorLevels = []string{"error", "fatal", "critical", "panic"}

// MinSpikeLines keeps a jump from 0 to 3 errors from reading as a spike.
const MinSpikeLines = 50

// LogErrorSpikes scores each workload's last-hour error-line count
// against the trailing baseline hours from log_volume_1m, with the same
// z-score rules as spend anomalies (internal/anomaly).
func (e *Engine) LogErrorSpikes(ctx context.Context, now time.Time, baseline time.Duration, z float64) ([]LogSpike, error) {
	hours := int(baseline.Hours())
	if hours < anomaly.DefaultMinBaseline {
		hours = 24
	}
	if z <= 0 {
		z = anomaly.DefaultZThreshold
	}
	start := now.Add(-time.Duration(hours+1) * time.Hour)
	snap := e.Clusters.Snapshot(ctx)
	var w chsql.Where
	w.Add("ts_minute >= toDateTime(?) AND ts_minute < toDateTime(?)", start.Unix(), now.Unix())
	w.In("level", errorLevels)
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, `
		SELECT cluster_id, namespace, workload, intDiv(toInt64(toUnixTimestamp(ts_minute)) - ?, 3600) AS bucket, sum(lines)
		FROM log_volume_1m WHERE `+w.SQL()+`
		GROUP BY cluster_id, namespace, workload, bucket
		LIMIT 500000`, append([]any{start.Unix()}, w.Args()...)...)
	if err != nil {
		return nil, fmt.Errorf("log spike query: %w", err)
	}
	series := map[WorkloadKey][]float64{}
	for rows.Next() {
		var (
			k      WorkloadKey
			c      string
			bucket int64
			lines  uint64
		)
		if err := rows.Scan(&c, &k.Namespace, &k.Workload, &bucket, &lines); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("log spike scan: %w", err)
		}
		if bucket < 0 || bucket > int64(hours) {
			continue
		}
		k.Cluster = snap.Display(c)
		s := series[k]
		if s == nil {
			s = make([]float64, hours+1)
			series[k] = s
		}
		s[bucket] += float64(lines)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := ScoreSpikes(series, hours, z)
	if len(out) == 0 {
		return nil, nil
	}
	rates, err := e.SpendRates(ctx, now)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ExposureUSDMonth = rates[WorkloadKey{out[i].Cluster, out[i].Namespace, out[i].Workload}]
	}
	return out, nil
}

// ScoreSpikes flags series whose last bucket is an upward z-score
// outlier with at least MinSpikeLines. Pure.
func ScoreSpikes(series map[WorkloadKey][]float64, hours int, z float64) []LogSpike {
	var out []LogSpike
	for k, s := range series {
		if len(s) != hours+1 {
			continue
		}
		base, cur := s[:hours], s[hours]
		if cur < MinSpikeLines {
			continue
		}
		stats, ok := anomaly.Score(base, cur, anomaly.DefaultMinBaseline)
		if !ok {
			// A flat baseline (often all-zero: a service that never logged
			// errors) can't be z-scored; a burst out of silence is the
			// spike worth showing.
			flat := true
			for _, v := range base {
				if v != base[0] {
					flat = false
				}
			}
			if !flat || cur <= base[0]*2 {
				continue
			}
			stats = anomaly.Stats{Mean: base[0], Z: math.Inf(1)}
			if base[0] > 0 {
				stats.DeltaPct = (cur - base[0]) / base[0] * 100
			}
		} else if stats.Z < z {
			continue
		}
		out = append(out, LogSpike{Cluster: k.Cluster, Namespace: k.Namespace, Workload: k.Workload,
			CurrentPerHour: cur, MeanPerHour: stats.Mean, StdDevPerHour: stats.StdDev, Z: stats.Z, DeltaPct: stats.DeltaPct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CurrentPerHour > out[j].CurrentPerHour })
	return out
}

// WorkloadKey identifies a workload.
type WorkloadKey struct{ Cluster, Namespace, Workload string }

// SpendRates returns each workload's run-rate spend in $/month from
// the last 24h of workload_cost_1h.
func (e *Engine) SpendRates(ctx context.Context, now time.Time) (map[WorkloadKey]float64, error) {
	snap := e.Clusters.Snapshot(ctx)
	start := timewin.FloorHour(now).Add(-24 * time.Hour)
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, `
		SELECT cluster_id, namespace, workload, sum(cost_usd)
		FROM workload_cost_1h WHERE ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)
		GROUP BY cluster_id, namespace, workload LIMIT 200000`, start.Unix(), timewin.CeilHour(now).Unix())
	if err != nil {
		return nil, fmt.Errorf("spend rate query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[WorkloadKey]float64{}
	covered := now.Sub(start)
	for rows.Next() {
		var (
			c, ns, wl string
			spent     float64
		)
		if err := rows.Scan(&c, &ns, &wl, &spent); err != nil {
			return nil, fmt.Errorf("spend rate scan: %w", err)
		}
		out[WorkloadKey{snap.Display(c), ns, wl}] += timewin.PerMonth(spent, covered)
	}
	return out, rows.Err()
}
