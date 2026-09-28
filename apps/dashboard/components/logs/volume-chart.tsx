// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Log volume histogram stacked by level. Drag across it to zoom: the
// brush writes ?from=&to= (absolute ms), which every panel on /logs —
// lines, patterns, metrics — re-queries against.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useTransition } from "react";
import { TimeChart } from "@/components/charts/time-chart";
import { formatCompact } from "@/lib/chart/scale";
import { LEVEL_COLOR, LEVEL_ORDER, normalizeLevel, seriesColor } from "@/lib/chart/palette";

export function VolumeChart({
  times,
  stepMs,
  series,
  startMs,
  endMs,
  groupBy = "level",
}: {
  times: number[];
  stepMs: number;
  series: { key: string; values: number[] }[];
  startMs: number;
  endMs: number;
  groupBy?: string;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, start] = useTransition();

  // Severity order bottom-up (errors at the base where they're easiest to read).
  const ordered =
    groupBy === "level"
      ? [...series].sort((a, b) => LEVEL_ORDER.indexOf(normalizeLevel(a.key)) - LEVEL_ORDER.indexOf(normalizeLevel(b.key)))
      : series;

  const zoom = (from: number, to: number) => {
    const next = new URLSearchParams(sp.toString());
    next.delete("window");
    next.set("from", String(from));
    next.set("to", String(to));
    start(() => router.push(`${pathname}?${next.toString()}`, { scroll: false }));
  };

  return (
    <div className={pending ? "opacity-70 transition-opacity" : ""}>
      <TimeChart
        mode="stacked"
        times={times}
        stepMs={stepMs}
        startMs={startMs}
        endMs={endMs}
        height={132}
        series={ordered.map((s, i) => ({
          key: s.key,
          label: s.key,
          color: groupBy === "level" ? LEVEL_COLOR[normalizeLevel(s.key)] : seriesColor(i, s.key),
          values: s.values,
        }))}
        yFormat={(v) => formatCompact(v)}
        valueFormat={(v) => `${Math.round(v).toLocaleString("en-US")} lines`}
        onBrush={zoom}
        ariaLabel="Log lines per interval by level — drag to zoom"
        emptyLabel="no log lines in this range"
      />
    </div>
  );
}
