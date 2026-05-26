// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package metrics emits the KubeHero chargeback metric schema on the
// collector's /metrics endpoint. Label cardinality is the contract
// every downstream component depends on — keep it narrow.
package metrics

import (
	"fmt"
	"io"
)

// Schema documents every metric the collector exports. The chart's
// PrometheusRule (deploy/helm/kubehero/templates/prometheusrule.yaml)
// assumes these exact names and labels.
const schema = `
# HELP kubehero_pod_cost_usd_per_second Attributed $/sec for a pod-second of compute. Labels are the canonical chargeback axis.
# TYPE kubehero_pod_cost_usd_per_second gauge
# HELP kubehero_pod_recoverable_usd_per_second Portion of pod cost reclaimable via right-sizing (requested − used, priced out).
# TYPE kubehero_pod_recoverable_usd_per_second gauge
# HELP kubehero_pod_cpu_millicores Pod CPU usage in millicores (1s resolution, eBPF-attributed).
# TYPE kubehero_pod_cpu_millicores gauge
# HELP kubehero_pod_memory_bytes Pod resident memory bytes.
# TYPE kubehero_pod_memory_bytes gauge
# HELP kubehero_pod_gpu_util_ratio GPU utilization as a ratio [0, 1]. Present only for pods using GPUs.
# TYPE kubehero_pod_gpu_util_ratio gauge
# HELP kubehero_node_cost_usd_per_hour List-price hourly cost of a node given its SKU + lifecycle.
# TYPE kubehero_node_cost_usd_per_hour gauge
# HELP kubehero_up 1 if the collector is up.
# TYPE kubehero_up gauge
`

// Series is a single sample emitted by the collector. In production
// eBPF drives real values; the scaffold ships demo series so dashboards
// and recording rules have data to render.
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
func WriteSeries(w io.Writer, s Series) {
	var labels string
	first := true
	for k, v := range s.Labels {
		if !first {
			labels += ","
		}
		labels += fmt.Sprintf(`%s="%s"`, k, v)
		first = false
	}
	if labels != "" {
		fmt.Fprintf(w, "%s{%s} %g\n", s.Name, labels, s.Value)
	} else {
		fmt.Fprintf(w, "%s %g\n", s.Name, s.Value)
	}
}
