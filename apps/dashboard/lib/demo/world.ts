// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The demo world: one coherent, deterministic fleet that every demo
// fixture is derived from — allocation, cost series, rightsizing,
// profiles, the service map, logs, alerts and the /ask agent all talk
// about the same workloads with the same numbers, so cross-navigating in
// demo mode tells one story:
//
//   · ml-inference/model-server-a100 idles eight A100s (the $18k line);
//   · retrieval/vectordb-ingress requests 16 cores and uses 0.4;
//   · shop/payments is in the middle of a Stripe-timeout error spike;
//   · shop/checkout regressed on json.Marshal (profiles diff);
//   · shop/cart is OOM-killing (rightsizing says upsize);
//   · shop/catalog pulls images cross-zone from S3 (network $).
//
// Clusters come from lib/fleet-data.ts. Named workloads carry
// hand-written numbers; a seeded long tail fills each cluster to ~78% of
// its node spend so allocation tables look like a real fleet (the rest is
// idle capacity).

import { CLUSTERS } from "@/lib/fleet-data";
import { rngFor } from "./rng";

export type WorkloadKind = "Deployment" | "StatefulSet" | "DaemonSet" | "CronJob" | "Job";
export type ProfileKind = "go-http" | "python-ml" | "node-web" | "rust-db" | "jvm-batch";

export type DemoContainer = {
  name: string;
  cpuReq: number; // cores
  cpuLim: number; // 0 = none
  memReqGiB: number;
  memLimGiB: number;
  cpuP50: number;
  cpuP95: number;
  cpuP99: number;
  cpuMax: number;
  memP50GiB: number;
  memP99GiB: number;
  memMaxGiB: number;
  oomKills: number;
  throttle: number; // 0..1
};

export type DemoWorkload = {
  cluster: string;
  namespace: string;
  name: string;
  kind: WorkloadKind;
  team: string;
  costCenter: string;
  nodepool: string;
  zone: string;
  replicas: number;
  containers: DemoContainer[];
  /** Monthly spend by resource at the trailing-30d rate. */
  cpuUsd: number;
  ramUsd: number;
  gpuUsd: number;
  networkUsd: number;
  pvUsd: number;
  gpuCount: number;
  /** Measured usage ÷ request, as the allocation engine reports it. */
  cpuEff: number;
  ramEff: number;
  logGbDay: number;
  labels: Record<string, string>;
  profile?: ProfileKind;
  /** Daily growth rate for the synthetic cost series. */
  trend: number;
  /** Days of usage history (drives rightsizing confidence). */
  historyDays: number;
  named: boolean;
};

export const SHARED_NAMESPACES = ["kube-system", "monitoring"] as const;

type Zones = Record<string, string[]>;
const ZONES: Zones = {
  "aks-westeu-prod-01": ["westeurope-1", "westeurope-2", "westeurope-3"],
  "aks-ne-staging": ["northeurope-1", "northeurope-2"],
  "gke-usc1-prod": ["us-central1-a", "us-central1-b", "us-central1-c"],
  "gke-euw4-batch": ["europe-west4-a", "europe-west4-b"],
  "eks-use1-prod": ["us-east-1a", "us-east-1b", "us-east-1c"],
  "eks-usw2-dev": ["us-west-2a", "us-west-2b"],
};

export function zonesOf(cluster: string): string[] {
  return ZONES[cluster] ?? ["zone-a"];
}

function c(
  name: string,
  cpuReq: number,
  memReqGiB: number,
  use: { p50: number; p95: number; p99: number; max: number; m50: number; m99: number; mmax: number },
  extra: Partial<DemoContainer> = {},
): DemoContainer {
  return {
    name,
    cpuReq,
    cpuLim: extra.cpuLim ?? cpuReq * 2,
    memReqGiB,
    memLimGiB: extra.memLimGiB ?? memReqGiB,
    cpuP50: use.p50,
    cpuP95: use.p95,
    cpuP99: use.p99,
    cpuMax: use.max,
    memP50GiB: use.m50,
    memP99GiB: use.m99,
    memMaxGiB: use.mmax,
    oomKills: extra.oomKills ?? 0,
    throttle: extra.throttle ?? 0,
  };
}

type Named = Omit<DemoWorkload, "named" | "labels" | "historyDays" | "trend" | "logGbDay"> &
  Partial<Pick<DemoWorkload, "labels" | "historyDays" | "trend" | "logGbDay">>;

const NAMED: Named[] = [
  // ── aks-westeu-prod-01 ────────────────────────────────────────────────
  {
    cluster: "aks-westeu-prod-01", namespace: "ml-inference", name: "model-server-a100", kind: "Deployment",
    team: "ml-inference", costCenter: "ml-platform", nodepool: "ml-a100", zone: "westeurope-2", replicas: 4,
    containers: [c("triton", 8, 64, { p50: 1.4, p95: 2.1, p99: 2.6, max: 3.4, m50: 21, m99: 26, mmax: 28.5 })],
    cpuUsd: 2100, ramUsd: 1400, gpuUsd: 23616, networkUsd: 620, pvUsd: 300, gpuCount: 8,
    cpuEff: 0.26, ramEff: 0.41, logGbDay: 3.2, profile: "python-ml", trend: 0.004,
  },
  {
    cluster: "aks-westeu-prod-01", namespace: "ml-inference", name: "embedding-batcher", kind: "Deployment",
    team: "ml-inference", costCenter: "ml-platform", nodepool: "general", zone: "westeurope-1", replicas: 2,
    containers: [c("batcher", 4, 16, { p50: 2.1, p95: 3.2, p99: 3.6, max: 3.9, m50: 9, m99: 12.5, mmax: 13.1 })],
    cpuUsd: 1900, ramUsd: 800, gpuUsd: 0, networkUsd: 210, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.68, ramEff: 0.71, logGbDay: 1.1, profile: "python-ml", trend: 0.002,
  },
  {
    cluster: "aks-westeu-prod-01", namespace: "edge", name: "api-ingress", kind: "Deployment",
    team: "edge", costCenter: "platform", nodepool: "general", zone: "westeurope-1", replicas: 6,
    containers: [c("gateway", 4, 4, { p50: 0.52, p95: 0.7, p99: 0.9, max: 1.3, m50: 0.9, m99: 1.2, mmax: 1.4 })],
    cpuUsd: 3100, ramUsd: 900, gpuUsd: 0, networkUsd: 1400, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.17, ramEff: 0.3, logGbDay: 6.4, profile: "go-http", trend: 0.001,
  },
  // ── aks-ne-staging ───────────────────────────────────────────────────
  {
    cluster: "aks-ne-staging", namespace: "platform", name: "metrics-scraper", kind: "Deployment",
    team: "platform", costCenter: "platform", nodepool: "general", zone: "northeurope-1", replicas: 12,
    containers: [c("scraper", 1, 2, { p50: 0.08, p95: 0.14, p99: 0.19, max: 0.31, m50: 0.4, m99: 0.55, mmax: 0.61 })],
    cpuUsd: 2800, ramUsd: 1600, gpuUsd: 0, networkUsd: 90, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.12, ramEff: 0.26, logGbDay: 2.2, trend: 0.02,
  },
  // ── gke-usc1-prod ────────────────────────────────────────────────────
  {
    cluster: "gke-usc1-prod", namespace: "edge", name: "frontend-gateway", kind: "Deployment",
    team: "edge", costCenter: "platform", nodepool: "n2-standard-8", zone: "us-central1-a", replicas: 4,
    containers: [c("gateway", 8, 8, { p50: 1.1, p95: 1.6, p99: 2.0, max: 2.6, m50: 2.4, m99: 3.1, mmax: 3.3 })],
    cpuUsd: 3600, ramUsd: 1100, gpuUsd: 0, networkUsd: 2200, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.2, ramEff: 0.39, logGbDay: 9.8, profile: "node-web", trend: 0.001,
  },
  // ── gke-euw4-batch ───────────────────────────────────────────────────
  {
    cluster: "gke-euw4-batch", namespace: "data", name: "etl-nightly", kind: "CronJob",
    team: "data", costCenter: "analytics", nodepool: "n2-standard-32", zone: "europe-west4-a", replicas: 3,
    containers: [c("spark-driver", 16, 64, { p50: 2.1, p95: 11.4, p99: 21, max: 31, m50: 22, m99: 48, mmax: 52 }, { cpuLim: 32, memLimGiB: 96 })],
    cpuUsd: 4400, ramUsd: 2200, gpuUsd: 0, networkUsd: 380, pvUsd: 800, gpuCount: 0,
    cpuEff: 0.31, ramEff: 0.52, logGbDay: 4.5, profile: "jvm-batch", trend: 0.003,
  },
  {
    cluster: "gke-euw4-batch", namespace: "data", name: "feature-store", kind: "StatefulSet",
    team: "data", costCenter: "analytics", nodepool: "n2-highmem-8", zone: "europe-west4-b", replicas: 3,
    containers: [c("store", 4, 32, { p50: 1.9, p95: 3.1, p99: 3.4, max: 3.8, m50: 24, m99: 28, mmax: 29.5 })],
    cpuUsd: 1800, ramUsd: 2600, gpuUsd: 0, networkUsd: 240, pvUsd: 1900, gpuCount: 0,
    cpuEff: 0.61, ramEff: 0.81, logGbDay: 0.8, trend: 0.001,
  },
  // ── eks-use1-prod ────────────────────────────────────────────────────
  {
    cluster: "eks-use1-prod", namespace: "ml-training", name: "llm-finetune", kind: "StatefulSet",
    team: "ml-research", costCenter: "ml-platform", nodepool: "p5-h100", zone: "us-east-1a", replicas: 3,
    containers: [c("trainer", 64, 512, { p50: 44, p95: 58, p99: 61, max: 63, m50: 380, m99: 455, mmax: 470 })],
    cpuUsd: 6100, ramUsd: 4200, gpuUsd: 172800, networkUsd: 1300, pvUsd: 2600, gpuCount: 24,
    cpuEff: 0.71, ramEff: 0.78, logGbDay: 2.1, profile: "python-ml", trend: 0.001, historyDays: 12,
  },
  {
    cluster: "eks-use1-prod", namespace: "retrieval", name: "vectordb-ingress", kind: "Deployment",
    team: "retrieval", costCenter: "ml-platform", nodepool: "m5-2xlarge", zone: "us-east-1b", replicas: 4,
    containers: [c("ingress", 16, 16, { p50: 0.28, p95: 0.41, p99: 0.55, max: 0.92, m50: 3.1, m99: 3.9, mmax: 4.2 })],
    cpuUsd: 9200, ramUsd: 2100, gpuUsd: 0, networkUsd: 1900, pvUsd: 400, gpuCount: 0,
    cpuEff: 0.026, ramEff: 0.24, logGbDay: 3.8, profile: "rust-db", trend: 0.002,
  },
  {
    cluster: "eks-use1-prod", namespace: "retrieval", name: "retrieval-indexer", kind: "Deployment",
    team: "retrieval", costCenter: "ml-platform", nodepool: "r5-2xlarge", zone: "us-east-1b", replicas: 3,
    containers: [c("indexer", 2, 64, { p50: 0.9, p95: 1.4, p99: 1.6, max: 1.9, m50: 4.8, m99: 5.8, mmax: 6.1 })],
    cpuUsd: 2400, ramUsd: 5800, gpuUsd: 0, networkUsd: 700, pvUsd: 900, gpuCount: 0,
    cpuEff: 0.7, ramEff: 0.09, logGbDay: 1.4, profile: "rust-db", trend: 0.001,
  },
  {
    cluster: "eks-use1-prod", namespace: "data", name: "queue-consumer", kind: "Deployment",
    team: "data", costCenter: "analytics", nodepool: "m5-xlarge", zone: "us-east-1c", replicas: 4,
    containers: [c("consumer", 2, 4, { p50: 0.01, p95: 0.02, p99: 0.03, max: 0.05, m50: 0.12, m99: 0.14, mmax: 0.15 })],
    cpuUsd: 1900, ramUsd: 700, gpuUsd: 0, networkUsd: 20, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.01, ramEff: 0.03, logGbDay: 0.05, trend: 0, historyDays: 30,
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "storefront", kind: "Deployment",
    team: "commerce", costCenter: "revenue", nodepool: "m5-2xlarge", zone: "us-east-1a", replicas: 12,
    containers: [c("web", 1, 2, { p50: 0.42, p95: 0.71, p99: 0.84, max: 0.97, m50: 0.9, m99: 1.3, mmax: 1.45 })],
    cpuUsd: 4200, ramUsd: 1800, gpuUsd: 0, networkUsd: 3100, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.52, ramEff: 0.55, logGbDay: 12.6, profile: "node-web", trend: 0.004,
    labels: { app: "storefront", tier: "web" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "checkout", kind: "Deployment",
    team: "commerce", costCenter: "revenue", nodepool: "m5-2xlarge", zone: "us-east-1a", replicas: 8,
    containers: [c("api", 1, 1, { p50: 0.46, p95: 0.78, p99: 0.93, max: 1.0, m50: 0.38, m99: 0.52, mmax: 0.58 }, { throttle: 0.18 })],
    cpuUsd: 3600, ramUsd: 1200, gpuUsd: 0, networkUsd: 800, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.61, ramEff: 0.44, logGbDay: 5.9, profile: "go-http", trend: 0.006,
    labels: { app: "checkout", tier: "api" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "payments", kind: "Deployment",
    team: "commerce", costCenter: "revenue", nodepool: "m5-2xlarge", zone: "us-east-1b", replicas: 6,
    containers: [c("api", 1, 1, { p50: 0.22, p95: 0.38, p99: 0.47, max: 0.61, m50: 0.31, m99: 0.42, mmax: 0.46 })],
    cpuUsd: 1800, ramUsd: 600, gpuUsd: 0, networkUsd: 1200, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.29, ramEff: 0.36, logGbDay: 4.1, profile: "go-http", trend: 0.003,
    labels: { app: "payments", tier: "api", pci: "true" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "cart", kind: "Deployment",
    team: "commerce", costCenter: "revenue", nodepool: "m5-2xlarge", zone: "us-east-1c", replicas: 6,
    containers: [c("api", 0.5, 0.5, { p50: 0.21, p95: 0.33, p99: 0.4, max: 0.46, m50: 0.41, m99: 0.49, mmax: 0.5 }, { oomKills: 7, memLimGiB: 0.5 })],
    cpuUsd: 1100, ramUsd: 1400, gpuUsd: 0, networkUsd: 300, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.58, ramEff: 0.96, logGbDay: 2.4, profile: "go-http", trend: 0.004,
    labels: { app: "cart", tier: "api" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "catalog", kind: "Deployment",
    team: "commerce", costCenter: "revenue", nodepool: "m5-xlarge", zone: "us-east-1c", replicas: 4,
    containers: [c("api", 1, 2, { p50: 0.31, p95: 0.52, p99: 0.63, max: 0.8, m50: 0.8, m99: 1.1, mmax: 1.2 })],
    cpuUsd: 1400, ramUsd: 900, gpuUsd: 0, networkUsd: 2400, pvUsd: 0, gpuCount: 0,
    cpuEff: 0.44, ramEff: 0.5, logGbDay: 1.9, profile: "go-http", trend: 0.002,
    labels: { app: "catalog", tier: "api" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "postgres", kind: "StatefulSet",
    team: "commerce", costCenter: "revenue", nodepool: "r5-2xlarge", zone: "us-east-1a", replicas: 3,
    containers: [c("postgres", 4, 32, { p50: 1.6, p95: 2.8, p99: 3.3, max: 3.9, m50: 21, m99: 26, mmax: 27.5 }, { cpuLim: 0 })],
    cpuUsd: 3200, ramUsd: 4100, gpuUsd: 0, networkUsd: 1600, pvUsd: 2800, gpuCount: 0,
    cpuEff: 0.58, ramEff: 0.72, logGbDay: 1.2, trend: 0.002,
    labels: { app: "postgres", tier: "data" },
  },
  {
    cluster: "eks-use1-prod", namespace: "shop", name: "redis", kind: "StatefulSet",
    team: "commerce", costCenter: "revenue", nodepool: "r5-xlarge", zone: "us-east-1b", replicas: 3,
    containers: [c("redis", 1, 8, { p50: 0.3, p95: 0.52, p99: 0.61, max: 0.7, m50: 5.1, m99: 5.6, mmax: 5.8 })],
    cpuUsd: 900, ramUsd: 2200, gpuUsd: 0, networkUsd: 500, pvUsd: 300, gpuCount: 0,
    cpuEff: 0.41, ramEff: 0.69, logGbDay: 0.3, trend: 0.001,
    labels: { app: "redis", tier: "data" },
  },
  // ── eks-usw2-dev ─────────────────────────────────────────────────────
  {
    cluster: "eks-usw2-dev", namespace: "dev", name: "preview-envs", kind: "Deployment",
    team: "dev", costCenter: "engineering", nodepool: "m5-large", zone: "us-west-2a", replicas: 18,
    containers: [c("app", 0.5, 1, { p50: 0.02, p95: 0.06, p99: 0.09, max: 0.2, m50: 0.18, m99: 0.26, mmax: 0.31 })],
    cpuUsd: 3200, ramUsd: 1900, gpuUsd: 0, networkUsd: 60, pvUsd: 200, gpuCount: 0,
    cpuEff: 0.09, ramEff: 0.22, logGbDay: 0.6, trend: 0.01,
  },
];

const FILLER_NS = ["apps", "api", "jobs", "internal-tools", "search", "billing", "auth", "notifications", "reporting"];
const FILLER_WORDS = [
  "ledger", "mailer", "scheduler", "gateway", "renderer", "resolver", "importer", "exporter", "webhooks",
  "sessions", "audit", "pricing", "quotas", "profiles", "reports", "thumbnails", "geo", "ranker", "sync",
  "tokens", "invoices", "digest", "crawler", "flags",
];
const TEAMS_BY_NS: Record<string, [string, string]> = {
  apps: ["product", "engineering"],
  api: ["product", "engineering"],
  jobs: ["data", "analytics"],
  "internal-tools": ["platform", "platform"],
  search: ["retrieval", "ml-platform"],
  billing: ["commerce", "revenue"],
  auth: ["identity", "platform"],
  notifications: ["product", "engineering"],
  reporting: ["data", "analytics"],
  "kube-system": ["platform", "platform"],
  monitoring: ["platform", "platform"],
};

/** Monthly node spend per cluster (the fleet fixture's $/day × 30.4). */
export function clusterMonthlyUsd(clusterId: string): number {
  const cl = CLUSTERS.find((x) => x.id === clusterId);
  if (!cl) return 0;
  return Number(cl.costDay.replace(/[$,]/g, "")) * (730 / 24);
}

export const ALLOCATED_SHARE = 0.78;

function filler(cluster: string, budget: number): DemoWorkload[] {
  const r = rngFor("filler", cluster);
  const out: DemoWorkload[] = [];
  const zones = zonesOf(cluster);
  // Shared platform namespaces first (~6% of the cluster).
  const shared: [string, string, WorkloadKind, number][] = [
    ["kube-system", "coredns", "Deployment", 0.012],
    ["kube-system", "kube-proxy", "DaemonSet", 0.01],
    ["kube-system", "aws-node", "DaemonSet", 0.008],
    ["monitoring", "prometheus", "StatefulSet", 0.018],
    ["monitoring", "kubehero-collector", "DaemonSet", 0.006],
    ["monitoring", "loki-compactor", "StatefulSet", 0.006],
  ];
  let spent = 0;
  for (const [ns, name, kind, share] of shared) {
    if (name === "aws-node" && !cluster.startsWith("eks")) continue;
    const total = budget * share * (0.8 + r() * 0.4);
    spent += total;
    out.push(fillerWorkload(cluster, ns, name, kind, total, zones, r));
  }
  let i = 0;
  const used = new Set<string>();
  while (spent < budget * 0.97 && i < 40) {
    i++;
    const ns = FILLER_NS[Math.floor(r() * FILLER_NS.length)];
    let name = FILLER_WORDS[Math.floor(r() * FILLER_WORDS.length)];
    if (used.has(`${ns}/${name}`)) name = `${name}-${Math.floor(r() * 9) + 2}`;
    used.add(`${ns}/${name}`);
    // Log-normal-ish sizes: a few big services, a long tail of small ones.
    const total = Math.min(budget - spent, budget * (0.01 + Math.pow(r(), 3) * 0.09));
    if (total < 150) break;
    spent += total;
    out.push(fillerWorkload(cluster, ns, name, r() < 0.15 ? "StatefulSet" : r() < 0.1 ? "CronJob" : "Deployment", total, zones, r));
  }
  return out;
}

function fillerWorkload(
  cluster: string,
  ns: string,
  name: string,
  kind: WorkloadKind,
  totalUsd: number,
  zones: string[],
  r: () => number,
): DemoWorkload {
  const [team, costCenter] = TEAMS_BY_NS[ns] ?? ["product", "engineering"];
  const cpuShare = 0.55 + r() * 0.2;
  const netShare = r() * 0.08;
  const pvShare = kind === "StatefulSet" ? 0.08 + r() * 0.1 : 0;
  const cpuUsd = totalUsd * cpuShare * (1 - netShare - pvShare);
  const ramUsd = totalUsd * (1 - cpuShare) * (1 - netShare - pvShare);
  // Size like a real service: pick a per-replica request, derive the
  // replica count from the spend ($/core-month ≈ 24.8, $/GiB-month ≈ 3.4).
  const totalCores = cpuUsd / 24.8;
  const totalGiB = ramUsd / 3.4;
  const perReplica = kind === "DaemonSet" ? 0.25 : [0.25, 0.5, 1, 1, 2, 2, 4, 8][Math.floor(r() * 8)];
  const replicas = Math.max(1, Math.min(kind === "DaemonSet" ? 60 : 80, Math.round(totalCores / perReplica)));
  const cpuReq = Math.max(0.05, Math.round((totalCores / replicas) * 20) / 20);
  const memReq = Math.max(0.125, Math.round((totalGiB / replicas) * 8) / 8);
  // Mostly over-provisioned (that's the industry norm), a few running hot.
  const hot = r() < 0.07;
  const cpuEff = hot ? 0.82 + r() * 0.2 : 0.08 + Math.pow(r(), 1.4) * 0.55;
  const ramEff = hot ? 0.8 + r() * 0.15 : 0.2 + r() * 0.5;
  const p95 = cpuReq * cpuEff * 1.35;
  const m99 = memReq * ramEff * 1.2;
  return {
    cluster,
    namespace: ns,
    name,
    kind,
    team,
    costCenter,
    nodepool: kind === "DaemonSet" ? "all" : "general",
    zone: zones[Math.floor(r() * zones.length)],
    replicas,
    containers: [
      c(name.split("-")[0], cpuReq, memReq, {
        p50: cpuReq * cpuEff,
        p95,
        p99: p95 * 1.15,
        max: p95 * 1.4,
        m50: memReq * ramEff,
        m99,
        mmax: m99 * 1.05,
      }),
    ],
    cpuUsd,
    ramUsd,
    gpuUsd: 0,
    networkUsd: totalUsd * netShare,
    pvUsd: totalUsd * pvShare,
    gpuCount: 0,
    cpuEff,
    ramEff,
    logGbDay: Math.round(totalUsd / 900) / 10 + 0.05,
    labels: { app: name },
    trend: (r() - 0.35) * 0.01,
    historyDays: r() < 0.12 ? 2 + Math.floor(r() * 3) : 30,
    named: false,
  };
}

let cached: DemoWorkload[] | null = null;

/** Every demo workload, named first. Deterministic and memoised. */
export function demoWorkloads(): DemoWorkload[] {
  if (cached) return cached;
  const named: DemoWorkload[] = NAMED.map((w) => ({
    labels: { app: w.name },
    historyDays: 30,
    trend: 0.002,
    logGbDay: 1,
    ...w,
    named: true,
  }));
  const out = [...named];
  for (const cl of CLUSTERS) {
    const budget = clusterMonthlyUsd(cl.id) * ALLOCATED_SHARE;
    const namedSpend = named
      .filter((w) => w.cluster === cl.id)
      .reduce((s, w) => s + w.cpuUsd + w.ramUsd + w.gpuUsd + w.networkUsd + w.pvUsd, 0);
    out.push(...filler(cl.id, Math.max(0, budget - namedSpend)));
  }
  cached = out;
  return out;
}

export function totalUsd(w: DemoWorkload): number {
  return w.cpuUsd + w.ramUsd + w.gpuUsd + w.networkUsd + w.pvUsd;
}

export function findWorkload(cluster: string, namespace: string, name: string): DemoWorkload | undefined {
  return demoWorkloads().find((w) => w.cluster === cluster && w.namespace === namespace && w.name === name);
}

/** Named workloads, optionally in one cluster — what logs/profiles/network narrate. */
export function namedWorkloads(cluster?: string): DemoWorkload[] {
  return demoWorkloads().filter((w) => w.named && (!cluster || w.cluster === cluster));
}
