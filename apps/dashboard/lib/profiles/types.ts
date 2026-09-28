// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// ProfilesService wire shapes (observe.proto) and view models.

import type { FlameNode } from "@/lib/flame/tree";

export type I64 = string | number;

export type ProfileTargetJson = {
  service?: string;
  namespace?: string;
  workload?: string;
  types?: string[];
  origin?: string;
  lastSeenUnixMs?: I64;
  cpuCoresAvg?: number;
  costUsdMonth?: number;
};

export type FlameNodeJson = {
  name?: string;
  parent?: number;
  depth?: number;
  self?: I64;
  total?: I64;
  baselineSelf?: I64;
  baselineTotal?: I64;
};

export type GetFlamegraphResponseJson = {
  nodes?: FlameNodeJson[];
  total?: I64;
  unit?: string;
  type?: string;
  baselineTotal?: I64;
  samples?: I64;
  costUsdMonth?: number;
};

export type TopFunctionJson = {
  name?: string;
  self?: I64;
  total?: I64;
  selfPct?: number;
  totalPct?: number;
  selfCostUsdMonth?: number;
};

export type GetTopFunctionsResponseJson = { functions?: TopFunctionJson[]; total?: I64; unit?: string };

export type ProfileTarget = {
  service: string;
  namespace: string;
  workload: string;
  types: string[];
  origin: string;
  lastSeenMs: number;
  cpuCoresAvg: number;
  costUsdMonth: number;
};

export type Flamegraph = {
  nodes: FlameNode[];
  total: number;
  unit: string;
  type: string;
  baselineTotal: number;
  samples: number;
  costUsdMonth: number;
};

export type TopFunction = {
  name: string;
  self: number;
  total: number;
  selfPct: number;
  totalPct: number;
  selfCostUsdMonth: number;
};
