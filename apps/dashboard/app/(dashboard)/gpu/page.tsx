// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Cpu } from "lucide-react";
import { Topbar } from "@/components/topbar";

export const metadata = { title: "GPU panel · KubeHero" };

type GpuRow = {
  id: string;
  kind: string;
  node: string;
  cluster: string;
  utilMean: number;
  vramUsedGB: number;
  vramTotalGB: number;
  idleHours: number;
  costMoUSD: number;
};

const GPUS: GpuRow[] = [
  { id: "gpu-01", kind: "A100 80GB", node: "aks-nc24ads-01", cluster: "aks-westeu-prod-01", utilMean: 12,  vramUsedGB: 9,  vramTotalGB: 80, idleHours: 12, costMoUSD: 2952 },
  { id: "gpu-02", kind: "A100 80GB", node: "aks-nc24ads-01", cluster: "aks-westeu-prod-01", utilMean: 18,  vramUsedGB: 14, vramTotalGB: 80, idleHours:  9, costMoUSD: 2952 },
  { id: "gpu-03", kind: "A100 80GB", node: "aks-nc24ads-02", cluster: "aks-westeu-prod-01", utilMean: 72,  vramUsedGB: 62, vramTotalGB: 80, idleHours:  1, costMoUSD: 2952 },
  { id: "gpu-04", kind: "A100 80GB", node: "aks-nc24ads-02", cluster: "aks-westeu-prod-01", utilMean: 84,  vramUsedGB: 68, vramTotalGB: 80, idleHours:  0, costMoUSD: 2952 },
  { id: "gpu-05", kind: "H100 80GB", node: "eks-p5-01",      cluster: "eks-use1-prod",      utilMean: 91,  vramUsedGB: 72, vramTotalGB: 80, idleHours:  0, costMoUSD: 7200 },
  { id: "gpu-06", kind: "H100 80GB", node: "eks-p5-01",      cluster: "eks-use1-prod",      utilMean: 88,  vramUsedGB: 70, vramTotalGB: 80, idleHours:  0, costMoUSD: 7200 },
  { id: "gpu-07", kind: "H100 80GB", node: "eks-p5-02",      cluster: "eks-use1-prod",      utilMean: 22,  vramUsedGB: 14, vramTotalGB: 80, idleHours:  8, costMoUSD: 7200 },
  { id: "gpu-08", kind: "L4 24GB",   node: "gke-g2-batch-01",cluster: "gke-euw4-batch",     utilMean: 38,  vramUsedGB:  9, vramTotalGB: 24, idleHours:  4, costMoUSD:  880 },
];

const totalIdleHrs = GPUS.reduce((a, g) => a + g.idleHours, 0);
const totalMonthly = GPUS.reduce((a, g) => a + g.costMoUSD, 0);
const wastedPct = GPUS.reduce((a, g) => a + (100 - g.utilMean), 0) / GPUS.length;

function tone(pct: number): string {
  if (pct >= 85) return "var(--color-signal)";
  if (pct >= 55) return "var(--color-cool)";
  if (pct >= 25) return "var(--color-warn)";
  return "var(--color-accent)";
}

export default function GpuPage() {
  return (
    <>
      <Topbar crumbs={[{ label: "gpu panel" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Cpu className="h-3 w-3" />
              /// gpu utilization · dcgm + MIG · 24h mean
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              {GPUS.length} GPUs across the fleet.
            </h1>
          </div>
        </div>

        {/* KPIs */}
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3">
          <Kpi
            label="Mean idle share"
            value={`${wastedPct.toFixed(0)}%`}
            sub="inverse of utilization"
            tone="var(--color-accent)"
          />
          <Kpi
            label="Idle GPU-hours · 24h"
            value={`${totalIdleHrs}`}
            sub="sum across fleet"
            tone="var(--color-warn)"
          />
          <Kpi
            label="Spend · month"
            value={`$${(totalMonthly / 1000).toFixed(1)}k`}
            sub="on-demand list price"
            tone="var(--color-fg)"
          />
        </div>

        {/* per-GPU table */}
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// per-gpu
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              dcgm · mig-aware · 1s resolution
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[820px] border-collapse text-[13px]">
              <thead>
                <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  <th className="py-2 pl-4 font-normal">GPU</th>
                  <th className="py-2 font-normal">Kind</th>
                  <th className="py-2 font-normal">Node · cluster</th>
                  <th className="py-2 pr-3 font-normal">Util (mean)</th>
                  <th className="py-2 pr-3 text-right font-normal">VRAM</th>
                  <th className="py-2 pr-3 text-right font-normal">Idle · 24h</th>
                  <th className="py-2 pr-4 text-right font-normal">Cost / mo</th>
                </tr>
              </thead>
              <tbody>
                {GPUS.map((g) => (
                  <tr
                    key={g.id}
                    className="border-b border-[var(--color-line)] last:border-b-0"
                  >
                    <td className="py-3 pl-4 font-mono text-[12.5px] text-[var(--color-fg)]">
                      {g.id}
                    </td>
                    <td className="py-3 pr-3 font-mono text-[12px] text-[var(--color-fg-dim)]">
                      {g.kind}
                    </td>
                    <td className="py-3 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">
                      {g.node}
                      <span className="ml-2 text-[var(--color-fg-faint)]">
                        · {g.cluster}
                      </span>
                    </td>
                    <td className="py-3 pr-3">
                      <div className="flex items-center gap-2">
                        <div className="relative h-[4px] w-24 bg-[var(--color-line)]">
                          <div
                            className="absolute left-0 top-0 h-full"
                            style={{ width: `${g.utilMean}%`, background: tone(g.utilMean) }}
                          />
                        </div>
                        <span
                          className="font-mono text-[11.5px] tabular-nums"
                          style={{ color: tone(g.utilMean) }}
                        >
                          {g.utilMean}%
                        </span>
                      </div>
                    </td>
                    <td className="py-3 pr-3 text-right font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">
                      {g.vramUsedGB}
                      <span className="text-[var(--color-fg-faint)]">
                        /{g.vramTotalGB}GB
                      </span>
                    </td>
                    <td className="py-3 pr-3 text-right font-mono text-[11.5px] tabular-nums" style={{ color: g.idleHours > 4 ? "var(--color-accent)" : "var(--color-fg)" }}>
                      {g.idleHours}h
                    </td>
                    <td className="py-3 pr-4 text-right font-mono text-[12px] tabular-nums text-[var(--color-fg)]">
                      ${g.costMoUSD.toLocaleString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </>
  );
}

function Kpi({
  label,
  value,
  sub,
  tone,
}: {
  label: string;
  value: string;
  sub: string;
  tone: string;
}) {
  return (
    <div className="flex flex-col gap-1.5 bg-[var(--color-bg-raised)] px-5 py-4">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        {label}
      </span>
      <span className="font-mono text-[22px] tabular-nums" style={{ color: tone }}>
        {value}
      </span>
      <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
        {sub}
      </span>
    </div>
  );
}
