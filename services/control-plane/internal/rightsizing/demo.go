// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rightsizing

// Demo returns recommendations for the built-in demo fleet (the same
// clusters and workloads the other demo fixtures use), computed by the
// real rules from synthetic usage so the demo shows exactly what the
// engine would say. Served only when ClickHouse is unwired.
func Demo(o Options) []Recommendation {
	type fx struct {
		u     Usage
		oom   int
		price Price
	}
	aws := Price{CPUCoreHour: 0.048, RAMGiBHour: 0.0064, Cloud: "aws"}
	gcp := Price{CPUCoreHour: 0.0335, RAMGiBHour: 0.0045, Cloud: "gcp"}
	azure := Price{CPUCoreHour: 0.0496, RAMGiBHour: 0.0066, Cloud: "azure"}
	with := func(p Price, replicas float64) Price { p.Replicas = replicas; return p }
	week := int64(7 * 24 * 12)
	fixtures := []fx{
		{u: Usage{Cluster: "eks-use1-prod", Namespace: "retrieval", Workload: "vectordb-ingress", WorkloadKind: "Deployment", Container: "ingress",
			CPU: Quantiles{P50: 0.22, P90: 0.33, P95: 0.41, P99: 0.62, Max: 0.9}, Mem: Quantiles{P50: 1.1 * GiB, P90: 1.3 * GiB, P95: 1.4 * GiB, P99: 1.6 * GiB, Max: 1.7 * GiB},
			CPURequest: 16, CPULimit: 16, MemRequest: 32 * GiB, MemLimit: 32 * GiB, Samples: week * 20, Buckets: week}, price: with(aws, 6)},
		{u: Usage{Cluster: "eks-use1-prod", Namespace: "retrieval", Workload: "retrieval-indexer", WorkloadKind: "StatefulSet", Container: "indexer",
			CPU: Quantiles{P50: 1.2, P90: 1.9, P95: 2.3, P99: 3.1, Max: 3.8}, Mem: Quantiles{P50: 4.2 * GiB, P90: 5.1 * GiB, P95: 5.5 * GiB, P99: 5.9 * GiB, Max: 6.1 * GiB},
			CPURequest: 4, CPULimit: 8, MemRequest: 64 * GiB, MemLimit: 64 * GiB, Samples: week * 20, Buckets: week}, price: with(aws, 3)},
		{u: Usage{Cluster: "gke-usc1-prod", Namespace: "edge", Workload: "frontend-gateway", WorkloadKind: "Deployment", Container: "envoy",
			CPU: Quantiles{P50: 0.9, P90: 1.3, P95: 1.6, P99: 2.2, Max: 2.9}, Mem: Quantiles{P50: 420 * MiB, P90: 480 * MiB, P95: 510 * MiB, P99: 560 * MiB, Max: 600 * MiB},
			CPURequest: 8, CPULimit: 8, MemRequest: 2 * GiB, MemLimit: 2 * GiB, Samples: week * 20, Buckets: week, ThrottleRisk: 0.01}, price: with(gcp, 8)},
		{u: Usage{Cluster: "aks-westeu-prod-01", Namespace: "edge", Workload: "api-ingress", WorkloadKind: "Deployment", Container: "envoy",
			CPU: Quantiles{P50: 0.35, P90: 0.55, P95: 0.7, P99: 0.95, Max: 1.2}, Mem: Quantiles{P50: 300 * MiB, P90: 340 * MiB, P95: 360 * MiB, P99: 380 * MiB, Max: 400 * MiB},
			CPURequest: 4, CPULimit: 4, MemRequest: 1 * GiB, MemLimit: 1 * GiB, Samples: week * 20, Buckets: week}, price: with(azure, 6)},
		{u: Usage{Cluster: "gke-euw4-batch", Namespace: "data", Workload: "etl-nightly", WorkloadKind: "CronJob", Container: "spark-executor",
			CPU: Quantiles{P50: 0.8, P90: 1.7, P95: 2.1, P99: 2.6, Max: 3.0}, Mem: Quantiles{P50: 6 * GiB, P90: 9 * GiB, P95: 10 * GiB, P99: 11.5 * GiB, Max: 12 * GiB},
			CPURequest: 8, CPULimit: 32, MemRequest: 16 * GiB, MemLimit: 16 * GiB, Samples: 3 * 288 * 20, Buckets: 3 * 288}, price: with(gcp, 12)},
		{u: Usage{Cluster: "eks-use1-prod", Namespace: "payments", Workload: "ledger", WorkloadKind: "Deployment", Container: "app",
			CPU: Quantiles{P50: 0.4, P90: 0.6, P95: 0.7, P99: 0.85, Max: 0.95}, Mem: Quantiles{P50: 700 * MiB, P90: 850 * MiB, P95: 900 * MiB, P99: 1000 * MiB, Max: 1020 * MiB},
			CPURequest: 1, CPULimit: 1, MemRequest: 768 * MiB, MemLimit: 1 * GiB, Samples: week * 20, Buckets: week, MaxRestarts: 4}, oom: 4, price: with(aws, 4)},
		{u: Usage{Cluster: "aks-ne-staging", Namespace: "platform", Workload: "metrics-scraper", WorkloadKind: "Deployment", Container: "prometheus",
			CPU: Quantiles{P50: 0.12, P90: 0.18, P95: 0.2, P99: 0.26, Max: 0.3}, Mem: Quantiles{P50: 900 * MiB, P90: 1000 * MiB, P95: 1050 * MiB, P99: 1100 * MiB, Max: 1150 * MiB},
			CPURequest: 2, CPULimit: 2, MemRequest: 4 * GiB, MemLimit: 4 * GiB, Samples: week * 20, Buckets: week}, price: with(azure, 12)},
		{u: Usage{Cluster: "gke-usc1-prod", Namespace: "checkout", Workload: "cart", WorkloadKind: "Deployment", Container: "app",
			CPU: Quantiles{P50: 0.45, P90: 0.7, P95: 0.8, P99: 0.95, Max: 1.1}, Mem: Quantiles{P50: 250 * MiB, P90: 270 * MiB, P95: 280 * MiB, P99: 290 * MiB, Max: 300 * MiB},
			CPURequest: 0.25, CPULimit: 0.5, MemRequest: 256 * MiB, MemLimit: 512 * MiB, Samples: week * 20, Buckets: week, ThrottleRisk: 0.34}, price: with(gcp, 5)},
	}
	if o.WindowLabel == "" {
		o.WindowLabel = "7d"
	}
	out := make([]Recommendation, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, Recommend(f.u, f.oom, f.price, o))
	}
	return out
}
