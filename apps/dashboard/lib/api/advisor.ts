// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Connect-RPC client for the Advisor service (services/advisor).
//
// Behavior mirrors lib/api/client.ts:
//   · ADVISOR_URL set  → real Connect-JSON call to the Go advisor
//   · unset / error    → returns null, callers fall back to the demo fixture
// Credentials: the signed-in user's token in token mode, else
// ADVISOR_TOKEN (see rpc.ts).

import "server-only";
import type { GetBriefingResponse } from "./types";
import { callOrNull, upstreamBase } from "./rpc";

const SERVICE = "kubehero.v1.AdvisorService";

function rpc<Req, Res>(method: string, req: Req): Promise<Res | null> {
  // Briefings can take a while when the LLM brain is cold.
  return callOrNull<Res>("advisor", SERVICE, method, req, { timeoutMs: 30_000 });
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
  return upstreamBase("advisor") !== null;
}
