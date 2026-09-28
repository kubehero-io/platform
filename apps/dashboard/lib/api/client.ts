// SPDX-License-Identifier: BUSL-1.1
// Connect-RPC client for the control-plane's ControlPlaneService.
//
// Behavior:
//   · CONTROL_PLANE_URL set  → real Connect call to the Go server
//   · unset                  → returns null (callers serve demo data)
//
// Transport, credentials, deadlines and error mapping live in rpc.ts;
// this file only names the RPCs.

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
import { callOrNull, callUnary, upstreamBase, type CallOptions, type RpcResult } from "./rpc";

const SERVICE = "kubehero.v1.ControlPlaneService";
const PRICING = "kubehero.v1.PricingService";

function rpc<Req, Res>(service: string, method: string, req: Req): Promise<Res | null> {
  return callOrNull<Res>("cp", service, method, req);
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
  return upstreamBase("cp") !== null;
}

// ─── WhoAmI ─────────────────────────────────────────────────────────────

export type WhoAmIResponse = {
  subject?: string;
  role?: string;
  email?: string;
  groups?: string[];
  clusterId?: string;
  authRequired?: boolean;
};

/** Resolves the principal behind a credential — used by the token sign-in. */
export async function whoAmI(opts: CallOptions): Promise<RpcResult<WhoAmIResponse>> {
  const res = await callUnary<WhoAmIResponse>("cp", SERVICE, "WhoAmI", {}, { timeoutMs: 5_000, onExpired: "return", ...opts });
  // proto3 JSON leaves false booleans out: a control plane that accepts
  // anonymous callers sends no authRequired at all.
  return res.ok ? { ok: true, data: { ...res.data, authRequired: res.data.authRequired === true } } : res;
}

// ─── ArmPolicy (admin) ───────────────────────────────────────────────────

export type ArmPolicyResponse = {
  policyName?: string;
  armed?: boolean;
  effectiveAtUnix?: string | number;
  auditId?: string;
};

export function armPolicy(args: {
  clusterId?: string;
  policyName: string;
  armed: boolean;
  reason?: string;
}): Promise<RpcResult<ArmPolicyResponse>> {
  return callUnary<ArmPolicyResponse>("cp", SERVICE, "ArmPolicy", args);
}
