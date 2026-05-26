// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, getTeamSpend } from "./client";
import type { TeamSpendDTO } from "./types";

export type TeamRow = {
  name: string;
  costCenter: string;
  spendMonthK: number;
  recoverableK: number;
  gpuIdleK: number;
  clouds: { aws: number; gcp: number; azure: number };
};

const DEMO: TeamRow[] = [
  { name: "ml-inference", costCenter: "ml-platform", spendMonthK: 82,  recoverableK: 18.2, gpuIdleK: 14.8, clouds: { aws: 45, gcp:  0, azure: 37 } },
  { name: "retrieval",    costCenter: "ml-platform", spendMonthK: 34,  recoverableK:  8.6, gpuIdleK:  3.2, clouds: { aws: 22, gcp: 12, azure:  0 } },
  { name: "data",         costCenter: "analytics",   spendMonthK: 22,  recoverableK:  6.1, gpuIdleK:  0,   clouds: { aws:  6, gcp: 16, azure:  0 } },
  { name: "edge",         costCenter: "platform",    spendMonthK: 21,  recoverableK:  7.6, gpuIdleK:  0,   clouds: { aws: 10, gcp:  3, azure:  8 } },
  { name: "platform",     costCenter: "platform",    spendMonthK:  4,  recoverableK:  0.8, gpuIdleK:  0,   clouds: { aws:  2, gcp:  1.5, azure: 0.5 } },
];

function dtoToRow(t: TeamSpendDTO): TeamRow {
  return {
    name: t.team,
    costCenter: t.costCenter,
    spendMonthK: t.spendUsdMonth / 1000,
    recoverableK: t.recoverableUsdMonth / 1000,
    gpuIdleK: t.gpuIdleUsdMonth / 1000,
    clouds: {
      aws: t.awsUsdMonth / 1000,
      gcp: t.gcpUsdMonth / 1000,
      azure: t.azureUsdMonth / 1000,
    },
  };
}

export type ChargebackState = {
  teams: TeamRow[];
  fleetTotalK: number;
  fleetRecoverableK: number;
  source: "live" | "demo";
};

export async function getChargeback(window: string = "30d"): Promise<ChargebackState> {
  if (!isLive()) {
    return {
      teams: DEMO,
      fleetTotalK: DEMO.reduce((a, t) => a + t.spendMonthK, 0),
      fleetRecoverableK: DEMO.reduce((a, t) => a + t.recoverableK, 0),
      source: "demo",
    };
  }
  const res = await getTeamSpend({ window });
  if (!res || res.teams.length === 0) {
    return {
      teams: DEMO,
      fleetTotalK: DEMO.reduce((a, t) => a + t.spendMonthK, 0),
      fleetRecoverableK: DEMO.reduce((a, t) => a + t.recoverableK, 0),
      source: "demo",
    };
  }
  return {
    teams: res.teams.map(dtoToRow),
    fleetTotalK: res.fleetTotalUsdMonth / 1000,
    fleetRecoverableK: res.fleetRecoverableUsdMonth / 1000,
    source: "live",
  };
}
