// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Connect-RPC client for the Advisor service (services/advisor).
//
// Behavior mirrors lib/api/client.ts:
//   · ADVISOR_URL set  → real Connect-JSON call to the Go advisor
//   · unset / error    → returns null, callers fall back to the demo fixture

import "server-only";
import type { GetBriefingResponse } from "./types";

const SERVICE = "kubehero.v1.AdvisorService";

function endpoint(): string | null {
  const v = process.env.ADVISOR_URL?.trim();
  return v && v.length > 0 ? v.replace(/\/$/, "") : null;
}

async function rpc<Req, Res>(
  method: string,
  req: Req,
  signal?: AbortSignal,
): Promise<Res | null> {
  const base = endpoint();
  if (!base) return null;
  try {
    const r = await fetch(`${base}/${SERVICE}/${method}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Connect-Protocol-Version": "1",
      },
      body: JSON.stringify(req ?? {}),
      cache: "no-store",
      signal,
    });
    if (!r.ok) {
      console.error("[advisor]", method, r.status, await r.text());
      return null;
    }
    return (await r.json()) as Res;
  } catch (err) {
    console.error("[advisor] rpc failed", method, err);
    return null;
  }
}

export async function getBriefing(
  clusterId?: string,
): Promise<GetBriefingResponse | null> {
  const req: { clusterId?: string; window: string } = { window: "24h" };
  if (clusterId) req.clusterId = clusterId;
  const res = await rpc<typeof req, GetBriefingResponse>("GetBriefing", req);
  // Guard against a live server sending an empty envelope.
  if (!res || !res.briefing) return null;
  return res;
}

export function isAdvisorLive(): boolean {
  return endpoint() !== null;
}
