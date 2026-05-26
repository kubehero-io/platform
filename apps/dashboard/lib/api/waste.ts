// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { getWorkload, isLive, listWasteRecommendations } from "./client";
import type { AuditEntryDTO, WasteRecommendationDTO } from "./types";
import type { WasteRowData } from "@/components/interactive/waste-row";
import type { Cloud } from "@/lib/fleet-data";
import type { AuditEntry } from "./audit";

const DEMO: WasteRowData[] = [
  { rank: "01", workload: "model-server-a100", ns: "ml-inference", cluster: "aks-westeu-prod-01", cloud: "AKS", detail: "gpu=8  util=12%",          amountK: 18.2, tone: "var(--color-accent)", action: "apply" },
  { rank: "02", workload: "vectordb-ingress",  ns: "retrieval",    cluster: "eks-use1-prod",      cloud: "EKS", detail: "cpu.req=16  used=0.41",    amountK:  8.6, tone: "var(--color-accent)", action: "apply" },
  { rank: "03", workload: "etl-nightly",       ns: "data",         cluster: "gke-euw4-batch",     cloud: "GKE", detail: "limit=32cpu  burst=2.1",   amountK:  6.1, tone: "var(--color-warn)",   action: "review" },
  { rank: "04", workload: "retrieval-indexer", ns: "retrieval",    cluster: "eks-use1-prod",      cloud: "EKS", detail: "mem.req=64Gi  used=6Gi",   amountK:  4.8, tone: "var(--color-accent)", action: "apply" },
  { rank: "05", workload: "frontend-gateway",  ns: "edge",         cluster: "gke-usc1-prod",      cloud: "GKE", detail: "cpu.req=8  used=1.6",      amountK:  4.2, tone: "var(--color-warn)",   action: "apply" },
  { rank: "06", workload: "metrics-scraper",   ns: "platform",     cluster: "aks-ne-staging",     cloud: "AKS", detail: "replicas=12  needed=2",    amountK:  3.9, tone: "var(--color-warn)",   action: "apply" },
  { rank: "07", workload: "api-ingress",       ns: "edge",         cluster: "aks-westeu-prod-01", cloud: "AKS", detail: "cpu.req=4  used=0.7",      amountK:  3.4, tone: "var(--color-warn)",   action: "apply" },
  { rank: "08", workload: "queue-consumer",    ns: "data",         cluster: "eks-use1-prod",      cloud: "EKS", detail: "empty for 14d",            amountK:  2.6, tone: "var(--color-accent)", action: "apply" },
];

const VALID_CLOUDS = new Set(["AKS", "GKE", "EKS"]);

function dtoToRow(r: WasteRecommendationDTO): WasteRowData {
  const cloud: Cloud = VALID_CLOUDS.has(r.cloud) ? (r.cloud as Cloud) : "EKS";
  const tone = r.severity === "warn" ? "var(--color-warn)" : "var(--color-accent)";
  const action = r.action === "review" ? "review" : "apply";
  return {
    rank: r.rank,
    workload: r.workload,
    ns: r.namespace,
    cluster: r.cluster,
    cloud,
    detail: r.signal,
    amountK: r.recoverableUsdMonth / 1000,
    tone,
    action,
  };
}

export type WasteState = {
  rows: WasteRowData[];
  source: "live" | "demo";
};

export async function getWaste(opts: { limit?: number } = {}): Promise<WasteState> {
  if (!isLive()) {
    return { rows: DEMO, source: "demo" };
  }
  const res = await listWasteRecommendations(opts);
  if (!res || res.recommendations.length === 0) {
    return { rows: DEMO, source: "demo" };
  }
  return { rows: res.recommendations.map(dtoToRow), source: "live" };
}

// ─── Workload drill-in ────────────────────────────────────────────────────

export type WorkloadDetail = {
  rec: WasteRowData | null;
  history: AuditEntry[];
  source: "live" | "demo";
};

function auditDtoToDisplay(e: AuditEntryDTO): AuditEntry {
  const at = e.at.replace("T", " ").replace("Z", "");
  const o = ["applied", "armed", "cooldown", "reverted"].includes(e.outcome)
    ? (e.outcome as AuditEntry["outcome"])
    : "armed";
  return {
    id: e.id,
    at,
    policy: e.policy,
    action: e.action,
    cluster: e.cluster,
    outcome: o,
    effectK: e.effectUsdMonth > 0 ? e.effectUsdMonth / 1000 : undefined,
  };
}

export async function getWorkloadDetail(args: {
  cluster: string;
  namespace: string;
  name: string;
}): Promise<WorkloadDetail | null> {
  // Try live first; on miss / unset, synthesise from the demo set.
  if (isLive()) {
    const res = await getWorkload(args);
    if (res) {
      return {
        rec: res.recommendation ? dtoToRow(res.recommendation) : null,
        history: res.history.map(auditDtoToDisplay),
        source: "live",
      };
    }
  }
  const rec = DEMO.find(
    (r) =>
      r.cluster === args.cluster &&
      r.ns === args.namespace &&
      r.workload === args.name,
  ) ?? null;
  return {
    rec,
    history: [
      { id: "aud-w-3", at: "2026-04-23 09:17:48", policy: `${args.namespace}/${args.name}`, action: "rightsize.apply · cpu.req 16 → 1", cluster: args.cluster, outcome: "applied", effectK: 8.6 },
      { id: "aud-w-2", at: "2026-04-23 09:14:02", policy: `${args.namespace}/${args.name}`, action: "rec evaluated · safe to apply", cluster: args.cluster, outcome: "armed" },
      { id: "aud-w-1", at: "2026-04-23 08:00:00", policy: `${args.namespace}/${args.name}`, action: "scan · waste signal detected", cluster: args.cluster, outcome: "armed" },
    ],
    source: "demo",
  };
}
