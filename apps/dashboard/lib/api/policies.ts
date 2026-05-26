// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, listPolicies } from "./client";
import type { PolicyDTO } from "./types";
import type { BudgetRowData } from "@/components/interactive/budget-row";

const DEMO: BudgetRowData[] = [
  { name: "prod-monthly-ceiling", scope: "env=prod · all clusters", ceilingUSD: 100_000, spentPct: 78, kind: "BudgetPolicy" },
  { name: "ml-gpu-ceiling",       scope: "ns=ml-inference",         ceilingUSD:  45_000, spentPct: 62, kind: "BudgetPolicy" },
  { name: "prod-burn-rate-2x",    scope: "prod-us-east-1",          ceilingUSD:  10_000, spentPct: 40, kind: "CeilingPolicy" },
  { name: "gpu-inference-cap",    scope: "ns=ml-inference",         ceilingUSD:  18_200, spentPct: 88, kind: "CeilingPolicy" },
  { name: "staging-monthly",      scope: "env=staging",             ceilingUSD:  15_000, spentPct: 23, kind: "BudgetPolicy" },
];

function dtoToRow(p: PolicyDTO): BudgetRowData {
  const kind: BudgetRowData["kind"] =
    p.kind === "CeilingPolicy" ? "CeilingPolicy" : "BudgetPolicy";
  return {
    name: p.name,
    scope: p.scope,
    ceilingUSD: p.ceilingUsd,
    spentPct: p.spentPct,
    kind,
  };
}

export type PoliciesState = {
  rows: BudgetRowData[];
  source: "live" | "demo";
};

export async function getPolicies(opts: { kind?: string } = {}): Promise<PoliciesState> {
  if (!isLive()) {
    return { rows: DEMO, source: "demo" };
  }
  const res = await listPolicies(opts);
  if (!res || res.policies.length === 0) {
    return { rows: DEMO, source: "demo" };
  }
  return { rows: res.policies.map(dtoToRow), source: "live" };
}
