// SPDX-License-Identifier: BUSL-1.1
// Connect-RPC client for the control-plane.
//
// Behavior:
//   · CONTROL_PLANE_URL set  → real Connect call to the Go server
//   · unset                  → returns mock data (dashboard demo mode)
//
// Connect's wire protocol is plain HTTP/2 with `Content-Type: application/proto`
// or `application/json`. We use JSON here so we can curl + grep the responses
// during development. Both are fine — switch to proto when bandwidth matters.

import "server-only";
import {
  type ClusterDTO,
  type GetTeamSpendResponse,
  type GetWorkloadResponse,
  type HealthCheckResponse,
  type ListAnomaliesResponse,
  type ListAuditLogResponse,
  type ListCapacityDemandsResponse,
  type ListClustersResponse,
  type ListPoliciesResponse,
  type ListVulnerabilitiesResponse,
  type ListWasteRecommendationsResponse,
  type QuoteResponse,
} from "./types";

const SERVICE = "kubehero.v1.ControlPlaneService";
const PRICING = "kubehero.v1.PricingService";

function endpoint(): string | null {
  const v = process.env.CONTROL_PLANE_URL?.trim();
  return v && v.length > 0 ? v.replace(/\/$/, "") : null;
}

async function rpc<Req, Res>(
  service: string,
  method: string,
  req: Req,
  signal?: AbortSignal,
): Promise<Res | null> {
  const base = endpoint();
  if (!base) return null;
  try {
    const r = await fetch(`${base}/${service}/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Connect-Protocol-Version": "1",
      },
      body: JSON.stringify(req ?? {}),
      cache: "no-store",
      signal,
    });
    if (!r.ok) {
      console.error("[control-plane]", method, r.status, await r.text());
      return null;
    }
    return (await r.json()) as Res;
  } catch (err) {
    console.error("[control-plane] rpc failed", method, err);
    return null;
  }
}

export async function listClusters(pageSize = 100): Promise<{
  clusters: ClusterDTO[];
  nextPageToken: string;
} | null> {
  return rpc<unknown, ListClustersResponse>(SERVICE, "ListClusters", { pageSize });
}

export async function healthCheck(): Promise<HealthCheckResponse | null> {
  return rpc<unknown, HealthCheckResponse>(SERVICE, "HealthCheck", {});
}

export async function quote(args: {
  cloud: string;
  sku: string;
  region: string;
  lifecycle: string;
}): Promise<QuoteResponse | null> {
  return rpc<typeof args, QuoteResponse>(PRICING, "Quote", args);
}

export async function listAuditLog(args: {
  clusterId?: string;
  limit?: number;
  outcome?: string;
} = {}): Promise<ListAuditLogResponse | null> {
  return rpc<typeof args, ListAuditLogResponse>(SERVICE, "ListAuditLog", args);
}

export async function listWasteRecommendations(args: {
  clusterId?: string;
  limit?: number;
} = {}): Promise<ListWasteRecommendationsResponse | null> {
  return rpc<typeof args, ListWasteRecommendationsResponse>(SERVICE, "ListWasteRecommendations", args);
}

export async function getWorkload(args: {
  cluster: string;
  namespace: string;
  name: string;
}): Promise<GetWorkloadResponse | null> {
  return rpc<typeof args, GetWorkloadResponse>(SERVICE, "GetWorkload", args);
}

export async function listPolicies(args: {
  clusterId?: string;
  kind?: string;
} = {}): Promise<ListPoliciesResponse | null> {
  return rpc<typeof args, ListPoliciesResponse>(SERVICE, "ListPolicies", args);
}

export async function getTeamSpend(args: {
  window?: string;
} = {}): Promise<GetTeamSpendResponse | null> {
  return rpc<typeof args, GetTeamSpendResponse>(SERVICE, "GetTeamSpend", args);
}

export async function listVulnerabilities(args: {
  clusterId?: string;
  severity?: string;
  limit?: number;
} = {}): Promise<ListVulnerabilitiesResponse | null> {
  return rpc<typeof args, ListVulnerabilitiesResponse>(SERVICE, "ListVulnerabilities", args);
}

export async function listAnomalies(args: {
  scope?: string;
  window?: string;
  limit?: number;
} = {}): Promise<ListAnomaliesResponse | null> {
  return rpc<typeof args, ListAnomaliesResponse>(SERVICE, "ListAnomalies", args);
}

export async function listCapacityDemands(args: {
  clusterId?: string;
  limit?: number;
} = {}): Promise<ListCapacityDemandsResponse | null> {
  return rpc<typeof args, ListCapacityDemandsResponse>(SERVICE, "ListCapacityDemands", args);
}

export function isLive(): boolean {
  return endpoint() !== null;
}
