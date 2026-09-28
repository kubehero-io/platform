// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { TimeChart } from "@/components/charts/time-chart";
import { formatUsd } from "@/lib/chart/scale";
import { seriesColor } from "@/lib/chart/palette";

export type CostChartSeries = { key: string; label: string; values: number[] };

/** Stacked spend per step. Colours follow the series order the server returned. */
export function CostChart({
  times,
  stepMs,
  series,
  startMs,
  endMs,
  height = 200,
  ariaLabel,
}: {
  times: number[];
  stepMs: number;
  series: CostChartSeries[];
  startMs: number;
  endMs: number;
  height?: number;
  ariaLabel: string;
}) {
  return (
    <TimeChart
      mode="stacked"
      times={times}
      stepMs={stepMs}
      startMs={Math.min(startMs, times[0] ?? startMs)}
      endMs={Math.max(endMs, (times[times.length - 1] ?? endMs) + stepMs)}
      series={series.map((s, i) => ({ ...s, color: seriesColor(i, s.key) }))}
      height={height}
      yFormat={(v) => formatUsd(v)}
      valueFormat={(v) => formatUsd(v, { cents: true })}
      ariaLabel={ariaLabel}
      emptyLabel="no spend recorded in this window"
    />
  );
}
