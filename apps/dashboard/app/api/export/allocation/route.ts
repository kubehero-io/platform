// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// CSV of exactly what /allocation shows (same params). Runs the same
// GetAllocation call server-side with the caller's credentials; a demo
// fallback is labelled in the filename and a `source` column.

import { type NextRequest } from "next/server";
import { getAllocation } from "@/lib/api/cost";
import { unauthorized } from "@/lib/api/guard";
import { allocationCsv } from "@/lib/cost/allocation";
import { parseAggregate } from "@/lib/cost/types";
import { getSession } from "@/lib/session";
import { isCostWindow } from "@/lib/time-range";

const FILTER_KEYS = ["cluster", "namespace", "team", "cost_center", "node", "nodepool", "zone", "workload"];
const NS_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

export async function GET(req: NextRequest) {
  if (!(await getSession())) return unauthorized();
  const q = req.nextUrl.searchParams;
  const get = (k: string, max = 253) => (q.get(k) ?? "").trim().slice(0, max);
  const aggregate = parseAggregate(get("agg") || undefined);
  const window = isCostWindow(get("window")) ? get("window") : "7d";
  const idle = get("idle") || "separate";
  const filters: Record<string, string> = {};
  for (const k of FILTER_KEYS) if (get(k)) filters[k] = get(k);
  const shared = get("shared", 600).split(",").map((s) => s.trim()).filter((s) => NS_RE.test(s)).slice(0, 20);

  const res = await getAllocation({
    window,
    aggregate,
    filters,
    includeIdle: idle === "separate",
    shareIdle: idle === "weighted" || idle === "even" ? idle : "",
    sharedNamespaces: shared,
    forceDemo: get("demo") === "1",
  });
  const name = `kubehero-allocation-${aggregate.replace(/[^a-z0-9_-]/gi, "_")}-${window}${res.source === "demo" ? "-demo" : ""}.csv`;
  return new Response(allocationCsv(res.data.rows, res.source), {
    headers: {
      "Content-Type": "text/csv; charset=utf-8",
      "Content-Disposition": `attachment; filename="${name}"`,
      "Cache-Control": "no-store",
    },
  });
}
