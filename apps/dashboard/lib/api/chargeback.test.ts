// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, describe, expect, it, vi } from "vitest";
import { getChargeback } from "./chargeback";
import type { GetTeamSpendResponse } from "./types";

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe("getChargeback (demo)", () => {
  it("aggregates fleet totals from the team rows", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const { teams, fleetTotalK, fleetRecoverableK, source } = await getChargeback();
    expect(source).toBe("demo");
    expect(teams.length).toBeGreaterThan(0);
    expect(fleetTotalK).toBeCloseTo(teams.reduce((a, t) => a + t.spendMonthK, 0), 6);
    expect(fleetRecoverableK).toBeCloseTo(teams.reduce((a, t) => a + t.recoverableK, 0), 6);
  });
});

describe("getChargeback (live)", () => {
  it("converts USD/month to $K rows and totals", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "http://cp.test:18080");
    const res: GetTeamSpendResponse = {
      teams: [
        {
          team: "ml-inference",
          costCenter: "ml-platform",
          spendUsdMonth: 82_000,
          recoverableUsdMonth: 18_200,
          gpuIdleUsdMonth: 14_800,
          awsUsdMonth: 45_000,
          gcpUsdMonth: 0,
          azureUsdMonth: 37_000,
        },
      ],
      fleetTotalUsdMonth: 82_000,
      fleetRecoverableUsdMonth: 18_200,
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({ ok: true, json: async () => res })),
    );

    const { teams, fleetTotalK, fleetRecoverableK, source } = await getChargeback("30d");
    expect(source).toBe("live");
    expect(teams).toEqual([
      {
        name: "ml-inference",
        costCenter: "ml-platform",
        spendMonthK: 82,
        recoverableK: 18.2,
        gpuIdleK: 14.8,
        clouds: { aws: 45, gcp: 0, azure: 37 },
      },
    ]);
    expect(fleetTotalK).toBe(82);
    expect(fleetRecoverableK).toBe(18.2);
  });
});
