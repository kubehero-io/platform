// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, listVulnerabilities } from "./client";
import type { VulnerabilityDTO } from "./types";

export type Vulnerability = VulnerabilityDTO;

const DEMO: Vulnerability[] = [
  { id: "CVE-2024-21626", severity: "critical", workload: "model-server-a100", namespace: "ml-inference", cluster: "aks-westeu-prod-01", image: "nvcr.io/nvidia/tritonserver:24.09-py3", packageName: "runc", installedVersion: "1.1.7", fixedVersion: "1.1.12", cvss: 8.6, costUsdMonth: 18200, source: "trivy", firstSeen: "2026-04-22T08:00:00Z" },
  { id: "CVE-2024-3094",  severity: "critical", workload: "vectordb-ingress",  namespace: "retrieval",    cluster: "eks-use1-prod",      image: "ghcr.io/qdrant/qdrant:v1.10.0",        packageName: "xz-utils",        installedVersion: "5.6.0",  fixedVersion: "5.6.2",  cvss: 10.0, costUsdMonth: 8600,  source: "trivy", firstSeen: "2026-04-21T12:14:00Z" },
  { id: "CVE-2024-6387",  severity: "critical", workload: "frontend-gateway",  namespace: "edge",         cluster: "gke-usc1-prod",      image: "nginx:1.25.3",                          packageName: "openssh-server",  installedVersion: "9.5p1",  fixedVersion: "9.8p1",  cvss: 8.1, costUsdMonth: 4200,  source: "trivy", firstSeen: "2026-04-20T03:00:00Z" },
  { id: "CVE-2024-26690", severity: "high",     workload: "etl-nightly",       namespace: "data",         cluster: "gke-euw4-batch",     image: "apache/airflow:2.9.2",                  packageName: "linux-libc-dev",  installedVersion: "5.15.0-91", fixedVersion: "5.15.0-118", cvss: 7.8, costUsdMonth: 6100, source: "trivy", firstSeen: "2026-04-19T14:40:00Z" },
  { id: "CVE-2024-37891", severity: "high",     workload: "metrics-scraper",   namespace: "platform",     cluster: "aks-ne-staging",     image: "prom/prometheus:v2.51.0",               packageName: "urllib3",          installedVersion: "2.1.0",  fixedVersion: "2.2.2",  cvss: 7.5, costUsdMonth: 3900, source: "trivy", firstSeen: "2026-04-18T09:30:00Z" },
  { id: "CVE-2024-2961",  severity: "high",     workload: "api-ingress",       namespace: "edge",         cluster: "aks-westeu-prod-01", image: "envoyproxy/envoy:v1.30.1",               packageName: "glibc",            installedVersion: "2.31-13", fixedVersion: "2.31-13+deb11u11", cvss: 7.3, costUsdMonth: 3400, source: "trivy", firstSeen: "2026-04-22T17:11:00Z" },
  { id: "CVE-2024-21891", severity: "high",     workload: "queue-consumer",    namespace: "data",         cluster: "eks-use1-prod",      image: "node:20.10-alpine",                      packageName: "node",             installedVersion: "20.10.0", fixedVersion: "20.11.1", cvss: 7.4, costUsdMonth: 2600, source: "trivy", firstSeen: "2026-04-15T10:00:00Z" },
  { id: "CVE-2024-28182", severity: "medium",   workload: "retrieval-indexer", namespace: "retrieval",    cluster: "eks-use1-prod",      image: "ghcr.io/qdrant/qdrant:v1.10.0",         packageName: "nghttp2",          installedVersion: "1.55.1", fixedVersion: "1.61.0", cvss: 5.4, costUsdMonth: 4800, source: "trivy", firstSeen: "2026-04-23T01:10:00Z" },
];

export type PostureState = {
  vulnerabilities: Vulnerability[];
  counts: { critical: number; high: number; medium: number; low: number };
  source: "live" | "demo";
};

function tally(vs: Vulnerability[]) {
  const c = { critical: 0, high: 0, medium: 0, low: 0 } as PostureState["counts"];
  for (const v of vs) {
    if (v.severity === "critical") c.critical++;
    else if (v.severity === "high") c.high++;
    else if (v.severity === "medium") c.medium++;
    else if (v.severity === "low") c.low++;
  }
  return c;
}

export async function getPosture(args: {
  severity?: string;
  clusterId?: string;
  limit?: number;
} = {}): Promise<PostureState> {
  if (!isLive()) {
    const filtered = filterDemo(DEMO, args);
    return { vulnerabilities: filtered, counts: tally(DEMO), source: "demo" };
  }
  const res = await listVulnerabilities(args);
  if (!res || res.vulnerabilities.length === 0) {
    const filtered = filterDemo(DEMO, args);
    return { vulnerabilities: filtered, counts: tally(DEMO), source: "demo" };
  }
  return {
    vulnerabilities: res.vulnerabilities,
    counts: {
      critical: res.criticalCount,
      high: res.highCount,
      medium: res.mediumCount,
      low: res.lowCount,
    },
    source: "live",
  };
}

function filterDemo(
  rows: Vulnerability[],
  args: { severity?: string; clusterId?: string; limit?: number },
): Vulnerability[] {
  let out = rows;
  if (args.severity) out = out.filter((v) => v.severity === args.severity);
  if (args.clusterId) out = out.filter((v) => v.cluster === args.clusterId);
  if (args.limit && args.limit > 0) out = out.slice(0, args.limit);
  return out;
}
