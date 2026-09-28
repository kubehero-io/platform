// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// What the shell (sidebar badges, footer, cluster switcher, ⌘K) shows,
// in one small payload behind /api/nav. The rule for every number here:
// it is real data or it is omitted. Demo-mode numbers are the demo
// dataset every page shows (and the footer says "demo"); a live install
// whose RPC failed gets `null` — never a fixture dressed up as live.

import "server-only";
import { createHash } from "node:crypto";
import { authMode, type AuthMode } from "@/lib/auth-mode";
import type { Cluster } from "@/lib/fleet-data";
import type { Role } from "@/lib/roles";
import { getSession } from "@/lib/session";
import { listAlerts } from "./alerts";
import { healthCheck } from "./client";
import { getFleet } from "./clusters";
import { listRightsizing } from "./cost";
import { resolveCredential, upstreamBase } from "./rpc";
import type { Sourced } from "./source";

export type NavStatus = {
  health: "live" | "demo" | "unreachable";
  firing: number | null;
  pending: number | null;
  recommendations: number | null;
  clusters: Pick<Cluster, "id" | "name" | "cloud" | "region" | "nodes" | "state">[];
  nodes: number;
  fleetSource: "live" | "demo";
  mode: AuthMode;
  role: Role | null;
};

// Rightsizing is the expensive call; its count changes slowly. Cache it
// per credential (hashed — tokens never become map keys) for 5 minutes.
const RECS_TTL_MS = 5 * 60_000;
const recsCache = new Map<string, { at: number; n: number | null }>();

async function recommendationsCount(health: NavStatus["health"]): Promise<number | null> {
  const cred = await resolveCredential("cp");
  const key = createHash("sha256").update(cred.header ?? "anon").digest("hex").slice(0, 16);
  const hit = recsCache.get(key);
  if (hit && Date.now() - hit.at < RECS_TTL_MS) return hit.n;
  const r = await listRightsizing({ minSavingsUsdMonth: 25, limit: 500 });
  const n = countable(r, health) ? r.data.recs.length : null;
  if (recsCache.size > 64) recsCache.clear();
  recsCache.set(key, { at: Date.now(), n });
  return n;
}

/** A number is shown if it is live, or if the whole dashboard is in demo mode. */
function countable<T>(s: Sourced<T>, health: NavStatus["health"]): boolean {
  return s.source === "live" || health === "demo";
}

export async function getNavStatus(): Promise<NavStatus> {
  const session = await getSession();
  let health: NavStatus["health"] = "demo";
  if (upstreamBase("cp")) {
    const h = await healthCheck();
    health = h ? "live" : "unreachable";
  }
  const [alerts, fleet, recs] = await Promise.all([listAlerts({ limit: 1 }), getFleet(), recommendationsCount(health)]);
  const alertsOk = countable(alerts, health);
  const clusters = fleet.source === "live" || health === "demo" ? fleet.clusters : [];
  return {
    health,
    firing: alertsOk ? alerts.data.firing : null,
    pending: alertsOk ? alerts.data.pending : null,
    recommendations: recs,
    clusters: clusters.map(({ id, name, cloud, region, nodes, state }) => ({ id, name, cloud, region, nodes, state })),
    nodes: clusters.reduce((s, c) => s + c.nodes, 0),
    fleetSource: fleet.source,
    mode: authMode(),
    role: session?.role ?? null,
  };
}
