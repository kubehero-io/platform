// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { CheckCircle2, ExternalLink, Key } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { IntegrationRow, type Integration } from "@/components/interactive/integration-row";
import { AuditExportRow, type Exporter } from "@/components/interactive/audit-export";
import { getSession } from "@/lib/session";

export const metadata = { title: "Settings · KubeHero" };

const INTEGRATIONS: Integration[] = [
  { id: "slack",      name: "Slack",          kind: "chat",    note: "routes ceiling alerts + budget threshold notices" },
  { id: "pagerduty",  name: "PagerDuty",      kind: "on-call", note: "critical severities page on-call directly" },
  { id: "opsgenie",   name: "OpsGenie",       kind: "on-call", note: "alternative to PagerDuty; same routing semantics" },
  { id: "github",     name: "GitHub",         kind: "gitops",  note: "open PRs with rightsizing patches into your manifests repo" },
  { id: "datadog",    name: "Datadog",        kind: "observ.", note: "forward our metrics into your existing APM context" },
];

const EXPORTERS: Exporter[] = [
  { id: "syslog",  name: "Syslog",  tint: "var(--color-signal)", cli: "kubehero audit forward --syslog",  example: "udp://siem.internal:514" },
  { id: "webhook", name: "Webhook", tint: "var(--color-cool)",   cli: "kubehero audit forward --webhook", example: "https://siem.internal/hooks/kubehero" },
  { id: "s3",      name: "S3",      tint: "var(--color-warn)",   cli: "kubehero audit forward --s3",      example: "s3://acme-compliance/kubehero/" },
];

const API_KEYS = [
  { id: "key-1", label: "ci-github-actions",  prefix: "khc_abc123…", scope: "read + policies",   lastUsed: "2m ago" },
  { id: "key-2", label: "terraform-atlantis", prefix: "khc_def456…", scope: "read-only",         lastUsed: "14h ago" },
  { id: "key-3", label: "grafana-alloc-scrape", prefix: "khc_ghi789…", scope: "read metrics only", lastUsed: "just now" },
];

export default async function SettingsPage() {
  const s = await getSession();
  return (
    <>
      <Topbar crumbs={[{ label: "settings" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            /// settings · {s?.org ?? "demo"}
          </div>
          <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
            Organization, team, and integrations.
          </h1>
        </div>

        <div className="grid gap-8">
          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <Header title="Organization" cli="kubehero org show" />
            <div className="grid gap-[1px] bg-[var(--color-line)] md:grid-cols-2">
              <KV label="Name"          value={s?.org ?? "demo"} />
              <KV label="Edition"       value="Open source · self-hosted" />
              <KV label="Nodes tracked" value="502" tint="var(--color-signal)" />
              <KV label="Retention"     value="90d · 1s resolution" />
              <KV label="Default team"  value={'"unattributed"'} />
              <KV label="Created"       value="2026-04-23" />
            </div>
          </section>

          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <Header title="Team" cli="kubehero team invite --email you@acme.com" />
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {[
                { email: s?.email ?? "demo@kubehero.io", role: "Admin",    badge: "you" as string | null },
                { email: "ops-lead@acme.com",            role: "Operator", badge: null },
                { email: "finance@acme.com",             role: "Viewer",   badge: null },
              ].map((m) => (
                <div key={m.email} className="flex items-center gap-3 bg-[var(--color-bg-raised)] px-4 py-3 font-mono text-[12.5px]">
                  <span className="h-6 w-6 shrink-0 rounded-sm bg-[var(--color-bg-sunken)] text-center leading-6 text-[var(--color-fg-dim)]">
                    {m.email[0]?.toUpperCase()}
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2">
                      <span className="truncate text-[var(--color-fg)]">{m.email}</span>
                      {m.badge && (
                        <span className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1 py-0.5 text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                          {m.badge}
                        </span>
                      )}
                    </span>
                  </span>
                  <span className="shrink-0 text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)]">
                    {m.role}
                  </span>
                </div>
              ))}
              <form action="" className="flex items-center gap-2 bg-[var(--color-bg-raised)] px-4 py-3">
                <input
                  type="email"
                  placeholder="teammate@acme.com"
                  className="flex-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[12px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] focus:border-[var(--color-fg-dim)]"
                />
                <button type="button" className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                  invite
                </button>
              </form>
            </div>
          </section>

          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <Header title="Integrations" cli="kubehero integrations list" />
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {INTEGRATIONS.map((i) => (
                <IntegrationRow key={i.id} integration={i} />
              ))}
            </div>
          </section>

          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <Header title="Audit export" cli="kubehero audit status" />
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {EXPORTERS.map((ex) => (
                <AuditExportRow key={ex.id} ex={ex} />
              ))}
            </div>
            <div className="border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              events are HMAC-signed · see{" "}
              <Link href="https://kubehero.io/docs/security" target="_blank" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                docs → security
              </Link>
            </div>
          </section>

          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <Header title="API keys" cli="kubehero auth tokens list" />
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {API_KEYS.map((k) => (
                <div key={k.id} className="flex items-center gap-3 bg-[var(--color-bg-raised)] px-4 py-3 font-mono text-[12px]">
                  <Key className="h-3 w-3 text-[var(--color-fg-faint)]" />
                  <span className="min-w-0 flex-1">
                    <span className="flex items-baseline justify-between gap-2">
                      <span className="text-[var(--color-fg)]">{k.label}</span>
                      <span className="shrink-0 text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                        {k.scope}
                      </span>
                    </span>
                    <span className="mt-0.5 flex items-baseline justify-between gap-2 text-[10.5px] text-[var(--color-fg-dim)]">
                      <span>{k.prefix}</span>
                      <span>last used {k.lastUsed}</span>
                    </span>
                  </span>
                  <button type="button" className="shrink-0 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] hover:text-[var(--color-accent)]">
                    revoke
                  </button>
                </div>
              ))}
              <div className="bg-[var(--color-bg-raised)] px-4 py-3">
                <button type="button" className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                  + new api key
                </button>
              </div>
            </div>
          </section>

          <section className="flex items-center justify-between border border-dashed border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-4 py-3 font-mono text-[11px]">
            <div className="flex items-center gap-2 text-[var(--color-fg-dim)]">
              <CheckCircle2 className="h-3.5 w-3.5 text-[var(--color-signal)]" />
              <span>
                Mirror everything here as YAML via{" "}
                <code className="text-[var(--color-fg)]">values.yaml</code> in your chart.
              </span>
            </div>
            <Link href="https://kubehero.io/docs/integrations/identity" target="_blank" className="inline-flex items-center gap-1.5 text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              identity + SSO
              <ExternalLink className="h-3 w-3" />
            </Link>
          </section>
        </div>
      </div>
    </>
  );
}

function Header({ title, cli }: { title: string; cli: string }) {
  return (
    <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        /// {title.toLowerCase()}
      </span>
      <code className="font-mono text-[10px] text-[var(--color-fg-faint)]">{cli}</code>
    </div>
  );
}

function KV({ label, value, tint }: { label: string; value: string; tint?: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 bg-[var(--color-bg-raised)] px-4 py-3 font-mono">
      <span className="text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        {label}
      </span>
      <span className="truncate text-[12px]" style={{ color: tint ?? "var(--color-fg)" }}>
        {value}
      </span>
    </div>
  );
}
