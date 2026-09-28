// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package metrics emits the KubeHero chargeback metric schema on the
// collector's /metrics endpoint. Label cardinality is the contract
// every downstream component depends on — keep it narrow.
package metrics

import (
	"io"
	"sort"
)

// Schema documents every metric the collector exports. The chart's
// PrometheusRule (deploy/helm/kubehero/templates/prometheusrule.yaml)
// assumes these exact names and labels.
const schema = `
# HELP kubehero_pod_cost_usd_per_second Attributed $/sec for a pod-second of compute. Labels are the canonical chargeback axis.
# TYPE kubehero_pod_cost_usd_per_second gauge
# HELP kubehero_pod_recoverable_usd_per_second Portion of pod cost reclaimable via right-sizing (requested − used, priced out).
# TYPE kubehero_pod_recoverable_usd_per_second gauge
# HELP kubehero_pod_cpu_millicores Pod CPU usage in millicores, measured by the kubelet summary API (0 when stats are unavailable).
# TYPE kubehero_pod_cpu_millicores gauge
# HELP kubehero_pod_memory_bytes Pod memory working set in bytes, measured by the kubelet summary API.
# TYPE kubehero_pod_memory_bytes gauge
# HELP kubehero_pod_gpu_util_ratio GPU utilization as a ratio [0, 1]. Present only for pods using GPUs.
# TYPE kubehero_pod_gpu_util_ratio gauge
# HELP kubehero_node_cost_usd_per_hour List-price hourly cost of a node given its SKU + lifecycle.
# TYPE kubehero_node_cost_usd_per_hour gauge
# HELP kubehero_up 1 if the collector is up.
# TYPE kubehero_up gauge
`

// Series is a single sample emitted by the collector: the cost scanner
// produces them from its last scan; --demo adds synthetic ones.
type Series struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// WriteSchema writes the HELP/TYPE declarations exactly once per scrape.
func WriteSchema(w io.Writer) {
	_, _ = io.WriteString(w, schema)
}

// WriteSeries formats a single series in Prometheus exposition format.
// Labels are written in sorted order with values escaped.
func WriteSeries(w io.Writer, s Series) {
	names := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		names = append(names, k)
	}
	sort.Strings(names)
	values := make([]string, len(names))
	for i, k := range names {
		values[i] = s.Labels[k]
	}
	writeSample(w, s.Name, names, values, s.Value)
}

// WriteAll writes series grouped by metric name (the exposition format
// requires a family's samples to be contiguous), preserving the
// relative order within each family.
func WriteAll(w io.Writer, series []Series) {
	sorted := append([]Series(nil), series...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, s := range sorted {
		WriteSeries(w, s)
	}
}
