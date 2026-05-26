// SPDX-License-Identifier: BUSL-1.1
// Mirror of the Connect proto schema so the dashboard can stay typed
// against control-plane responses without importing the workspace
// proto package directly. Hand-curated; if the .proto changes, update
// here too. The shape matches exactly what the Go server returns.

export type Cloud = "aws" | "gcp" | "azure";

export type ClusterDTO = {
  id: string;
  name: string;
  cloud: Cloud;
  region: string;
  nodes: number;
};

export type ListClustersResponse = {
  clusters: ClusterDTO[];
  nextPageToken: string;
};

export type HealthCheckResponse = {
  status: string;
  version: string;
};

export type QuoteResponse = {
  pricePerHour: number;
  currency: string;
};

// Display types — augmented for the dashboard UI. The server returns
// the lower-cased shape; we map it here.
export type Cluster = {
  id: string;
  name: string;
  cloud: "AKS" | "GKE" | "EKS";
  region: string;
  nodes: number;
  costDay: string;
  recoverable: string;
  state: "healthy" | "warn" | "critical";
  gpu: string;
};

const CLOUD_DISPLAY: Record<Cloud, "AKS" | "GKE" | "EKS"> = {
  aws: "EKS",
  gcp: "GKE",
  azure: "AKS",
};

export function dtoToDisplay(c: ClusterDTO, fallback?: Partial<Cluster>): Cluster {
  return {
    id: c.id,
    name: c.name,
    cloud: CLOUD_DISPLAY[c.cloud] ?? "EKS",
    region: c.region,
    nodes: c.nodes,
    costDay: fallback?.costDay ?? "—",
    recoverable: fallback?.recoverable ?? "—",
    state: fallback?.state ?? "healthy",
    gpu: fallback?.gpu ?? "—",
  };
}

// ─── ListAuditLog ─────────────────────────────────────────────────────────

export type AuditEntryDTO = {
  id: string;
  at: string;
  policy: string;
  action: string;
  cluster: string;
  outcome: "applied" | "armed" | "cooldown" | "reverted" | string;
  effectUsdMonth: number;
};

export type ListAuditLogResponse = {
  entries: AuditEntryDTO[];
};

// ─── ListWasteRecommendations ────────────────────────────────────────────

export type WasteRecommendationDTO = {
  rank: string;
  workload: string;
  namespace: string;
  cluster: string;
  cloud: string;        // AKS | GKE | EKS
  signal: string;
  recoverableUsdMonth: number;
  action: "apply" | "review" | string;
  severity: "accent" | "warn" | string;
};

export type ListWasteRecommendationsResponse = {
  recommendations: WasteRecommendationDTO[];
};

export type GetWorkloadResponse = {
  recommendation?: WasteRecommendationDTO;
  history: AuditEntryDTO[];
};

// ─── ListPolicies ─────────────────────────────────────────────────────────

export type PolicyDTO = {
  name: string;
  scope: string;
  ceilingUsd: number;
  spentPct: number;
  kind: "BudgetPolicy" | "CeilingPolicy" | string;
  armed: boolean;
};

export type ListPoliciesResponse = {
  policies: PolicyDTO[];
};

// ─── GetTeamSpend ─────────────────────────────────────────────────────────

export type TeamSpendDTO = {
  team: string;
  costCenter: string;
  spendUsdMonth: number;
  recoverableUsdMonth: number;
  gpuIdleUsdMonth: number;
  awsUsdMonth: number;
  gcpUsdMonth: number;
  azureUsdMonth: number;
};

export type GetTeamSpendResponse = {
  teams: TeamSpendDTO[];
  fleetTotalUsdMonth: number;
  fleetRecoverableUsdMonth: number;
};

// ─── ListVulnerabilities ─────────────────────────────────────────────────

export type Severity = "critical" | "high" | "medium" | "low";

export type VulnerabilityDTO = {
  id: string;
  severity: Severity | string;
  workload: string;
  namespace: string;
  cluster: string;
  image: string;
  packageName: string;
  installedVersion: string;
  fixedVersion: string;
  cvss: number;
  costUsdMonth: number;
  source: string;
  firstSeen: string;
};

export type ListVulnerabilitiesResponse = {
  vulnerabilities: VulnerabilityDTO[];
  criticalCount: number;
  highCount: number;
  mediumCount: number;
  lowCount: number;
};

// ─── ListAnomalies ───────────────────────────────────────────────────────

export type AnomalyDTO = {
  id: string;
  kind: "spend" | "capacity" | "posture" | string;
  title: string;
  subject: string;
  detail: string;
  deltaPct: number;
  impactUsdMonth: number;
  severity: "info" | "warn" | "critical" | string;
  source: string;
  linkPath: string;
};

export type ListAnomaliesResponse = {
  anomalies: AnomalyDTO[];
  total: number;
};

// ─── ListCapacityDemands ─────────────────────────────────────────────────

export type CapacityDemandDTO = {
  id: string;
  cluster: string;
  namespace: string;
  workload: string;
  pendingPods: number;
  requestedCpu: string;
  requestedMem: string;
  requestedGpu: string;
  oldestPendingAge: string;
  recommendedAction: string;
  recommendedCostUsdMonth: number;
  blockedCostUsdMonth: number;
  source: string;
};

export type ListCapacityDemandsResponse = {
  demands: CapacityDemandDTO[];
  totalPendingPods: number;
  totalBlockedUsdMonth: number;
};
