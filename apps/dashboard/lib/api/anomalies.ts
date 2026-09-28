// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { DEMO_ANOMALIES as DEMO } from "@/lib/demo/anomalies";
import { isLive, listAnomalies } from "./client";
import type { AnomalyDTO } from "./types";

export type Anomaly = AnomalyDTO;


export type AnomaliesState = {
  anomalies: Anomaly[];
  source: "live" | "demo";
};

export async function getAnomalies(opts: { window?: string; limit?: number } = {}): Promise<AnomaliesState> {
  if (!isLive()) {
    return { anomalies: DEMO, source: "demo" };
  }
  const res = await listAnomalies(opts);
  if (!res || res.anomalies.length === 0) {
    return { anomalies: DEMO, source: "demo" };
  }
  return { anomalies: res.anomalies, source: "live" };
}
