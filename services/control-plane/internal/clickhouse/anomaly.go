// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/anomaly"
)

// SpendAnomalyProvider reads pod_cost_1s and flags per-workload spend
// whose last-hour rate deviates from the trailing baseline by
// |z| >= ZThreshold.
//
// Like BurnRateProvider we query the raw 1s table rather than the 1m
// rollup: the hourly buckets here are aligned to "now", not to
// calendar minutes, so the rollup's edges would smear the current
// bucket and dampen exactly the spike we're trying to catch.
type SpendAnomalyProvider struct {
	DB *sql.DB
	// ZThreshold is the |z| at which a series is flagged. Non-positive
	// falls back to anomaly.DefaultZThreshold (3.0). Wired from
	// KUBEHERO_ANOMALY_Z_THRESHOLD in main.
	ZThreshold float64
}

// SpendAnomaly is one flagged (cluster, namespace, workload) series.
// All dollar figures are $/hour except ImpactUSDMonth, which follows
// burnrate.go's monthly-extrapolation convention (× 24 × 30) and is
// signed: positive for spikes, negative for drops.
type SpendAnomaly struct {
	ClusterID string
	Namespace string
	Workload  string

	CurrentUSDHour float64
	MeanUSDHour    float64
	StdDevUSDHour  float64
	Z              float64
	DeltaPct       float64
	ImpactUSDMonth float64
}

// EffectiveThreshold resolves the configured threshold with the
// package default, so callers (severity mapping in the RPC layer) see
// the same number the detector used.
func (p *SpendAnomalyProvider) EffectiveThreshold() float64 {
	if p == nil || p.ZThreshold <= 0 {
		return anomaly.DefaultZThreshold
	}
	return p.ZThreshold
}

// Detect buckets the trailing `baseline` + 1h of pod_cost_1s into
// hourly spend per (cluster, namespace, workload), scores the last
// hour against the baseline hours, and returns every series whose
// |z| crosses the threshold, ordered by |monthly impact| descending.
//
// Query design: one aggregation pass over the window —
//
//	SELECT cluster_id, namespace, pod,
//	       intDiv(ts - start, 3600000) AS bucket,
//	       sum(cost_usd_sec) AS spend_usd
//	FROM pod_cost_1s
//	WHERE ts >= start AND ts < now
//	GROUP BY cluster_id, namespace, pod, bucket
//
// Buckets are relative to `now − (baselineHours+1)h`, so every bucket
// covers a full hour and the last one ends exactly at now (no partial
// calendar-hour bias). Pod → workload collapsing (stripping
// ReplicaSet/pod-template hash suffixes) happens Go-side so the
// heuristic is unit-testable without a live ClickHouse; the extra rows
// transferred are bounded by pods-per-workload, which is small at the
// hourly grain.
//
// Guards, in order:
//   - a workload needs >= anomaly.DefaultMinBaseline non-empty
//     baseline hours before it is scored — freshly deployed workloads
//     don't get flagged just for existing;
//   - hours where a known workload emitted no samples count as $0
//     (scaled to zero ≠ missing data);
//   - flat/zero-variance baselines are unscoreable (see anomaly.Score).
func (p *SpendAnomalyProvider) Detect(ctx context.Context, baseline time.Duration) ([]SpendAnomaly, error) {
	if p == nil || p.DB == nil {
		return nil, errors.New("spend-anomaly provider not configured")
	}
	baselineHours := int(baseline.Hours())
	if baselineHours < 2 {
		return nil, fmt.Errorf("baseline %s too short: need at least 2h", baseline)
	}

	now := time.Now().UTC()
	startMS := now.Add(-time.Duration(baselineHours+1) * time.Hour).UnixMilli()
	endMS := now.UnixMilli()

	const q = `
		SELECT cluster_id, namespace, pod,
		       intDiv(ts - ?, 3600000) AS bucket,
		       sum(cost_usd_sec) AS spend_usd
		FROM pod_cost_1s
		WHERE ts >= ? AND ts < ?
		GROUP BY cluster_id, namespace, pod, bucket`

	rows, err := p.DB.QueryContext(ctx, q, startMS, startMS, endMS)
	if err != nil {
		return nil, fmt.Errorf("clickhouse anomaly query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	type key struct{ cluster, namespace, workload string }
	series := map[key][]float64{} // len = baselineHours+1, zero-filled

	for rows.Next() {
		var cluster, namespace, pod string
		var bucket int64
		var spend float64
		if err := rows.Scan(&cluster, &namespace, &pod, &bucket, &spend); err != nil {
			return nil, fmt.Errorf("clickhouse anomaly scan: %w", err)
		}
		if bucket < 0 || bucket > int64(baselineHours) {
			continue // clock-skewed sample outside the window
		}
		k := key{cluster, namespace, WorkloadFromPod(pod)}
		s, ok := series[k]
		if !ok {
			s = make([]float64, baselineHours+1)
			series[k] = s
		}
		s[bucket] += spend
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse anomaly rows: %w", err)
	}

	threshold := p.EffectiveThreshold()
	var out []SpendAnomaly
	for k, s := range series {
		base, current := s[:baselineHours], s[baselineHours]
		nonEmpty := 0
		for _, v := range base {
			if v > 0 {
				nonEmpty++
			}
		}
		if nonEmpty < anomaly.DefaultMinBaseline {
			continue // insufficient history for this workload
		}
		stats, ok := anomaly.Score(base, current, anomaly.DefaultMinBaseline)
		if !ok || !stats.Anomalous(threshold) {
			continue
		}
		out = append(out, SpendAnomaly{
			ClusterID:      k.cluster,
			Namespace:      k.namespace,
			Workload:       k.workload,
			CurrentUSDHour: current,
			MeanUSDHour:    stats.Mean,
			StdDevUSDHour:  stats.StdDev,
			Z:              stats.Z,
			DeltaPct:       stats.DeltaPct,
			ImpactUSDMonth: (current - stats.Mean) * 24 * 30,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return math.Abs(out[i].ImpactUSDMonth) > math.Abs(out[j].ImpactUSDMonth)
	})
	return out, nil
}

// Kubernetes generates pod-name suffixes from rand.String's safe
// alphabet (no vowels, no 0/1/3 — avoids accidental words and
// lookalike digits). Hash-suffix regexps below reference this set.
const k8sRandAlphabet = "bcdfghjklmnpqrstvwxz2456789"

var (
	// CronJob pods: <cronjob>-<minutes-since-epoch>-<rand5>. The digit
	// block can contain 0/1/3 (it's a timestamp, not a rand string),
	// so it needs its own pattern ahead of the Deployment form.
	cronJobPodRE = regexp.MustCompile(`^(.+)-\d{7,12}-[` + k8sRandAlphabet + `]{5}$`)
	// Deployment pods: <workload>-<pod-template-hash>-<rand5>.
	deploymentPodRE = regexp.MustCompile(`^(.+)-[` + k8sRandAlphabet + `]{5,10}-[` + k8sRandAlphabet + `]{5}$`)
	// StatefulSet pods: <workload>-<ordinal>.
	statefulSetPodRE = regexp.MustCompile(`^(.+)-\d+$`)
	// DaemonSet / bare Job pods: <workload>-<rand5>.
	randSuffixPodRE = regexp.MustCompile(`^(.+)-[` + k8sRandAlphabet + `]{5}$`)
)

// WorkloadFromPod collapses a pod name to its owning workload name by
// stripping the generated suffixes Kubernetes appends. It is a
// heuristic — we don't have owner references in pod_cost_1s — but the
// alphabet-restricted patterns make false strips rare, and a wrong
// split only fragments one workload's series rather than corrupting
// another's. Order matters: the two-suffix CronJob/Deployment forms
// must win before the single-suffix forms.
func WorkloadFromPod(pod string) string {
	if m := cronJobPodRE.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	if m := deploymentPodRE.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	if m := statefulSetPodRE.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	if m := randSuffixPodRE.FindStringSubmatch(pod); m != nil {
		return m[1]
	}
	return pod
}
