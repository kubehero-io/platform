// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package source

import "context"

// Demo is the offline source: a small built-in fixture snapshot used
// when CONTROL_PLANE_URL is unset (and as the degrade path when the
// control-plane is unreachable). Shapes match the control-plane's own
// demo fixtures so the dashboard looks the same either way.
type Demo struct{}

func (Demo) Fetch(_ context.Context, clusterID, window string) (*Snapshot, error) {
	snap := DemoSnapshot()
	snap.ClusterID = clusterID
	snap.Window = window
	return snap, nil
}

// DemoSnapshot returns a fresh copy of the fixture so callers can
// mutate it freely (tests do).
func DemoSnapshot() *Snapshot {
	return &Snapshot{
		Origin: "demo",
		Window: "24h",
		Clusters: []Cluster{
			{ID: "eks-use1-prod", Name: "eks-use1-prod", Cloud: "aws", Region: "us-east-1", Nodes: 42},
			{ID: "gke-euw1-prod", Name: "gke-euw1-prod", Cloud: "gcp", Region: "europe-west1", Nodes: 18},
		},
		Waste: []WasteRecommendation{
			{
				Rank: "01", Workload: "model-server-a100", Namespace: "ml-inference",
				Cluster: "eks-use1-prod", Cloud: "EKS",
				Signal:              "gpu.util=7%  req=2xA100",
				RecoverableUSDMonth: 18400,
				Action:              "review", Severity: "warn",
			},
			{
				Rank: "02", Workload: "checkout-api", Namespace: "payments",
				Cluster: "eks-use1-prod", Cloud: "EKS",
				Signal:              "cpu.req=16  used=0.41",
				RecoverableUSDMonth: 6200,
				Action:              "apply", Severity: "accent",
			},
			{
				Rank: "03", Workload: "batch-etl", Namespace: "data",
				Cluster: "gke-euw1-prod", Cloud: "GKE",
				Signal:              "mem.req=64Gi  used=11Gi",
				RecoverableUSDMonth: 3100,
				Action:              "apply", Severity: "accent",
			},
		},
		Anomalies: []Anomaly{
			{
				ID: "anom-7c91", Kind: "spend", Title: "ml-inference spend +34% w/w",
				Subject: "ml-inference", Detail: "GPU nodepool grew 3→5 nodes with flat request volume",
				DeltaPct: 34, ImpactUSDMonth: 12300, Severity: "warn", Source: "demo",
				LinkPath: "/workloads/eks-use1-prod/ml-inference/model-server-a100",
			},
			{
				ID: "anom-2b44", Kind: "capacity", Title: "payments pending pods",
				Subject: "payments", Detail: "6 pods unschedulable for 40m on nodepool general-m5",
				DeltaPct: 0, ImpactUSDMonth: 2100, Severity: "info", Source: "demo",
				LinkPath: "/capacity/eks-use1-prod/payments",
			},
		},
		Spend: &TeamSpendReport{
			Teams: []TeamSpend{
				{Team: "ml-platform", CostCenter: "cc-401", SpendUSDMonth: 74000, RecoverableUSDMonth: 21500, GPUIdleUSDMonth: 16800},
				{Team: "payments", CostCenter: "cc-102", SpendUSDMonth: 31000, RecoverableUSDMonth: 6200},
			},
			FleetTotalUSDMonth:       128400,
			FleetRecoverableUSDMonth: 27700,
		},
		BurnRate: &BurnRate{BurnRateMilli: 1350, Available: true, Source: "demo"},
	}
}
