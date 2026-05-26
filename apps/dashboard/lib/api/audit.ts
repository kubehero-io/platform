// SPDX-License-Identifier: BUSL-1.1
import "server-only";

import { isLive, listAuditLog } from "./client";
import type { AuditEntryDTO } from "./types";

export type AuditEntry = {
  id: string;
  at: string;
  policy: string;
  action: string;
  cluster: string;
  outcome: "applied" | "armed" | "cooldown" | "reverted";
  effectK?: number; // $k/mo realised saving
};

const DEMO: AuditEntry[] = [
  { id: "aud-54021", at: "2026-04-23 09:17:48", policy: "prod-burn-rate-2x",    action: "hpa.cap · 50%",            cluster: "eks-use1-prod",     outcome: "applied",  effectK: 8.6  },
  { id: "aud-54020", at: "2026-04-23 09:14:02", policy: "prod-burn-rate-2x",    action: "trigger evaluated · true", cluster: "eks-use1-prod",     outcome: "armed"   },
  { id: "aud-53980", at: "2026-04-22 18:44:11", policy: "gpu-inference-cap",    action: "pod.evict · 4 pods",       cluster: "aks-westeu-prod-01",outcome: "applied",  effectK: 12.2 },
  { id: "aud-53979", at: "2026-04-22 18:41:03", policy: "gpu-inference-cap",    action: "trigger evaluated · true", cluster: "aks-westeu-prod-01",outcome: "armed"   },
  { id: "aud-53711", at: "2026-04-22 02:01:18", policy: "prod-monthly-ceiling", action: "threshold 80% crossed",    cluster: "all prod",          outcome: "cooldown"},
  { id: "aud-53544", at: "2026-04-21 14:20:07", policy: "staging-monthly",      action: "recommend · cpu.cap",      cluster: "aks-ne-staging",    outcome: "reverted"},
];

function dtoToDisplay(e: AuditEntryDTO): AuditEntry {
  // Server returns RFC3339; the table renders "YYYY-MM-DD HH:MM:SS".
  // Trim Z + replace T with a space for readability.
  const at = e.at.replace("T", " ").replace("Z", "");
  const o = ["applied", "armed", "cooldown", "reverted"].includes(e.outcome)
    ? (e.outcome as AuditEntry["outcome"])
    : "armed";
  const effectK = e.effectUsdMonth > 0 ? e.effectUsdMonth / 1000 : undefined;
  return {
    id: e.id,
    at,
    policy: e.policy,
    action: e.action,
    cluster: e.cluster,
    outcome: o,
    effectK,
  };
}

export type AuditLogState = {
  entries: AuditEntry[];
  source: "live" | "demo";
};

export async function getAuditLog(opts: {
  outcome?: string;
  limit?: number;
} = {}): Promise<AuditLogState> {
  if (!isLive()) {
    return { entries: DEMO, source: "demo" };
  }
  const res = await listAuditLog(opts);
  if (!res || res.entries.length === 0) {
    return { entries: DEMO, source: "demo" };
  }
  return { entries: res.entries.map(dtoToDisplay), source: "live" };
}
