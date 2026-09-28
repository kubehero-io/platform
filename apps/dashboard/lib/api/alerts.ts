// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// AlertsService. Reads degrade to labelled demo data; writes never
// pretend: without a control plane they report "demo — not persisted",
// and control-plane errors (permission_denied for non-admins,
// invalid_argument for a bad rule) come back as typed errors the UI
// shows next to the form.

import "server-only";
import { demoAlerts, demoRules, demoSilences, demoTestRule } from "@/lib/demo/alerts";
import { toAlert, toRule, toSilence, toTestResult } from "@/lib/alerts/rules";
import type { Alert, AlertJson, AlertRule, AlertRuleJson, Silence, SilenceJson, TestAlertRuleResponseJson, TestResult } from "@/lib/alerts/types";
import { callUnary, type RpcError, type RpcResult } from "./rpc";
import { demo, live, type Sourced } from "./source";

const SERVICE = "kubehero.v1.AlertsService";

function degrade<T>(err: RpcError, fixture: () => T): Sourced<T> {
  return err.code === "not_configured" ? demo(fixture(), "unset") : demo(fixture(), "error", `${err.code}: ${err.message}`.slice(0, 160));
}

export type AlertsView = { alerts: Alert[]; firing: number; pending: number };

export async function listAlerts(q: { state?: string; limit?: number } = {}): Promise<Sourced<AlertsView>> {
  const fixture = (): AlertsView => {
    const all = demoAlerts();
    const alerts = q.state ? all.filter((a) => a.state === q.state) : all;
    return { alerts, firing: all.filter((a) => a.state === "firing").length, pending: all.filter((a) => a.state === "pending").length };
  };
  const res = await callUnary<{ alerts?: AlertJson[]; firing?: number; pending?: number }>("cp", SERVICE, "ListAlerts", {
    state: q.state ?? "",
    limit: q.limit ?? 200,
  });
  if (!res.ok) return degrade(res.error, fixture);
  const alerts = (res.data.alerts ?? []).map(toAlert);
  return live({ alerts, firing: res.data.firing ?? alerts.filter((a) => a.state === "firing").length, pending: res.data.pending ?? alerts.filter((a) => a.state === "pending").length });
}

export async function listAlertRules(q: { kind?: string } = {}): Promise<Sourced<AlertRule[]>> {
  const fixture = () => demoRules().filter((r) => !q.kind || r.kind === q.kind);
  const res = await callUnary<{ rules?: AlertRuleJson[] }>("cp", SERVICE, "ListAlertRules", { kind: q.kind ?? "" });
  if (!res.ok) return degrade(res.error, fixture);
  return live((res.data.rules ?? []).map(toRule));
}

export async function listSilences(q: { includeExpired?: boolean } = {}): Promise<Sourced<Silence[]>> {
  const fixture = () => demoSilences(Date.now(), q.includeExpired);
  const res = await callUnary<{ silences?: SilenceJson[] }>("cp", SERVICE, "ListSilences", { includeExpired: !!q.includeExpired });
  if (!res.ok) return degrade(res.error, fixture);
  return live((res.data.silences ?? []).map(toSilence));
}

// ─── writes (called from server actions only) ──────────────────────────

export function upsertAlertRule(rule: AlertRuleJson): Promise<RpcResult<{ rule?: AlertRuleJson }>> {
  return callUnary("cp", SERVICE, "UpsertAlertRule", { rule });
}

export function deleteAlertRule(id: string): Promise<RpcResult<unknown>> {
  return callUnary("cp", SERVICE, "DeleteAlertRule", { id });
}

export async function testAlertRule(rule: AlertRuleJson): Promise<Sourced<TestResult>> {
  const fixture = () =>
    demoTestRule({ kind: (rule.kind ?? "logs") as never, query: rule.query ?? "", op: rule.op ?? ">", threshold: Number(rule.threshold ?? 0) });
  const res = await callUnary<TestAlertRuleResponseJson>("cp", SERVICE, "TestAlertRule", { rule }, { timeoutMs: 20_000 });
  if (!res.ok) {
    // A rule the control plane rejects is an answer, not an outage.
    if (res.error.code === "invalid_argument") return live({ results: [], error: res.error.message, execMs: 0 });
    return degrade(res.error, fixture);
  }
  return live(toTestResult(res.data));
}

export function createSilence(s: SilenceJson): Promise<RpcResult<{ silence?: SilenceJson }>> {
  return callUnary("cp", SERVICE, "CreateSilence", { silence: s });
}

export function deleteSilence(id: string): Promise<RpcResult<unknown>> {
  return callUnary("cp", SERVICE, "DeleteSilence", { id });
}
