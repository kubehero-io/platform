// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package insights derives operator-facing signals from cluster events
// and the cost plane: capacity demands (pods the scheduler can't place,
// priced as the node capacity that would unblock them), OOM-kill bursts
// and log error spikes (the non-spend anomaly kinds).
package insights

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Engine reads events and cost from ClickHouse.
type Engine struct {
	CH       *sql.DB
	Clusters *clusters.Resolver
	Timeout  time.Duration
}

func (e *Engine) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 20 * time.Second
}

// Demand is one workload blocked on scheduling.
type Demand struct {
	ID                string
	Cluster           string
	Namespace         string
	Workload          string
	PendingPods       int
	CPUCores          float64 // total requested by the pending pods
	MemBytes          float64
	GPUs              float64
	GPUKind           string
	OldestAge         time.Duration
	Message           string
	RecommendedAction string
	RecommendedUSDMo  float64
	BlockedUSDMo      float64
	SKU               string
	Nodepool          string
	NodesToAdd        int
}

// pendingWindow: an unschedulable event older than this no longer
// counts as a live demand.
const pendingWindow = 30 * time.Minute

type pendingPod struct {
	cluster, namespace, workload, pod string
	attrs                             map[string]string
	message                           string
	lastTS                            time.Time
}

// CapacityDemands returns workloads with pods that were unschedulable
// in the last 30 minutes and haven't started running since.
func (e *Engine) CapacityDemands(ctx context.Context, clusterID string, now time.Time) ([]Demand, error) {
	snap := e.Clusters.Snapshot(ctx)
	aliases := snap.Aliases(clusterID)
	since := now.Add(-pendingWindow)

	var ew, rw chsql.Where
	ew.Add("kind = 'unschedulable'")
	ew.Add("ts >= fromUnixTimestamp64Milli(toInt64(?))", since.UnixMilli())
	rw.Add("ts >= ?", since.UnixMilli())
	if aliases != nil {
		ew.In("cluster_id", aliases)
		rw.In("cluster_id", aliases)
	}
	// A pod that shows up in pod_cost_1s after its last unschedulable
	// event got placed; only the rest are still pending. join_use_nulls
	// is pinned: an unmatched pod must read seen_ms = 0, not NULL.
	query := `
		SELECT e.cluster_id, e.namespace, e.workload, e.pod, e.attrs, e.msg, e.last_ms
		FROM (
			SELECT cluster_id, namespace, workload, pod,
			       argMax(attributes, ts) AS attrs, argMax(message, ts) AS msg,
			       toInt64(toUnixTimestamp64Milli(max(ts))) AS last_ms
			FROM cluster_events WHERE ` + ew.SQL() + `
			GROUP BY cluster_id, namespace, workload, pod
		) AS e
		LEFT JOIN (
			SELECT cluster_id, namespace, pod, max(ts) AS seen_ms
			FROM pod_cost_1s WHERE ` + rw.SQL() + `
			GROUP BY cluster_id, namespace, pod
		) AS r ON r.cluster_id = e.cluster_id AND r.namespace = e.namespace AND r.pod = e.pod
		WHERE r.seen_ms < e.last_ms
		LIMIT 20000
		SETTINGS join_use_nulls = 0`
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, query, append(ew.Args(), rw.Args()...)...)
	if err != nil {
		return nil, fmt.Errorf("capacity demand query: %w", err)
	}
	var pods []pendingPod
	for rows.Next() {
		var (
			p      pendingPod
			c      string
			lastMS int64
		)
		if err := rows.Scan(&c, &p.namespace, &p.workload, &p.pod, &p.attrs, &p.message, &lastMS); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("capacity demand scan: %w", err)
		}
		p.cluster = snap.Display(c)
		p.lastTS = time.UnixMilli(lastMS).UTC()
		if p.workload == "" {
			p.workload = p.attrs["workload"]
		}
		if p.workload == "" {
			p.workload = clickhouse.WorkloadFromPod(p.pod)
		}
		pods = append(pods, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(pods) == 0 {
		return nil, nil
	}
	skus, err := e.nodeSKUs(ctx, aliases, now, snap)
	if err != nil {
		return nil, err
	}
	return buildDemands(pods, skus, now), nil
}

// SKU is one node type seen recently in a cluster.
type SKU struct {
	Cluster, Nodepool, Name, GPUKind string
	PricePerHour                     float64
	CPUCores, MemBytes, GPUs         float64
}

func (e *Engine) nodeSKUs(ctx context.Context, aliases []string, now time.Time, snap *clusters.Snapshot) ([]SKU, error) {
	var w chsql.Where
	w.Add("ts >= ?", now.Add(-6*time.Hour).UnixMilli())
	if aliases != nil {
		w.In("cluster_id", aliases)
	}
	query := `
		SELECT cluster_id, nodepool, sku, any(gpu_kind), toFloat64(max(gpu_count)), avg(price_per_hour),
		       avg(cpu_allocatable_millicores) / 1000, avg(mem_allocatable_bytes)
		FROM node_cost_1s WHERE ` + w.SQL() + `
		GROUP BY cluster_id, nodepool, sku LIMIT 10000`
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, query, w.Args()...)
	if err != nil {
		return nil, fmt.Errorf("node sku query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []SKU
	for rows.Next() {
		var (
			s SKU
			c string
		)
		if err := rows.Scan(&c, &s.Nodepool, &s.Name, &s.GPUKind, &s.GPUs, &s.PricePerHour, &s.CPUCores, &s.MemBytes); err != nil {
			return nil, fmt.Errorf("node sku scan: %w", err)
		}
		s.Cluster = snap.Display(c)
		out = append(out, s)
	}
	return out, rows.Err()
}

func attrFloat(attrs map[string]string, key string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(attrs[key]), 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// buildDemands groups pending pods per workload and prices the node
// capacity that would place them. Pure.
func buildDemands(pods []pendingPod, skus []SKU, now time.Time) []Demand {
	type key struct{ cluster, namespace, workload string }
	groups := map[key][]pendingPod{}
	var order []key
	for _, p := range pods {
		k := key{p.cluster, p.namespace, p.workload}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	out := make([]Demand, 0, len(order))
	for _, k := range order {
		ps := groups[k]
		d := Demand{Cluster: k.cluster, Namespace: k.namespace, Workload: k.workload, PendingPods: len(ps)}
		sum := sha256.Sum256([]byte(k.cluster + "|" + k.namespace + "|" + k.workload))
		d.ID = "demand-" + hex.EncodeToString(sum[:4])
		// Per-pod request: the largest seen (replicas share a template).
		var podCPU, podMem, podGPU float64
		nodepool := ""
		for _, p := range ps {
			cpu := attrFloat(p.attrs, "cpu_millicores") / 1000
			mem := attrFloat(p.attrs, "mem_bytes")
			gpu := attrFloat(p.attrs, "gpu")
			d.CPUCores += cpu
			d.MemBytes += mem
			d.GPUs += gpu
			podCPU, podMem, podGPU = math.Max(podCPU, cpu), math.Max(podMem, mem), math.Max(podGPU, gpu)
			age := time.Duration(attrFloat(p.attrs, "age_sec"))*time.Second + now.Sub(p.lastTS)
			if age > d.OldestAge {
				d.OldestAge = age
			}
			if d.Message == "" {
				d.Message = p.message
			}
			if np := p.attrs["nodepool"]; np != "" {
				nodepool = np
			}
		}
		sku, perNode := cheapestFit(skus, k.cluster, nodepool, podCPU, podMem, podGPU)
		switch {
		case podCPU == 0 && podMem == 0 && podGPU == 0:
			d.RecommendedAction = "investigate: no resource request recorded — likely affinity, taints or quota"
			if d.Message != "" {
				d.RecommendedAction += " (" + truncate(d.Message, 120) + ")"
			}
		case sku == nil:
			d.RecommendedAction = fmt.Sprintf("no node type seen in %s fits one pod (%s cores, %s) — add a larger nodepool",
				k.cluster, fmtCores(podCPU), fmtBytes(podMem))
		default:
			nodes := int(math.Ceil(float64(len(ps)) / float64(perNode)))
			d.NodesToAdd, d.SKU, d.Nodepool, d.GPUKind = nodes, sku.Name, sku.Nodepool, sku.GPUKind
			d.RecommendedAction = fmt.Sprintf("scale nodepool %s by %d node%s (%s)", orUnknown(sku.Nodepool), nodes, plural(nodes), sku.Name)
			d.RecommendedUSDMo = float64(nodes) * sku.PricePerHour * timewin.MonthHours
			if sku.CPUCores > 0 {
				// The work that isn't happening, priced at this SKU's
				// per-core rate.
				d.BlockedUSDMo = d.CPUCores * sku.PricePerHour / sku.CPUCores * timewin.MonthHours
			}
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].BlockedUSDMo != out[j].BlockedUSDMo {
			return out[i].BlockedUSDMo > out[j].BlockedUSDMo
		}
		return out[i].PendingPods > out[j].PendingPods
	})
	return out
}

// cheapestFit picks the lowest-priced SKU in the cluster (preferring
// the hinted nodepool) that fits one pod, and how many pods it holds.
func cheapestFit(skus []SKU, cluster, nodepool string, cpu, mem, gpu float64) (*SKU, int) {
	var best *SKU
	bestPer := 0
	for pass := 0; pass < 2 && best == nil; pass++ {
		for i := range skus {
			s := &skus[i]
			if s.Cluster != cluster || s.PricePerHour <= 0 {
				continue
			}
			if pass == 0 && nodepool != "" && s.Nodepool != nodepool {
				continue
			}
			per := podsPerNode(s, cpu, mem, gpu)
			if per <= 0 {
				continue
			}
			if best == nil || s.PricePerHour < best.PricePerHour {
				best, bestPer = s, per
			}
		}
		if nodepool == "" {
			break
		}
	}
	return best, bestPer
}

func podsPerNode(s *SKU, cpu, mem, gpu float64) int {
	per := math.MaxInt32
	fit := func(have, need float64) {
		if need <= 0 {
			return
		}
		if n := int(math.Floor(have / need)); n < per {
			per = n
		}
	}
	fit(s.CPUCores, cpu)
	fit(s.MemBytes, mem)
	fit(s.GPUs, gpu)
	if per == math.MaxInt32 {
		return 1
	}
	return per
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orUnknown(s string) string {
	if s == "" {
		return "(unlabelled)"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func fmtCores(c float64) string {
	if c > 0 && c < 1 {
		return fmt.Sprintf("%.0fm", c*1000)
	}
	return strconv.FormatFloat(math.Round(c*100)/100, 'f', -1, 64)
}

func fmtBytes(b float64) string {
	const gib = 1 << 30
	if b >= gib {
		return strconv.FormatFloat(math.Round(b/gib*10)/10, 'f', -1, 64) + " GiB"
	}
	return fmt.Sprintf("%.0f MiB", b/(1<<20))
}

// FmtCores / FmtBytes / FmtAge render demand fields for the API.
func FmtCores(c float64) string { return fmtCores(c) + " cores" }

// FmtBytes renders bytes as GiB/MiB.
func FmtBytes(b float64) string { return fmtBytes(b) }

// FmtAge renders a duration the way the dashboard shows it: "4h 12m".
func FmtAge(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int(d/time.Hour) % 24
	m := int(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
