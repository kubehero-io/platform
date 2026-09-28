// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// CostService (allocation, series, rightsizing, efficiency) with the
// dashboard's degrade-to-demo contract: unset / failing control plane →
// demo fixtures from lib/demo/cost.ts, labelled with the reason.

import "server-only";
import { demoAllocation, demoCostSeries, demoEfficiency, demoRightsizing } from "@/lib/demo/cost";
import { toAllocationView, toCostSeriesView, toEfficiencyView, toRightsizingView } from "@/lib/cost/map";
import type {
  Aggregate,
  AllocationView,
  CostSeriesView,
  EfficiencyView,
  GetAllocationResponseJson,
  GetCostTimeseriesResponseJson,
  GetEfficiencyResponseJson,
  ListRightsizingResponseJson,
  RightsizingView,
} from "@/lib/cost/types";
import { costWindowRange, isCostWindow } from "@/lib/time-range";
import { callUnary, type RpcError } from "./rpc";
import { demo, live, type Sourced } from "./source";

const SERVICE = "kubehero.v1.CostService";

function degrade<T>(err: RpcError, fixture: () => T): Sourced<T> {
  return err.code === "not_configured" ? demo(fixture(), "unset") : demo(fixture(), "error", `${err.code}: ${err.message}`.slice(0, 160));
}

export type AllocationQuery = {
  window: string;
  aggregate: Aggregate;
  filters?: Record<string, string>;
  includeIdle?: boolean;
  shareIdle?: "" | "weighted" | "even";
  sharedNamespaces?: string[];
  clusterId?: string;
  /** Force demo fixtures (?demo=1 preview). */
  forceDemo?: boolean;
};

export async function getAllocation(q: AllocationQuery): Promise<Sourced<AllocationView>> {
  const fixture = () => demoAllocation(q);
  if (q.forceDemo) return demo(fixture(), "forced");
  const window = isCostWindow(q.window) ? q.window : "7d";
  const res = await callUnary<GetAllocationResponseJson>("cp", SERVICE, "GetAllocation", {
    window,
    aggregate: [q.aggregate],
    filters: q.filters ?? {},
    includeIdle: q.includeIdle ?? false,
    shareIdle: q.shareIdle ?? "",
    sharedNamespaces: q.sharedNamespaces ?? [],
    clusterId: q.clusterId ?? "",
  });
  if (!res.ok) return degrade(res.error, fixture);
  const view = toAllocationView(res.data, costWindowRange(window));
  if (res.data.source === "demo") return demo(view, "upstream");
  // An install that has never ingested a sample returns nothing at all
  // for an unfiltered query — show the fixtures, labelled, instead of a
  // blank page. A filtered query that matches nothing is a real answer.
  if (view.rows.length === 0 && Object.keys(q.filters ?? {}).length === 0) return demo(fixture(), "empty");
  return live(view);
}

export type SeriesQuery = {
  window?: string;
  groupBy?: string;
  filters?: Record<string, string>;
  top?: number;
  clusterId?: string;
  forceDemo?: boolean;
};

export async function getCostTimeseries(q: SeriesQuery): Promise<Sourced<CostSeriesView>> {
  const fixture = () => demoCostSeries(q);
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<GetCostTimeseriesResponseJson>("cp", SERVICE, "GetCostTimeseries", {
    window: q.window ?? "30d",
    groupBy: q.groupBy ?? "",
    filters: q.filters ?? {},
    top: q.top ?? 7,
    clusterId: q.clusterId ?? "",
  });
  if (!res.ok) return degrade(res.error, fixture);
  const view = toCostSeriesView(res.data, q.groupBy ?? "");
  if (res.data.source === "demo") return demo(view, "upstream");
  if (view.times.length === 0 && Object.keys(q.filters ?? {}).length === 0) return demo(fixture(), "empty");
  return live(view);
}

export type RightsizingQuery = {
  clusterId?: string;
  namespace?: string;
  window?: string;
  headroomPct?: number;
  cpuPercentile?: "p90" | "p95" | "p99" | "max";
  minSavingsUsdMonth?: number;
  limit?: number;
  forceDemo?: boolean;
};

export async function listRightsizing(q: RightsizingQuery = {}): Promise<Sourced<RightsizingView>> {
  const fixture = () => demoRightsizing(q);
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<ListRightsizingResponseJson>("cp", SERVICE, "ListRightsizing", {
    clusterId: q.clusterId ?? "",
    namespace: q.namespace ?? "",
    window: q.window ?? "7d",
    headroomPct: q.headroomPct ?? 15,
    cpuPercentile: q.cpuPercentile ?? "p95",
    minSavingsUsdMonth: q.minSavingsUsdMonth ?? 0,
    limit: q.limit ?? 500,
  });
  if (!res.ok) return degrade(res.error, fixture);
  const view = toRightsizingView(res.data);
  if (res.data.source === "demo") return demo(view, "upstream");
  // Zero recommendations on a live install is a legitimate answer
  // ("everything fits") — never paper over it with fixtures.
  return live(view);
}

export async function getEfficiency(q: { clusterId?: string; window?: string; forceDemo?: boolean } = {}): Promise<Sourced<EfficiencyView>> {
  const fixture = () => demoEfficiency(q);
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<GetEfficiencyResponseJson>("cp", SERVICE, "GetEfficiency", {
    clusterId: q.clusterId ?? "",
    window: q.window ?? "7d",
  });
  if (!res.ok) return degrade(res.error, fixture);
  const view = toEfficiencyView(res.data);
  if (res.data.source === "demo") return demo(view, "upstream");
  return live(view);
}
