// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, describe, expect, it, vi } from "vitest";
import { getCluster, getFleet } from "./clusters";
import { CLUSTERS } from "@/lib/fleet-data";
import type { ListClustersResponse } from "./types";

const CP_URL = "http://cp.test:18080";

function stubListClusters(res: ListClustersResponse) {
  vi.stubEnv("CONTROL_PLANE_URL", CP_URL);
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({ ok: true, json: async () => res })),
  );
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe("getFleet", () => {
  it("serves the demo fixture when CONTROL_PLANE_URL is unset", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const fleet = await getFleet();
    expect(fleet.source).toBe("demo");
    expect(fleet.clusters).toEqual(CLUSTERS);
  });

  it("falls back to demo when the control-plane returns no clusters", async () => {
    stubListClusters({ clusters: [], nextPageToken: "" });
    const fleet = await getFleet();
    expect(fleet.source).toBe("demo");
    expect(fleet.clusters).toEqual(CLUSTERS);
  });

  it("serves live clusters, enriched from the demo set by id", async () => {
    stubListClusters({
      clusters: [
        // Known id — narrative fields (cost, gpu, state) come from the fixture.
        { id: "eks-use1-prod", name: "eks-use1-prod", cloud: "aws", region: "us-east-1", nodes: 999 },
        // Unknown id — placeholders.
        { id: "gke-new-01", name: "gke-new-01", cloud: "gcp", region: "us-west1", nodes: 3 },
      ],
      nextPageToken: "",
    });
    const fleet = await getFleet();
    expect(fleet.source).toBe("live");
    expect(fleet.clusters).toHaveLength(2);

    const known = fleet.clusters[0];
    expect(known).toMatchObject({
      id: "eks-use1-prod",
      cloud: "EKS",
      nodes: 999, // live value wins
      costDay: "$12,940", // enriched from fixture
      state: "critical",
      gpu: "32× H100",
    });

    const fresh = fleet.clusters[1];
    expect(fresh).toMatchObject({
      id: "gke-new-01",
      cloud: "GKE",
      nodes: 3,
      costDay: "—",
      recoverable: "—",
      gpu: "—",
      state: "healthy",
    });
  });
});

describe("getCluster", () => {
  it("finds a demo cluster by id", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const { cluster, source } = await getCluster("aks-westeu-prod-01");
    expect(source).toBe("demo");
    expect(cluster?.name).toBe("aks-westeu-prod-01");
  });

  it("returns null for an unknown id", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const { cluster } = await getCluster("nope");
    expect(cluster).toBeNull();
  });
});
