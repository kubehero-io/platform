// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo data shared by every list-style RPC when no backing store is
// wired. Kept identical to the marketing-site fleet so screenshots,
// docs, and local dev all show the same numbers.

package rpc

import kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"

func demoClusters() []*kuberov1.Cluster {
	return []*kuberov1.Cluster{
		{Id: "aks-westeu-prod-01", Name: "aks-westeu-prod-01", Cloud: "azure", Region: "westeurope", Nodes: 142},
		{Id: "aks-ne-staging", Name: "aks-ne-staging", Cloud: "azure", Region: "northeurope", Nodes: 24},
		{Id: "gke-usc1-prod", Name: "gke-usc1-prod", Cloud: "gcp", Region: "us-central1", Nodes: 88},
		{Id: "gke-euw4-batch", Name: "gke-euw4-batch", Cloud: "gcp", Region: "europe-west4", Nodes: 62},
		{Id: "eks-use1-prod", Name: "eks-use1-prod", Cloud: "aws", Region: "us-east-1", Nodes: 210},
		{Id: "eks-usw2-dev", Name: "eks-usw2-dev", Cloud: "aws", Region: "us-west-2", Nodes: 38},
	}
}

func demoAuditEntries() []*kuberov1.AuditEntry {
	return []*kuberov1.AuditEntry{
		{Id: "aud-54021", At: "2026-04-23T09:17:48Z", Policy: "prod-burn-rate-2x", Action: "hpa.cap · 50%", Cluster: "eks-use1-prod", Outcome: "applied", EffectUsdMonth: 8600},
		{Id: "aud-54020", At: "2026-04-23T09:14:02Z", Policy: "prod-burn-rate-2x", Action: "trigger evaluated · true", Cluster: "eks-use1-prod", Outcome: "armed"},
		{Id: "aud-53980", At: "2026-04-22T18:44:11Z", Policy: "gpu-inference-cap", Action: "pod.evict · 4 pods", Cluster: "aks-westeu-prod-01", Outcome: "applied", EffectUsdMonth: 12200},
		{Id: "aud-53979", At: "2026-04-22T18:41:03Z", Policy: "gpu-inference-cap", Action: "trigger evaluated · true", Cluster: "aks-westeu-prod-01", Outcome: "armed"},
		{Id: "aud-53711", At: "2026-04-22T02:01:18Z", Policy: "prod-monthly-ceiling", Action: "threshold 80% crossed", Cluster: "all prod", Outcome: "cooldown"},
		{Id: "aud-53544", At: "2026-04-21T14:20:07Z", Policy: "staging-monthly", Action: "recommend · cpu.cap", Cluster: "aks-ne-staging", Outcome: "reverted"},
	}
}

func demoWaste() []*kuberov1.WasteRecommendation {
	return []*kuberov1.WasteRecommendation{
		{Rank: "01", Workload: "model-server-a100", Namespace: "ml-inference", Cluster: "aks-westeu-prod-01", Cloud: "AKS", Signal: "gpu=8  util=12%", RecoverableUsdMonth: 18200, Action: "apply", Severity: "accent"},
		{Rank: "02", Workload: "vectordb-ingress", Namespace: "retrieval", Cluster: "eks-use1-prod", Cloud: "EKS", Signal: "cpu.req=16  used=0.41", RecoverableUsdMonth: 8600, Action: "apply", Severity: "accent"},
		{Rank: "03", Workload: "etl-nightly", Namespace: "data", Cluster: "gke-euw4-batch", Cloud: "GKE", Signal: "limit=32cpu  burst=2.1", RecoverableUsdMonth: 6100, Action: "review", Severity: "warn"},
		{Rank: "04", Workload: "retrieval-indexer", Namespace: "retrieval", Cluster: "eks-use1-prod", Cloud: "EKS", Signal: "mem.req=64Gi  used=6Gi", RecoverableUsdMonth: 4800, Action: "apply", Severity: "accent"},
		{Rank: "05", Workload: "frontend-gateway", Namespace: "edge", Cluster: "gke-usc1-prod", Cloud: "GKE", Signal: "cpu.req=8  used=1.6", RecoverableUsdMonth: 4200, Action: "apply", Severity: "warn"},
		{Rank: "06", Workload: "metrics-scraper", Namespace: "platform", Cluster: "aks-ne-staging", Cloud: "AKS", Signal: "replicas=12  needed=2", RecoverableUsdMonth: 3900, Action: "apply", Severity: "warn"},
		{Rank: "07", Workload: "api-ingress", Namespace: "edge", Cluster: "aks-westeu-prod-01", Cloud: "AKS", Signal: "cpu.req=4  used=0.7", RecoverableUsdMonth: 3400, Action: "apply", Severity: "warn"},
		{Rank: "08", Workload: "queue-consumer", Namespace: "data", Cluster: "eks-use1-prod", Cloud: "EKS", Signal: "empty for 14d", RecoverableUsdMonth: 2600, Action: "apply", Severity: "accent"},
	}
}

func demoPolicies() []*kuberov1.Policy {
	return []*kuberov1.Policy{
		{Name: "prod-monthly-ceiling", Scope: "env=prod · all clusters", CeilingUsd: 100000, SpentPct: 78, Kind: "BudgetPolicy", Armed: true},
		{Name: "ml-gpu-ceiling", Scope: "ns=ml-inference", CeilingUsd: 45000, SpentPct: 62, Kind: "BudgetPolicy", Armed: true},
		{Name: "prod-burn-rate-2x", Scope: "prod-us-east-1", CeilingUsd: 10000, SpentPct: 40, Kind: "CeilingPolicy", Armed: true},
		{Name: "gpu-inference-cap", Scope: "ns=ml-inference", CeilingUsd: 18200, SpentPct: 88, Kind: "CeilingPolicy", Armed: true},
		{Name: "staging-monthly", Scope: "env=staging", CeilingUsd: 15000, SpentPct: 23, Kind: "BudgetPolicy", Armed: false},
	}
}

// demoWorkloadHistory returns synthetic audit entries scoped to a single
// workload. The narrative follows what an operator would see: an
// arming event, then an apply, then a follow-up scan.
func demoWorkloadHistory(cluster, namespace, name string) []*kuberov1.AuditEntry {
	tag := namespace + "/" + name
	return []*kuberov1.AuditEntry{
		{Id: "aud-w-3", At: "2026-04-23T09:17:48Z", Policy: tag, Action: "rightsize.apply · cpu.req 16 → 1", Cluster: cluster, Outcome: "applied", EffectUsdMonth: 8600},
		{Id: "aud-w-2", At: "2026-04-23T09:14:02Z", Policy: tag, Action: "rec evaluated · safe to apply", Cluster: cluster, Outcome: "armed"},
		{Id: "aud-w-1", At: "2026-04-23T08:00:00Z", Policy: tag, Action: "scan · waste signal detected", Cluster: cluster, Outcome: "armed"},
	}
}

// demoCapacityDemands returns a curated capacity-pressure set. Every
// row carries (a) what's blocked, (b) the dollar cost of the blocked
// work, (c) the recommended capacity addition + its dollar cost. A
// well-run platform's job is to keep (c) below (b) — that's the
// per-row signal the page surfaces.
func demoCapacityDemands() []*kuberov1.CapacityDemand {
	return []*kuberov1.CapacityDemand{
		{
			Id: "demand-gke-batch-a100",
			Cluster: "gke-euw4-batch", Namespace: "data",
			Workload:                "etl-nightly",
			PendingPods:             12,
			RequestedCpu:            "96 cores",
			RequestedMem:            "768 GiB",
			RequestedGpu:            "",
			OldestPendingAge:        "4h 12m",
			RecommendedAction:       "scale nodepool n2-standard-32 by 3 nodes",
			RecommendedCostUsdMonth: 1840,
			BlockedCostUsdMonth:     6100,
			Source:                  "demo",
		},
		{
			Id: "demand-aks-ml-a100",
			Cluster: "aks-westeu-prod-01", Namespace: "ml-inference",
			Workload:                "model-server-a100-canary",
			PendingPods:             2,
			RequestedCpu:            "32 cores",
			RequestedMem:            "256 GiB",
			RequestedGpu:            "2× A100",
			OldestPendingAge:        "1h 3m",
			RecommendedAction:       "scale nodepool ml-a100 by 1 node",
			RecommendedCostUsdMonth: 4100,
			BlockedCostUsdMonth:     18200,
			Source:                  "demo",
		},
		{
			Id: "demand-eks-retrieval",
			Cluster: "eks-use1-prod", Namespace: "retrieval",
			Workload:                "retrieval-indexer",
			PendingPods:             4,
			RequestedCpu:            "16 cores",
			RequestedMem:            "64 GiB",
			RequestedGpu:            "",
			OldestPendingAge:        "22m",
			RecommendedAction:       "scale nodepool m5-2xlarge by 2 nodes",
			RecommendedCostUsdMonth: 560,
			BlockedCostUsdMonth:     4800,
			Source:                  "demo",
		},
	}
}

// demoAnomalies returns a curated set of demo anomalies with stable
// IDs. Each one deep-links into the page that owns the underlying
// signal so the overview card → existing-page → action loop closes
// in two clicks. `source` is "demo" until the ingest pipeline lands
// real ClickHouse rolling-window stats — at that point this whole
// function deletes and the cp computes the same shape from
// pod_cost_1s + audit_log + vulnerability_reports.
func demoAnomalies() []*kuberov1.Anomaly {
	return []*kuberov1.Anomaly{
		{
			Id:             "anom-7c91",
			Kind:           "spend",
			Title:          "ml-inference spend +34% w/w",
			Subject:        "ml-inference",
			Detail:         "model-server-a100 + retrieval-indexer carry 78% of the increase",
			DeltaPct:       34,
			ImpactUsdMonth: 18200,
			Severity:       "warn",
			Source:         "demo",
			LinkPath:       "/workloads/aks-westeu-prod-01/ml-inference/model-server-a100",
		},
		{
			Id:             "anom-3a02",
			Kind:           "capacity",
			Title:          "gke-euw4-batch: 12 pending pods, last 4h",
			Subject:        "gke-euw4-batch",
			Detail:         "etl-nightly couldn't schedule against current nodepool capacity",
			DeltaPct:       0,
			ImpactUsdMonth: 6100,
			Severity:       "warn",
			Source:         "demo",
			LinkPath:       "/clusters/gke-euw4-batch",
		},
		{
			Id:             "anom-c4f8",
			Kind:           "posture",
			Title:          "3 new criticals this week ($31k/mo at risk)",
			Subject:        "fleet-wide",
			Detail:         "CVE-2024-3094 (xz-utils) on vectordb-ingress is the highest cost × CVSS",
			DeltaPct:       0,
			ImpactUsdMonth: 31000,
			Severity:       "critical",
			Source:         "demo",
			LinkPath:       "/posture?severity=critical",
		},
		{
			Id:             "anom-1d52",
			Kind:           "spend",
			Title:          "frontend-gateway cpu.req 4× over use",
			Subject:        "edge / frontend-gateway",
			Detail:         "30-day rolling avg use is 1.6 cores against a 8-core request",
			DeltaPct:       0,
			ImpactUsdMonth: 4200,
			Severity:       "info",
			Source:         "demo",
			LinkPath:       "/workloads/gke-usc1-prod/edge/frontend-gateway",
		},
	}
}

// demoVulnerabilities is the synthetic posture set, joined with the
// per-workload monthly cost so /posture can rank by "expensive AND
// vulnerable". Numbers chosen to mirror real Trivy output shape.
func demoVulnerabilities() []*kuberov1.Vulnerability {
	return []*kuberov1.Vulnerability{
		{Id: "CVE-2024-21626", Severity: "critical", Workload: "model-server-a100", Namespace: "ml-inference", Cluster: "aks-westeu-prod-01", Image: "nvcr.io/nvidia/tritonserver:24.09-py3", PackageName: "runc", InstalledVersion: "1.1.7", FixedVersion: "1.1.12", Cvss: 8.6, CostUsdMonth: 18200, Source: "trivy", FirstSeen: "2026-04-22T08:00:00Z"},
		{Id: "CVE-2024-3094", Severity: "critical", Workload: "vectordb-ingress", Namespace: "retrieval", Cluster: "eks-use1-prod", Image: "ghcr.io/qdrant/qdrant:v1.10.0", PackageName: "xz-utils", InstalledVersion: "5.6.0", FixedVersion: "5.6.2", Cvss: 10.0, CostUsdMonth: 8600, Source: "trivy", FirstSeen: "2026-04-21T12:14:00Z"},
		{Id: "CVE-2024-6387", Severity: "critical", Workload: "frontend-gateway", Namespace: "edge", Cluster: "gke-usc1-prod", Image: "nginx:1.25.3", PackageName: "openssh-server", InstalledVersion: "9.5p1", FixedVersion: "9.8p1", Cvss: 8.1, CostUsdMonth: 4200, Source: "trivy", FirstSeen: "2026-04-20T03:00:00Z"},
		{Id: "CVE-2024-26690", Severity: "high", Workload: "etl-nightly", Namespace: "data", Cluster: "gke-euw4-batch", Image: "apache/airflow:2.9.2", PackageName: "linux-libc-dev", InstalledVersion: "5.15.0-91", FixedVersion: "5.15.0-118", Cvss: 7.8, CostUsdMonth: 6100, Source: "trivy", FirstSeen: "2026-04-19T14:40:00Z"},
		{Id: "CVE-2024-37891", Severity: "high", Workload: "metrics-scraper", Namespace: "platform", Cluster: "aks-ne-staging", Image: "prom/prometheus:v2.51.0", PackageName: "urllib3", InstalledVersion: "2.1.0", FixedVersion: "2.2.2", Cvss: 7.5, CostUsdMonth: 3900, Source: "trivy", FirstSeen: "2026-04-18T09:30:00Z"},
		{Id: "CVE-2024-2961", Severity: "high", Workload: "api-ingress", Namespace: "edge", Cluster: "aks-westeu-prod-01", Image: "envoyproxy/envoy:v1.30.1", PackageName: "glibc", InstalledVersion: "2.31-13", FixedVersion: "2.31-13+deb11u11", Cvss: 7.3, CostUsdMonth: 3400, Source: "trivy", FirstSeen: "2026-04-22T17:11:00Z"},
		{Id: "CVE-2024-21891", Severity: "high", Workload: "queue-consumer", Namespace: "data", Cluster: "eks-use1-prod", Image: "node:20.10-alpine", PackageName: "node", InstalledVersion: "20.10.0", FixedVersion: "20.11.1", Cvss: 7.4, CostUsdMonth: 2600, Source: "trivy", FirstSeen: "2026-04-15T10:00:00Z"},
		{Id: "CVE-2024-28182", Severity: "medium", Workload: "retrieval-indexer", Namespace: "retrieval", Cluster: "eks-use1-prod", Image: "ghcr.io/qdrant/qdrant:v1.10.0", PackageName: "nghttp2", InstalledVersion: "1.55.1", FixedVersion: "1.61.0", Cvss: 5.4, CostUsdMonth: 4800, Source: "trivy", FirstSeen: "2026-04-23T01:10:00Z"},
	}
}

func demoTeamSpend() []*kuberov1.TeamSpend {
	return []*kuberov1.TeamSpend{
		{Team: "ml-inference", CostCenter: "ml-platform", SpendUsdMonth: 82000, RecoverableUsdMonth: 18200, GpuIdleUsdMonth: 14800, AwsUsdMonth: 45000, GcpUsdMonth: 0, AzureUsdMonth: 37000},
		{Team: "retrieval", CostCenter: "ml-platform", SpendUsdMonth: 34000, RecoverableUsdMonth: 8600, GpuIdleUsdMonth: 3200, AwsUsdMonth: 22000, GcpUsdMonth: 12000, AzureUsdMonth: 0},
		{Team: "data", CostCenter: "analytics", SpendUsdMonth: 22000, RecoverableUsdMonth: 6100, GpuIdleUsdMonth: 0, AwsUsdMonth: 6000, GcpUsdMonth: 16000, AzureUsdMonth: 0},
		{Team: "edge", CostCenter: "platform", SpendUsdMonth: 21000, RecoverableUsdMonth: 7600, GpuIdleUsdMonth: 0, AwsUsdMonth: 10000, GcpUsdMonth: 3000, AzureUsdMonth: 8000},
		{Team: "platform", CostCenter: "platform", SpendUsdMonth: 4000, RecoverableUsdMonth: 800, GpuIdleUsdMonth: 0, AwsUsdMonth: 2000, GcpUsdMonth: 1500, AzureUsdMonth: 500},
	}
}
