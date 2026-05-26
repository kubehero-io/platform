// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// TODO: replace with a Connect client call to ControlPlaneService.ListClusters.
// See services/control-plane/internal/rpc/control.go — same shape.

export type Cloud = "AKS" | "GKE" | "EKS";
export type ClusterState = "healthy" | "warn" | "critical";

export type Cluster = {
  id: string;
  name: string;
  cloud: Cloud;
  region: string;
  nodes: number;
  gpu: string;
  costDay: string;
  recoverable: string;
  state: ClusterState;
};

export const CLUSTERS: Cluster[] = [
  { id: "aks-westeu-prod-01", name: "aks-westeu-prod-01", cloud: "AKS", region: "westeurope",  nodes: 142, gpu: "8× A100",  costDay: "$4,820",  recoverable: "$1,920", state: "warn" },
  { id: "aks-ne-staging",     name: "aks-ne-staging",     cloud: "AKS", region: "northeurope", nodes:  24, gpu: "—",        costDay: "$480",    recoverable: "$110",   state: "healthy" },
  { id: "gke-usc1-prod",      name: "gke-usc1-prod",      cloud: "GKE", region: "us-central1", nodes:  88, gpu: "—",        costDay: "$2,140",  recoverable: "$380",   state: "healthy" },
  { id: "gke-euw4-batch",     name: "gke-euw4-batch",     cloud: "GKE", region: "europe-west4",nodes:  62, gpu: "16× L4",   costDay: "$1,680",  recoverable: "$540",   state: "warn" },
  { id: "eks-use1-prod",      name: "eks-use1-prod",      cloud: "EKS", region: "us-east-1",   nodes: 210, gpu: "32× H100", costDay: "$12,940", recoverable: "$5,180", state: "critical" },
  { id: "eks-usw2-dev",       name: "eks-usw2-dev",       cloud: "EKS", region: "us-west-2",   nodes:  38, gpu: "—",        costDay: "$620",    recoverable: "$180",   state: "healthy" },
];

export const cloudColor: Record<Cloud, string> = {
  AKS: "var(--color-cool)",
  GKE: "var(--color-signal)",
  EKS: "var(--color-warn)",
};

export const stateMeta: Record<ClusterState, { label: string; color: string }> = {
  healthy:  { label: "healthy",    color: "var(--color-signal)" },
  warn:     { label: "overcommit", color: "var(--color-warn)"   },
  critical: { label: "burning",    color: "var(--color-accent)" },
};
