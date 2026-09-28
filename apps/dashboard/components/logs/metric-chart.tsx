// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Series from a LogQL metric query (count_over_time, rate, sum by …).
// Lines on one axis; more than eight series fold into "other" upstream of
// here, so every colour is a validated palette slot.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useTransition } from "react";
import { TimeChart } from "@/components/charts/time-chart";
import { formatCompact } from "@/lib/chart/scale";
import { seriesColor } from "@/lib/chart/palette";
import type { MetricSeries } from "@/lib/logs/types";

export function MetricChart({
  times,
  series,
  startMs,
  endMs,
  unit,
}: {
  times: number[];
  series: MetricSeries[];
  startMs: number;
  endMs: number;
  unit: string;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [, start] = useTransition();
  const zoom = (from: number, to: number) => {
    const next = new URLSearchParams(sp.toString());
    next.delete("window");
    next.set("from", String(from));
    next.set("to", String(to));
    start(() => router.push(`${pathname}?${next.toString()}`, { scroll: false }));
  };
  return (
    <TimeChart
      mode="lines"
      times={times}
      startMs={startMs}
      endMs={endMs}
      height={260}
      series={series.map((s, i) => ({ key: s.key, label: s.label, color: seriesColor(i, s.key), values: s.values }))}
      yFormat={(v) => formatCompact(v)}
      valueFormat={(v) => `${formatCompact(v, 2)}${unit}`}
      onBrush={zoom}
      ariaLabel="Metric query result"
      emptyLabel="the query returned no series"
    />
  );
}
