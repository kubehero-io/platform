// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo NetworkService: believable service maps per cluster, priced the
// way the control plane prices flows (AWS-style: cross-zone $0.01/GB in
// each direction, internet egress $0.09/GB). eks-use1-prod tells the
// story: storefront → checkout → payments → Stripe, catalog pulling
// product images from S3 through the NAT (billed as egress), chatty
// cross-zone service calls, postgres replicating across zones, and a
// retransmit-heavy link from payments to Stripe.

import type { MapEdge, MapNode, NetworkCost, ServiceMap } from "@/lib/network/types";
import { namedWorkloads } from "./world";

const CROSS_ZONE_USD_PER_GB = 0.02; // $0.01 out + $0.01 in
const EGRESS_USD_PER_GB = 0.09;
const MONTH_S = 730 * 3600;

type EdgeSpec = { from: string; to: string; port: number; mbps: number; retransPct?: number; proto?: string };

const EXTERNAL_ZONE = "";

// Rates in megabytes per second, averaged over the day.
const MAPS: Record<string, { externals: string[]; edges: EdgeSpec[]; extraNodes?: { id: string; name: string; ns: string; zone: string; kind: string }[] }> = {
  "eks-use1-prod": {
    externals: ["internet", "api.stripe.com", "s3.us-east-1.amazonaws.com", "huggingface.co", "sentry.io"],
    extraNodes: [
      { id: "service:kube-system/kube-dns", name: "kube-dns", ns: "kube-system", zone: "us-east-1a", kind: "service" },
      { id: "workload:monitoring/prometheus", name: "prometheus", ns: "monitoring", zone: "us-east-1c", kind: "workload" },
    ],
    edges: [
      { from: "external:internet", to: "workload:shop/storefront", port: 443, mbps: 9.5 },
      { from: "workload:shop/storefront", to: "external:internet", port: 443, mbps: 38 },
      { from: "workload:shop/storefront", to: "workload:shop/checkout", port: 8080, mbps: 4.1 },
      { from: "workload:shop/storefront", to: "workload:shop/catalog", port: 8080, mbps: 6.8 },
      { from: "workload:shop/storefront", to: "workload:shop/cart", port: 8080, mbps: 2.2 },
      { from: "workload:shop/checkout", to: "workload:shop/payments", port: 8080, mbps: 1.4 },
      { from: "workload:shop/checkout", to: "workload:shop/postgres", port: 5432, mbps: 3.3 },
      { from: "workload:shop/checkout", to: "workload:shop/cart", port: 8080, mbps: 1.1 },
      { from: "workload:shop/cart", to: "workload:shop/redis", port: 6379, mbps: 5.2 },
      { from: "workload:shop/payments", to: "external:api.stripe.com", port: 443, mbps: 0.9, retransPct: 2.4 },
      { from: "workload:shop/payments", to: "workload:shop/postgres", port: 5432, mbps: 0.6 },
      { from: "workload:shop/catalog", to: "external:s3.us-east-1.amazonaws.com", port: 443, mbps: 21 },
      { from: "workload:shop/catalog", to: "workload:shop/postgres", port: 5432, mbps: 1.9 },
      { from: "workload:shop/postgres", to: "workload:shop/postgres", port: 5432, mbps: 0 },
      { from: "workload:retrieval/retrieval-indexer", to: "workload:retrieval/vectordb-ingress", port: 6333, mbps: 7.4 },
      { from: "workload:retrieval/vectordb-ingress", to: "external:s3.us-east-1.amazonaws.com", port: 443, mbps: 3.1 },
      { from: "workload:ml-training/llm-finetune", to: "external:huggingface.co", port: 443, mbps: 2.6 },
      { from: "workload:ml-training/llm-finetune", to: "external:s3.us-east-1.amazonaws.com", port: 443, mbps: 14 },
      { from: "workload:data/queue-consumer", to: "workload:shop/redis", port: 6379, mbps: 0.02 },
      { from: "workload:shop/storefront", to: "external:sentry.io", port: 443, mbps: 0.3 },
      { from: "workload:monitoring/prometheus", to: "workload:shop/storefront", port: 9090, mbps: 0.7 },
      { from: "workload:monitoring/prometheus", to: "workload:shop/checkout", port: 9090, mbps: 0.5 },
      { from: "workload:monitoring/prometheus", to: "workload:retrieval/vectordb-ingress", port: 9090, mbps: 0.4 },
      { from: "workload:shop/checkout", to: "service:kube-system/kube-dns", port: 53, mbps: 0.2, proto: "udp" },
      { from: "workload:shop/storefront", to: "service:kube-system/kube-dns", port: 53, mbps: 0.3, proto: "udp" },
    ],
  },
  "aks-westeu-prod-01": {
    externals: ["internet", "huggingface.co"],
    edges: [
      { from: "external:internet", to: "workload:edge/api-ingress", port: 443, mbps: 6.2 },
      { from: "workload:edge/api-ingress", to: "external:internet", port: 443, mbps: 22 },
      { from: "workload:edge/api-ingress", to: "workload:ml-inference/model-server-a100", port: 8001, mbps: 3.8 },
      { from: "workload:ml-inference/model-server-a100", to: "workload:ml-inference/embedding-batcher", port: 8080, mbps: 1.6 },
      { from: "workload:ml-inference/model-server-a100", to: "external:huggingface.co", port: 443, mbps: 1.2 },
    ],
  },
  "gke-usc1-prod": {
    externals: ["internet"],
    edges: [
      { from: "external:internet", to: "workload:edge/frontend-gateway", port: 443, mbps: 7.1 },
      { from: "workload:edge/frontend-gateway", to: "external:internet", port: 443, mbps: 31 },
    ],
  },
  "gke-euw4-batch": {
    externals: ["storage.googleapis.com"],
    edges: [
      { from: "workload:data/etl-nightly", to: "external:storage.googleapis.com", port: 443, mbps: 18 },
      { from: "workload:data/etl-nightly", to: "workload:data/feature-store", port: 6379, mbps: 4.4 },
    ],
  },
};

function nodeId(ns: string, name: string): string {
  return `workload:${ns}/${name}`;
}

export function demoClustersWithMaps(): string[] {
  return Object.keys(MAPS);
}

export function demoServiceMap(q: { clusterId?: string; namespace?: string; startMs: number; endMs: number }): ServiceMap {
  const clusters = q.clusterId ? [q.clusterId] : ["eks-use1-prod"];
  const seconds = Math.max(1, (q.endMs - q.startMs) / 1000);
  const nodes = new Map<string, MapNode>();
  const edges: MapEdge[] = [];
  for (const cluster of clusters) {
    const spec = MAPS[cluster];
    if (!spec) continue;
    for (const w of namedWorkloads(cluster)) {
      nodes.set(nodeId(w.namespace, w.name), { id: nodeId(w.namespace, w.name), name: w.name, namespace: w.namespace, kind: "workload", zone: w.zone, bytesIn: 0, bytesOut: 0, costUsdMonth: 0 });
    }
    for (const x of spec.extraNodes ?? []) {
      nodes.set(x.id, { id: x.id, name: x.name, namespace: x.ns, kind: x.kind, zone: x.zone, bytesIn: 0, bytesOut: 0, costUsdMonth: 0 });
    }
    for (const ext of spec.externals) {
      nodes.set(`external:${ext}`, { id: `external:${ext}`, name: ext, namespace: "", kind: "external", zone: EXTERNAL_ZONE, bytesIn: 0, bytesOut: 0, costUsdMonth: 0 });
    }
    // postgres also replicates across its three zones; that flow is a
    // self-loop on the pooled node, so it shows in the costs table only.
    for (const e of spec.edges) {
      if (e.from === e.to) continue;
      const src = nodes.get(e.from);
      const dst = nodes.get(e.to);
      if (!src || !dst) continue;
      const bps = e.mbps * 1e6;
      const bytes = bps * seconds;
      const egress = dst.kind === "external";
      const ingressFromInternet = src.kind === "external";
      const crossZone = !egress && !ingressFromInternet && !!src.zone && !!dst.zone && src.zone !== dst.zone;
      const monthlyGb = (bps * MONTH_S) / 1e9;
      const costUsdMonth = egress ? monthlyGb * EGRESS_USD_PER_GB : crossZone ? monthlyGb * CROSS_ZONE_USD_PER_GB : 0;
      const packets = bytes / 1460;
      edges.push({
        id: `${e.from}→${e.to}:${e.port}/${e.proto ?? "tcp"}`,
        source: e.from,
        target: e.to,
        port: e.port,
        protocol: e.proto ?? "tcp",
        bytes,
        bytesPerSec: bps,
        crossZone,
        egress,
        costUsdMonth,
        retransmits: packets * ((e.retransPct ?? 0.05) / 100),
      });
      src.bytesOut += bytes;
      dst.bytesIn += bytes;
      src.costUsdMonth += costUsdMonth;
    }
  }
  let list = [...nodes.values()].filter((n) => n.bytesIn + n.bytesOut > 0);
  let es = edges;
  if (q.namespace) {
    // Focus: the namespace's nodes plus their direct peers.
    const focus = new Set(list.filter((n) => n.namespace === q.namespace).map((n) => n.id));
    es = edges.filter((e) => focus.has(e.source) || focus.has(e.target));
    const keep = new Set(es.flatMap((e) => [e.source, e.target]));
    list = list.filter((n) => keep.has(n.id));
  }
  const cz = es.filter((e) => e.crossZone).reduce((s, e) => s + e.bytes, 0) / 1e9;
  const eg = es.filter((e) => e.egress).reduce((s, e) => s + e.bytes, 0) / 1e9;
  return {
    nodes: list,
    edges: es,
    totalCostUsdMonth: es.reduce((s, e) => s + e.costUsdMonth, 0),
    crossZoneGb: cz,
    egressGb: eg,
  };
}

export function demoNetworkCosts(q: { clusterId?: string; startMs: number; endMs: number; limit?: number }): { costs: NetworkCost[]; totalUsdMonth: number } {
  const map = demoServiceMap({ clusterId: q.clusterId, startMs: q.startMs, endMs: q.endMs });
  const byNode = new Map<string, NetworkCost & { topBytes: number }>();
  const nodeById = new Map(map.nodes.map((n) => [n.id, n]));
  for (const e of map.edges) {
    const src = nodeById.get(e.source);
    if (!src || src.kind === "external") continue;
    const k = src.id;
    const row =
      byNode.get(k) ??
      ({ namespace: src.namespace, workload: src.name, egressGb: 0, crossZoneGb: 0, egressUsdMonth: 0, crossZoneUsdMonth: 0, totalUsdMonth: 0, topDestination: "", topBytes: 0 } as NetworkCost & { topBytes: number });
    const gb = e.bytes / 1e9;
    if (e.egress) {
      row.egressGb += gb;
      row.egressUsdMonth += e.costUsdMonth;
    } else if (e.crossZone) {
      row.crossZoneGb += gb;
      row.crossZoneUsdMonth += e.costUsdMonth;
    }
    row.totalUsdMonth = row.egressUsdMonth + row.crossZoneUsdMonth;
    if ((e.egress || e.crossZone) && e.bytes > row.topBytes) {
      row.topBytes = e.bytes;
      row.topDestination = nodeById.get(e.target)?.name ?? e.target;
    }
    byNode.set(k, row);
  }
  // postgres cross-zone replication (3 replicas across zones).
  if ((q.clusterId ?? "eks-use1-prod") === "eks-use1-prod") {
    const pg =
      byNode.get("workload:shop/postgres") ??
      ({ namespace: "shop", workload: "postgres", egressGb: 0, crossZoneGb: 0, egressUsdMonth: 0, crossZoneUsdMonth: 0, totalUsdMonth: 0, topDestination: "", topBytes: 0 } as NetworkCost & { topBytes: number });
    byNode.set("workload:shop/postgres", pg);
    const bps = 2.4e6;
    const gb = (bps * Math.max(1, (q.endMs - q.startMs) / 1000)) / 1e9;
    pg.crossZoneGb += gb;
    pg.crossZoneUsdMonth += ((bps * MONTH_S) / 1e9) * CROSS_ZONE_USD_PER_GB;
    pg.totalUsdMonth = pg.egressUsdMonth + pg.crossZoneUsdMonth;
    pg.topDestination = pg.topDestination || "postgres (replicas, us-east-1b/c)";
  }
  const costs = [...byNode.values()]
    .filter((r) => r.totalUsdMonth > 0)
    .sort((a, b) => b.totalUsdMonth - a.totalUsdMonth)
    .slice(0, q.limit ?? 100)
    .map(({ topBytes: _t, ...rest }) => rest);
  return { costs, totalUsdMonth: costs.reduce((s, c) => s + c.totalUsdMonth, 0) };
}

