// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { CLUSTERS as DEMO_CLUSTERS, type Cluster } from "@/lib/fleet-data";
import { listClusters as rpcListClusters, isLive } from "./client";
import { dtoToDisplay } from "./types";

export type FleetState = {
  clusters: Cluster[];
  source: "live" | "demo";
};

// Demo clusters carry richer data (cost, recoverable, gpu) than the
// minimal Connect schema. When live data arrives, we merge by id so
// the table doesn't lose its narrative numbers until those fields land
// in the schema.
const DEMO_BY_ID = new Map(DEMO_CLUSTERS.map((c) => [c.id, c]));

export async function getFleet(): Promise<FleetState> {
  if (!isLive()) {
    return { clusters: DEMO_CLUSTERS, source: "demo" };
  }
  const res = await rpcListClusters(100);
  if (!res || res.clusters.length === 0) {
    return { clusters: DEMO_CLUSTERS, source: "demo" };
  }
  const merged: Cluster[] = res.clusters.map((c) =>
    dtoToDisplay(c, DEMO_BY_ID.get(c.id)),
  );
  return { clusters: merged, source: "live" };
}

export async function getCluster(id: string): Promise<{
  cluster: Cluster | null;
  source: "live" | "demo";
}> {
  const fleet = await getFleet();
  return {
    cluster: fleet.clusters.find((c) => c.id === id) ?? null,
    source: fleet.source,
  };
}
