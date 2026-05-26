// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ExternalLink, Receipt } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { SpendSankey } from "@/components/spend-sankey";
import { getChargeback } from "@/lib/api/chargeback";

export const metadata = { title: "Chargeback · KubeHero" };
export const dynamic = "force-dynamic";

const windowMultiplier: Record<string, number> = {
  "24h": 1 / 30,
  "7d": 7 / 30,
  "30d": 1,
};

type SearchParams = Promise<{ window?: string }>;

export default async function ChargebackPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const { window: w = "30d" } = await searchParams;
  const win = w in windowMultiplier ? w : "30d";
  const mult = windowMultiplier[win];
  const { teams: rawTeams, fleetTotalK: ft, fleetRecoverableK: fr, source } =
    await getChargeback(win);
  // Server doesn't model time-series yet — scale linearly so the page
  // honours the selector. Replace with a real ClickHouse window function
  // call once pod_cost_1s rows exist.
  const teams = rawTeams.map((t) => ({
    ...t,
    spendMonthK: t.spendMonthK * mult,
    recoverableK: t.recoverableK * mult,
    gpuIdleK: t.gpuIdleK * mult,
    clouds: {
      aws: t.clouds.aws * mult,
      gcp: t.clouds.gcp * mult,
      azure: t.clouds.azure * mult,
    },
  }));
  const fleetTotalK = ft * mult;
  const fleetRecoverableK = fr * mult;
  return (
    <>
      <Topbar crumbs={[{ label: "chargeback" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Receipt className="h-3 w-3" />
              /// chargeback · team → workload → cloud · {win}
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Follow the money.
            </h1>
            <p className="mt-2 max-w-2xl text-[14px] text-[var(--color-fg-dim)]">
              Every pod-second attributed to a team, cost-center, and cloud
              via your existing Kubernetes labels. For the PromQL
              conventions, see{" "}
              <Link href="/docs/chargeback" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                docs → chargeback
              </Link>
              .
            </p>
          </div>
          <div className="flex items-baseline gap-4 font-mono">
            <div>
              <div className="text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                fleet / {win}
              </div>
              <div className="text-[22px] tabular-nums text-[var(--color-fg)]">
                ${(fleetTotalK * 1000).toLocaleString()}
              </div>
            </div>
            <div>
              <div className="text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                recoverable
              </div>
              <div className="text-[22px] tabular-nums text-[var(--color-signal)]">
                ${(fleetRecoverableK * 1000).toLocaleString()}
              </div>
            </div>
            <DataSourceBadge source={source} />
          </div>
        </div>

        {/* Sankey */}
        <SpendSankey />

        {/* Team rollup table */}
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// team rollup · monthly
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              sort · spend · desc
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] border-collapse text-[13px]">
              <thead>
                <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  <th className="py-2 pl-4 font-normal">Team</th>
                  <th className="py-2 font-normal">Cost center</th>
                  <th className="py-2 pr-3 text-right font-normal">AWS</th>
                  <th className="py-2 pr-3 text-right font-normal">GCP</th>
                  <th className="py-2 pr-3 text-right font-normal">Azure</th>
                  <th className="py-2 pr-3 text-right font-normal">Spend</th>
                  <th className="py-2 pr-3 text-right font-normal">Recoverable</th>
                  <th className="py-2 pr-4 text-right font-normal">GPU idle</th>
                </tr>
              </thead>
              <tbody>
                {teams.map((t) => (
                  <tr key={t.name} className="border-b border-[var(--color-line)] last:border-b-0">
                    <td className="py-3 pl-4 font-mono text-[12.5px] text-[var(--color-fg)]">
                      {t.name}
                    </td>
                    <td className="py-3 pr-3 font-mono text-[12px] text-[var(--color-fg-dim)]">
                      {t.costCenter}
                    </td>
                    <Money k={t.clouds.aws}   tint="var(--color-warn)"   />
                    <Money k={t.clouds.gcp}   tint="var(--color-signal)" />
                    <Money k={t.clouds.azure} tint="var(--color-cool)"   />
                    <td className="py-3 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">
                      ${t.spendMonthK.toFixed(1)}k
                    </td>
                    <td className="py-3 pr-3 text-right font-mono tabular-nums text-[var(--color-signal)]">
                      ${t.recoverableK.toFixed(1)}k
                    </td>
                    <td className="py-3 pr-4 text-right font-mono tabular-nums" style={{ color: t.gpuIdleK > 0 ? "var(--color-accent)" : "var(--color-fg-faint)" }}>
                      {t.gpuIdleK > 0 ? `$${t.gpuIdleK.toFixed(1)}k` : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>kubehero.io/team label → chargeback axis</span>
            <Link href="/docs/integrations/prometheus-grafana" className="inline-flex items-center gap-1.5 text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              PromQL rules <ExternalLink className="h-3 w-3" />
            </Link>
          </div>
        </div>
      </div>
    </>
  );
}

function Money({ k, tint }: { k: number; tint: string }) {
  return (
    <td className="py-3 pr-3 text-right font-mono tabular-nums" style={{ color: k > 0 ? tint : "var(--color-fg-faint)" }}>
      {k > 0 ? `$${k.toFixed(1)}k` : "—"}
    </td>
  );
}
