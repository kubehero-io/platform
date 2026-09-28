// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use server";

// Alert rule + silence mutations. Every action re-checks the session
// (server actions are forgeable POSTs), validates the full payload with
// the same rules the editor shows, requires admin for writes, and then
// lets the control plane enforce everything again. Without a control
// plane nothing is persisted and the result says so.

import { refresh } from "next/cache";
import { currentSession, requireAdmin, type ActionResult } from "@/lib/api/guard";
import { createSilence, deleteAlertRule, deleteSilence, testAlertRule, upsertAlertRule } from "@/lib/api/alerts";
import { describeRpcError, upstreamBase, type RpcError } from "@/lib/api/rpc";
import { ruleFromDraft, validateDraft } from "@/lib/alerts/rules";
import { validateRuleQuery } from "@/lib/alerts/selector";
import type { RuleDraft, TestResult } from "@/lib/alerts/types";

const ID_RE = /^[A-Za-z0-9._:-]{1,128}$/;
const LABEL_KEY_RE = /^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/;

function upstreamError(e: RpcError) {
  return {
    ok: false as const,
    error: {
      code: e.code === "permission_denied" ? ("permission_denied" as const) : e.code === "invalid_argument" ? ("invalid_argument" as const) : ("upstream" as const),
      message: describeRpcError(e),
    },
  };
}

function checkDraft(d: RuleDraft): string | null {
  if (!d || typeof d !== "object") return "missing rule";
  const errors = validateDraft(d);
  const q = validateRuleQuery(d.kind, d.query);
  const first = Object.entries(errors)[0];
  if (first) return `${first[0]}: ${first[1]}`;
  if (q) return `query: ${q}`;
  if (d.id && !ID_RE.test(d.id)) return "bad rule id";
  return null;
}

export async function saveRule(draft: RuleDraft): Promise<ActionResult<{ id?: string }>> {
  const gate = await requireAdmin();
  if (!gate.ok) return gate;
  const bad = checkDraft(draft);
  if (bad) return { ok: false, error: { code: "invalid_argument", message: bad } };
  if (!upstreamBase("cp")) return { ok: true, demo: true, note: "demo mode · the rule was validated but not persisted" };
  const res = await upsertAlertRule(ruleFromDraft(draft));
  if (!res.ok) return upstreamError(res.error);
  refresh();
  return { ok: true, data: { id: res.data.rule?.id } };
}

export async function removeRule(id: string): Promise<ActionResult> {
  const gate = await requireAdmin();
  if (!gate.ok) return gate;
  if (typeof id !== "string" || !ID_RE.test(id)) return { ok: false, error: { code: "invalid_argument", message: "bad rule id" } };
  if (!upstreamBase("cp")) return { ok: true, demo: true, note: "demo mode · nothing was deleted" };
  const res = await deleteAlertRule(id);
  if (!res.ok) return upstreamError(res.error);
  refresh();
  return { ok: true };
}

export async function testRule(draft: RuleDraft): Promise<ActionResult<TestResult & { demo: boolean }>> {
  if (!(await currentSession())) return { ok: false, error: { code: "unauthenticated", message: "Sign in again." } };
  const bad = checkDraft(draft);
  if (bad) return { ok: false, error: { code: "invalid_argument", message: bad } };
  const res = await testAlertRule(ruleFromDraft(draft));
  return { ok: true, data: { ...res.data, demo: res.source === "demo" } };
}

export async function addSilence(input: {
  matchers: Record<string, string>;
  durationMinutes: number;
  comment: string;
}): Promise<ActionResult<{ id?: string }>> {
  const gate = await requireAdmin();
  if (!gate.ok) return gate;
  const entries = Object.entries(input?.matchers ?? {}).filter(([k, v]) => typeof k === "string" && typeof v === "string");
  if (entries.length === 0) return { ok: false, error: { code: "invalid_argument", message: "add at least one matcher" } };
  if (entries.length > 16) return { ok: false, error: { code: "invalid_argument", message: "at most 16 matchers" } };
  for (const [k, v] of entries) {
    if (!LABEL_KEY_RE.test(k)) return { ok: false, error: { code: "invalid_argument", message: `invalid label "${k}"` } };
    if (!v || v.length > 256) return { ok: false, error: { code: "invalid_argument", message: `value for ${k} must be 1–256 characters` } };
  }
  const minutes = Math.floor(Number(input.durationMinutes));
  if (!Number.isFinite(minutes) || minutes < 5 || minutes > 30 * 24 * 60) {
    return { ok: false, error: { code: "invalid_argument", message: "duration must be between 5 minutes and 30 days" } };
  }
  const comment = String(input.comment ?? "").trim().slice(0, 500);
  if (!comment) return { ok: false, error: { code: "invalid_argument", message: "say why — the comment lands in the audit trail" } };
  if (!upstreamBase("cp")) return { ok: true, demo: true, note: "demo mode · the silence was validated but not persisted" };
  const now = Date.now();
  const res = await createSilence({
    matchers: Object.fromEntries(entries),
    startsAt: new Date(now).toISOString(),
    endsAt: new Date(now + minutes * 60_000).toISOString(),
    comment,
    createdBy: gate.session.email || gate.session.subject || "",
  });
  if (!res.ok) return upstreamError(res.error);
  refresh();
  return { ok: true, data: { id: res.data.silence?.id } };
}

export async function expireSilence(id: string): Promise<ActionResult> {
  const gate = await requireAdmin();
  if (!gate.ok) return gate;
  if (typeof id !== "string" || !ID_RE.test(id)) return { ok: false, error: { code: "invalid_argument", message: "bad silence id" } };
  if (!upstreamBase("cp")) return { ok: true, demo: true, note: "demo mode · nothing was changed" };
  const res = await deleteSilence(id);
  if (!res.ok) return upstreamError(res.error);
  refresh();
  return { ok: true };
}
