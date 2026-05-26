// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, listAnomalies } from "./client";
import type { AnomalyDTO } from "./types";

export type Anomaly = AnomalyDTO;

const DEMO: Anomaly[] = [
  {
    id: "anom-7c91",
    kind: "spend",
    title: "ml-inference spend +34% w/w",
    subject: "ml-inference",
    detail: "model-server-a100 + retrieval-indexer carry 78% of the increase",
    deltaPct: 34,
    impactUsdMonth: 18200,
    severity: "warn",
    source: "demo",
    linkPath: "/workloads/aks-westeu-prod-01/ml-inference/model-server-a100",
  },
  {
    id: "anom-3a02",
    kind: "capacity",
    title: "gke-euw4-batch: 12 pending pods, last 4h",
    subject: "gke-euw4-batch",
    detail: "etl-nightly couldn't schedule against current nodepool capacity",
    deltaPct: 0,
    impactUsdMonth: 6100,
    severity: "warn",
    source: "demo",
    linkPath: "/clusters/gke-euw4-batch",
  },
  {
    id: "anom-c4f8",
    kind: "posture",
    title: "3 new criticals this week ($31k/mo at risk)",
    subject: "fleet-wide",
    detail: "CVE-2024-3094 (xz-utils) on vectordb-ingress is the highest cost × CVSS",
    deltaPct: 0,
    impactUsdMonth: 31000,
    severity: "critical",
    source: "demo",
    linkPath: "/posture?severity=critical",
  },
  {
    id: "anom-1d52",
    kind: "spend",
    title: "frontend-gateway cpu.req 4× over use",
    subject: "edge / frontend-gateway",
    detail: "30-day rolling avg use is 1.6 cores against a 8-core request",
    deltaPct: 0,
    impactUsdMonth: 4200,
    severity: "info",
    source: "demo",
    linkPath: "/workloads/gke-usc1-prod/edge/frontend-gateway",
  },
];

export type AnomaliesState = {
  anomalies: Anomaly[];
  source: "live" | "demo";
};

export async function getAnomalies(opts: { window?: string; limit?: number } = {}): Promise<AnomaliesState> {
  if (!isLive()) {
    return { anomalies: DEMO, source: "demo" };
  }
  const res = await listAnomalies(opts);
  if (!res || res.anomalies.length === 0) {
    return { anomalies: DEMO, source: "demo" };
  }
  return { anomalies: res.anomalies, source: "live" };
}
