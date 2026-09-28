// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /alerts — one alerting engine over every signal: firing / pending /
// resolved alerts, the rules (with an editor + live test for every kind)
// and silences. Mutations go through server actions → AlertsService;
// the control plane enforces admin for writes.

import Link from "next/link";
import { ArrowUpRight, BellOff, BellRing, MessageCircleQuestion, Pencil, Plus, Siren } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { UrlTabs } from "@/components/ui/url-tabs";
import { UrlDrawer } from "@/components/ui/drawer";
import { Chip, severityTone } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { ParamToggle } from "@/components/ui/param-controls";
import { RuleEditor } from "@/components/alerts/rule-editor";
import { SilenceForm } from "@/components/alerts/silence-form";
import { ExpireSilence } from "@/components/alerts/expire-silence";
import { listAlertRules, listAlerts, listSilences } from "@/lib/api/alerts";
import { combineSources } from "@/lib/api/source";
import { matchersForAlert } from "@/lib/alerts/rules";
import type { Alert, AlertRule } from "@/lib/alerts/types";
import { authMode } from "@/lib/auth-mode";
import { canAdmin } from "@/lib/roles";
import { getSessionView } from "@/lib/session";
import { formatAgo, formatCompact } from "@/lib/chart/scale";
import { flatParams, hrefWith, param, type SearchParamsRecord } from "@/lib/url";
import { requestNow } from "@/lib/request-time";

export const metadata = { title: "Alerts · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/alerts";
const TABS = ["firing", "pending", "resolved", "rules", "silences"] as const;
type Tab = (typeof TABS)[number];

export default async function AlertsPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const tab: Tab = (TABS as readonly string[]).includes(param(sp, "tab")) ? (param(sp, "tab") as Tab) : "firing";
  const includeExpired = param(sp, "expired") === "1";
  const ruleParam = param(sp, "rule", 128);
  const silenceParam = param(sp, "silence", 128);

  const [alerts, rules, silences, session] = await Promise.all([
    listAlerts({ limit: 300 }),
    listAlertRules(),
    listSilences({ includeExpired }),
    getSessionView(),
  ]);
  const admin = authMode() === "demo" || canAdmin(session?.role);
  const src = combineSources(alerts, rules, silences);
  const all = alerts.data.alerts;
  const byState = (s: string) => all.filter((a) => a.state === s);
  const firing = byState("firing");
  const pending = byState("pending");
  const resolved = byState("resolved");
  const now = requestNow();

  const editing: AlertRule | undefined = ruleParam && ruleParam !== "new" ? rules.data.find((r) => r.id === ruleParam) : undefined;
  const silenceFor: Alert | undefined = silenceParam && silenceParam !== "new" ? all.find((a) => a.id === silenceParam) : undefined;

  const tabs = [
    { id: "firing", label: "firing", count: firing.length, tone: firing.length > 0 ? "var(--color-accent)" : undefined },
    { id: "pending", label: "pending", count: pending.length, tone: pending.length > 0 ? "var(--color-warn)" : undefined },
    { id: "resolved", label: "resolved · 24h", count: resolved.length },
    { id: "rules", label: "rules", count: rules.data.length },
    { id: "silences", label: "silences", count: silences.data.filter((s) => Date.parse(s.endsAt) > now).length },
  ];

  return (
    <>
      <Topbar crumbs={[{ label: "control" }, { label: "alerts" }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Siren}
          iconTone={firing.length > 0 ? "var(--color-accent)" : undefined}
          eyebrow={<>/// alerts · logs · cost · budget · anomaly · network · events</>}
          title={firing.length > 0 ? `${firing.length} alert${firing.length === 1 ? "" : "s"} firing.` : "Nothing is firing."}
          sub="One rule engine over every signal KubeHero stores, routed to the same channels as policy escalations."
          actions={
            <>
              <Link href={hrefWith(PATH, params, { rule: "new", tab: "rules" })} className="btn-secondary !px-2.5 !py-1 !text-[12px]" scroll={false}>
                <Plus className="h-3.5 w-3.5" aria-hidden /> new rule
              </Link>
              <DataSourceBadge {...src} />
            </>
          }
        />

        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <UrlTabs pathname={PATH} params={params} tabs={tabs} active={tab} label="alert views" resetKeys={["rule", "silence"]} />

          {(tab === "firing" || tab === "pending" || tab === "resolved") && (
            <AlertTable alerts={tab === "firing" ? firing : tab === "pending" ? pending : resolved} state={tab} params={params} now={now} />
          )}

          {tab === "rules" && <RulesTable rules={rules.data} params={params} alerts={all} />}

          {tab === "silences" && (
            <div>
              <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-line)] px-4 py-2">
                <ParamToggle param="expired" label="show expired" checked={includeExpired} />
                <Link href={hrefWith(PATH, params, { silence: "new" })} scroll={false} className="inline-flex items-center gap-1 font-mono text-[10.5px] uppercase tracking-[0.12em] text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                  <BellOff className="h-3 w-3" aria-hidden /> new silence
                </Link>
              </div>
              {silences.data.length === 0 ? (
                <EmptyState compact icon={BellOff} title="No silences" body="Silence an alert from its row to mute notifications while you work on it." />
              ) : (
                <table className="w-full border-collapse text-[12px]">
                  <thead>
                    <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                      <th className="py-2 pl-4 font-normal">matchers</th>
                      <th className="py-2 font-normal">ends</th>
                      <th className="py-2 font-normal">by</th>
                      <th className="py-2 font-normal">reason</th>
                      <th className="py-2 pr-4 text-right font-normal" />
                    </tr>
                  </thead>
                  <tbody>
                    {silences.data.map((s) => {
                      const ends = Date.parse(s.endsAt);
                      const active = ends > now;
                      return (
                        <tr key={s.id} className={`border-b border-[var(--color-line)] last:border-b-0 ${active ? "" : "opacity-55"}`}>
                          <td className="py-2 pl-4 pr-3">
                            <span className="flex flex-wrap gap-1">
                              {Object.entries(s.matchers).map(([k, v]) => (
                                <Chip key={k} dot={false}>
                                  <span className="normal-case tracking-normal">
                                    <span className="text-[var(--color-syn-label)]">{k}</span>={v}
                                  </span>
                                </Chip>
                              ))}
                            </span>
                          </td>
                          <td className="py-2 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">{active ? `in ${formatAgo(ends - now)}` : `expired ${formatAgo(now - ends)} ago`}</td>
                          <td className="py-2 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">{s.createdBy || "—"}</td>
                          <td className="max-w-[360px] py-2 pr-3 text-[12px] text-[var(--color-fg-dim)]">{s.comment || "—"}</td>
                          <td className="py-2 pr-4 text-right">{active && <ExpireSilence id={s.id} disabled={!admin} />}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          )}
        </div>
      </div>

      {ruleParam && (
        <UrlDrawer param="rule" width={640} title={editing ? `edit · ${editing.name}` : "new alert rule"} subtitle={editing ? `${editing.kind} · created by ${editing.createdBy || "—"}` : "evaluated on the control plane"}>
          <RuleEditor key={ruleParam} rule={editing} canAdmin={admin} />
        </UrlDrawer>
      )}
      {silenceParam && (
        <UrlDrawer param="silence" width={560} title={silenceFor ? `silence · ${silenceFor.ruleName}` : "new silence"} subtitle="matchers must all equal the alert's labels (alertname = rule name)">
          <SilenceForm key={silenceParam} initial={silenceFor ? matchersForAlert(silenceFor) : { alertname: "" }} canAdmin={admin} />
        </UrlDrawer>
      )}
    </>
  );
}

function AlertTable({ alerts, state, params, now }: { alerts: Alert[]; state: string; params: Record<string, string>; now: number }) {
  if (alerts.length === 0) {
    return (
      <EmptyState
        compact
        icon={BellRing}
        title={state === "firing" ? "Nothing firing — all rules are quiet" : state === "pending" ? "Nothing pending" : "No alerts resolved in the last 24h"}
        body={state === "firing" ? "Rules keep evaluating every interval; pending alerts appear here once their condition holds for the rule's pending period." : undefined}
        cta={{ href: hrefWith(PATH, params, { tab: "rules" }), label: "review rules" }}
      />
    );
  }
  const sorted = [...alerts].sort((a, b) => sevRank(b.severity) - sevRank(a.severity) || Date.parse(b.startedAt) - Date.parse(a.startedAt));
  return (
    <div className="max-h-[70vh] overflow-auto">
      <table className="w-full min-w-[980px] border-collapse text-[12.5px]" data-nav-list>
        <thead>
          <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pl-4 font-normal">alert</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 font-normal">labels</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 text-right font-normal">value</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 font-normal">{state === "resolved" ? "resolved" : "since"}</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-4 text-right font-normal" />
          </tr>
        </thead>
        <tbody>
          {sorted.map((a) => {
            const since = state === "resolved" ? a.resolvedAt : state === "pending" ? a.startedAt : a.firedAt || a.startedAt;
            const sinceMs = Date.parse(since);
            const ask = `Why is "${a.ruleName}" ${state} for ${Object.entries(a.labels)
              .filter(([k]) => ["namespace", "workload", "subject", "policy"].includes(k))
              .map(([, v]) => v)
              .join("/") || "the fleet"}?`;
            return (
              <tr key={a.id} className="group border-b border-[var(--color-line)] align-top last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50">
                <td className="max-w-[380px] py-2.5 pl-4 pr-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Chip tone={severityTone(a.severity)}>{a.severity}</Chip>
                    <span className="font-mono text-[12.5px] text-[var(--color-fg)]" data-nav-item tabIndex={-1}>
                      {a.ruleName}
                    </span>
                    {a.silenced && (
                      <Chip tone="var(--color-fg-faint)" title="notifications suppressed by a silence">
                        silenced
                      </Chip>
                    )}
                  </div>
                  {a.summary && <div className="mt-1 text-[12px] leading-relaxed text-[var(--color-fg-dim)]">{a.summary}</div>}
                </td>
                <td className="max-w-[340px] py-2.5 pr-3">
                  <span className="flex flex-wrap gap-1">
                    {Object.entries(a.labels).map(([k, v]) => (
                      <span key={k} className="border border-[var(--color-line)] bg-[var(--color-bg)] px-1 py-px font-mono text-[10.5px] text-[var(--color-fg-dim)]">
                        <span className="text-[var(--color-syn-label)]">{k}</span>={v}
                      </span>
                    ))}
                  </span>
                </td>
                <td className="py-2.5 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">{formatCompact(a.value, 2)}</td>
                <td className="whitespace-nowrap py-2.5 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">
                  {Number.isFinite(sinceMs) ? `${formatAgo(now - sinceMs)} ago` : "—"}
                </td>
                <td className="whitespace-nowrap py-2.5 pr-4 text-right">
                  <span className="inline-flex items-center gap-3 font-mono text-[10.5px] uppercase tracking-[0.12em]">
                    {a.linkPath && (
                      <Link href={a.linkPath} className="inline-flex items-center gap-1 text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                        open <ArrowUpRight className="h-3 w-3" aria-hidden />
                      </Link>
                    )}
                    <Link href={`/ask?q=${encodeURIComponent(ask)}`} className="inline-flex items-center gap-1 text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]" title="Ask KubeHero to investigate">
                      <MessageCircleQuestion className="h-3 w-3" aria-hidden /> ask
                    </Link>
                    {state !== "resolved" && !a.silenced && (
                      <Link href={hrefWith(PATH, params, { silence: a.id })} scroll={false} className="inline-flex items-center gap-1 text-[var(--color-fg-dim)] hover:text-[var(--color-warn)]">
                        <BellOff className="h-3 w-3" aria-hidden /> silence
                      </Link>
                    )}
                  </span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function RulesTable({ rules, params, alerts }: { rules: AlertRule[]; params: Record<string, string>; alerts: Alert[] }) {
  if (rules.length === 0) {
    return <EmptyState compact icon={Siren} title="No alert rules yet" cta={{ href: hrefWith(PATH, params, { rule: "new" }), label: "create the first rule" }} />;
  }
  const firingBy = new Map<string, number>();
  for (const a of alerts) if (a.state === "firing") firingBy.set(a.ruleId, (firingBy.get(a.ruleId) ?? 0) + 1);
  return (
    <div className="max-h-[70vh] overflow-auto">
      <table className="w-full min-w-[980px] border-collapse text-[12.5px]" data-nav-list>
        <thead>
          <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pl-4 font-normal">rule</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 font-normal">kind</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 font-normal">condition</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 font-normal">channels</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-4 text-right font-normal">state</th>
          </tr>
        </thead>
        <tbody>
          {rules.map((r) => (
            <tr key={r.id} className={`border-b border-[var(--color-line)] align-top last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50 ${r.enabled ? "" : "opacity-60"}`}>
              <td className="max-w-[420px] py-2.5 pl-4 pr-3">
                <Link href={hrefWith(PATH, params, { rule: r.id })} scroll={false} data-nav-item className="inline-flex items-center gap-2 font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]">
                  <Chip tone={severityTone(r.severity)}>{r.severity}</Chip>
                  {r.name}
                  <Pencil className="h-3 w-3 text-[var(--color-fg-faint)]" aria-hidden />
                </Link>
                <div className="mt-1 break-all font-mono text-[11px] leading-relaxed text-[var(--color-fg-dim)]">{r.query}</div>
              </td>
              <td className="py-2.5 pr-3">
                <Chip dot={false}>{r.kind}</Chip>
              </td>
              <td className="whitespace-nowrap py-2.5 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">
                {r.op} {r.threshold} · for {r.pendingFor} · every {r.evalInterval}
              </td>
              <td className="max-w-[220px] py-2.5 pr-3 font-mono text-[11px] text-[var(--color-fg-dim)]">
                {r.channels.length === 0 ? <span className="text-[var(--color-fg-faint)]">ui only</span> : r.channels.map((c) => <div key={c} className="truncate">{c}</div>)}
              </td>
              <td className="py-2.5 pr-4 text-right">
                {!r.enabled ? (
                  <Chip tone="var(--color-fg-faint)">disabled</Chip>
                ) : firingBy.get(r.id) ? (
                  <Chip tone="var(--color-accent)">firing × {firingBy.get(r.id)}</Chip>
                ) : (
                  <Chip tone="var(--color-signal)">ok</Chip>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function sevRank(s: string): number {
  return s === "critical" ? 3 : s === "warn" ? 2 : 1;
}
