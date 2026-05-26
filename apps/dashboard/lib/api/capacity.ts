// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, listCapacityDemands } from "./client";
import type { CapacityDemandDTO } from "./types";

export type CapacityDemand = CapacityDemandDTO;

const DEMO: CapacityDemand[] = [
  {
    id: "demand-gke-batch-a100",
    cluster: "gke-euw4-batch", namespace: "data", workload: "etl-nightly",
    pendingPods: 12, requestedCpu: "96 cores", requestedMem: "768 GiB", requestedGpu: "",
    oldestPendingAge: "4h 12m",
    recommendedAction: "scale nodepool n2-standard-32 by 3 nodes",
    recommendedCostUsdMonth: 1840, blockedCostUsdMonth: 6100, source: "demo",
  },
  {
    id: "demand-aks-ml-a100",
    cluster: "aks-westeu-prod-01", namespace: "ml-inference", workload: "model-server-a100-canary",
    pendingPods: 2, requestedCpu: "32 cores", requestedMem: "256 GiB", requestedGpu: "2× A100",
    oldestPendingAge: "1h 3m",
    recommendedAction: "scale nodepool ml-a100 by 1 node",
    recommendedCostUsdMonth: 4100, blockedCostUsdMonth: 18200, source: "demo",
  },
  {
    id: "demand-eks-retrieval",
    cluster: "eks-use1-prod", namespace: "retrieval", workload: "retrieval-indexer",
    pendingPods: 4, requestedCpu: "16 cores", requestedMem: "64 GiB", requestedGpu: "",
    oldestPendingAge: "22m",
    recommendedAction: "scale nodepool m5-2xlarge by 2 nodes",
    recommendedCostUsdMonth: 560, blockedCostUsdMonth: 4800, source: "demo",
  },
];

export type CapacityState = {
  demands: CapacityDemand[];
  totalPendingPods: number;
  totalBlockedUsdMonth: number;
  source: "live" | "demo";
};

export async function getCapacity(opts: { clusterId?: string; limit?: number } = {}): Promise<CapacityState> {
  if (!isLive()) {
    return aggregate(DEMO, "demo");
  }
  const res = await listCapacityDemands(opts);
  if (!res || res.demands.length === 0) {
    return aggregate(DEMO, "demo");
  }
  return {
    demands: res.demands,
    totalPendingPods: res.totalPendingPods,
    totalBlockedUsdMonth: res.totalBlockedUsdMonth,
    source: "live",
  };
}

function aggregate(demands: CapacityDemand[], source: "live" | "demo"): CapacityState {
  const totalPendingPods = demands.reduce((s, d) => s + d.pendingPods, 0);
  const totalBlockedUsdMonth = demands.reduce((s, d) => s + d.blockedCostUsdMonth, 0);
  return { demands, totalPendingPods, totalBlockedUsdMonth, source };
}
