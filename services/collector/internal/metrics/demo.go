// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package metrics

import (
	"math"
	"time"
)

// Demo returns a realistic-looking set of series demonstrating the
// chargeback labels. Used until eBPF probes populate real values.
// The shape exactly matches what production emits — only the numbers
// are synthetic.
func Demo() []Series {
	now := float64(time.Now().Unix())
	teams := []string{"ml-inference", "retrieval", "data", "edge", "platform"}
	pools := []poolMeta{
		{"aks-nc24ads", "azure", "westeurope", "A100 80GB"},
		{"eks-p5",      "aws",   "us-east-1",  "H100 80GB"},
		{"gke-g2",      "gcp",   "europe-west4", "L4 24GB"},
		{"aks-d16as",   "azure", "westeurope", ""},
		{"eks-c6i",     "aws",   "us-east-1",  ""},
	}

	var out []Series
	for _, team := range teams {
		for i, p := range pools {
			phase := math.Sin(now/73 + float64(i)*1.7)
			used := 0.3 + 0.2*phase
			requested := 0.55 + 0.1*phase
			if requested < used {
				requested = used + 0.05
			}
			// priced cost per pod-second, normalized to scale
			base := 0.00005 + 0.00003*math.Abs(phase)
			if p.gpuKind != "" {
				base *= 40 // GPUs are ~40× more expensive
			}

			labels := map[string]string{
				"namespace":   team,
				"pod":         team + "-worker",
				"team":        team,
				"cost_center": "eng",
				"nodepool":    p.nodepool,
				"cloud":       p.cloud,
				"region":      p.region,
				"cluster":     p.cloud + "-" + p.region,
			}
			if p.gpuKind != "" {
				labels["gpu_kind"] = p.gpuKind
			}

			out = append(out,
				Series{Name: "kubehero_pod_cost_usd_per_second", Labels: labels, Value: base * used},
				Series{Name: "kubehero_pod_recoverable_usd_per_second", Labels: labels, Value: base * (requested - used)},
				Series{Name: "kubehero_pod_cpu_millicores", Labels: labels, Value: used * 4000},
			)
			if p.gpuKind != "" {
				out = append(out, Series{
					Name:   "kubehero_pod_gpu_util_ratio",
					Labels: labels,
					// GPU teams alternate hot and cold; ml-inference runs hot,
					// platform keeps idle pools.
					Value: clamp(0.15 + 0.75*math.Abs(math.Sin(now/41+float64(i)))),
				})
			}
		}
	}
	return out
}

type poolMeta struct{ nodepool, cloud, region, gpuKind string }

func clamp(x float64) float64 {
	if x < 0 { return 0 }
	if x > 1 { return 1 }
	return x
}
