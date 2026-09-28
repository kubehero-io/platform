// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use server";

// Arming a kill-switch is the most consequential click in the product,
// so it goes through the real ControlPlaneService.ArmPolicy RPC (admin
// role, HMAC-signed audit row, channel page) whenever a control plane is
// connected. Demo mode has nothing to persist to and says so.

import { refresh } from "next/cache";
import { armPolicy } from "@/lib/api/client";
import { requireAdmin, type ActionResult } from "@/lib/api/guard";
import { describeRpcError, upstreamBase } from "@/lib/api/rpc";

const NAME_RE = /^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/; // DNS-1123 subdomain

export async function setPolicyArmed(input: {
  policyName: string;
  armed: boolean;
  reason?: string;
}): Promise<ActionResult<{ auditId?: string }>> {
  const gate = await requireAdmin();
  if (!gate.ok) return gate;

  const policyName = String(input.policyName ?? "");
  if (!NAME_RE.test(policyName)) {
    return { ok: false, error: { code: "invalid_argument", message: "Not a valid policy name." } };
  }
  const reason = String(input.reason ?? "").slice(0, 500);

  if (!upstreamBase("cp")) {
    return { ok: true, demo: true, note: "demo mode · nothing was persisted" };
  }
  const res = await armPolicy({ policyName, armed: input.armed === true, reason });
  if (!res.ok) {
    return {
      ok: false,
      error: {
        code: res.error.code === "permission_denied" ? "permission_denied" : "upstream",
        message: describeRpcError(res.error),
      },
    };
  }
  refresh();
  return { ok: true, data: { auditId: res.data.auditId } };
}
