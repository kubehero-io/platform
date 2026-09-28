// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// FinOps FOCUS export. Live: a streaming proxy to the control plane's
// GET /api/v1/export/focus with the signed-in user's token (the CSV can
// be large, so it is piped through, never buffered). Demo (no control
// plane): FOCUS-shaped rows from the demo allocation, marked
// x_KubeHeroSource=demo and "-demo" in the filename. A failing control
// plane is an error here, never silently demo data — this file may end
// up in a finance system.

import { type NextRequest } from "next/server";
import { demoAllocation } from "@/lib/demo/cost";
import { focusCsv } from "@/lib/cost/focus";
import { unauthorized } from "@/lib/api/guard";
import { resolveCredential, upstreamBase } from "@/lib/api/rpc";
import { getSession } from "@/lib/session";
import { isCostWindow } from "@/lib/time-range";

const AGGREGATES = new Set(["namespace", "workload", "team", "cluster", "controller", "node", "nodepool", "zone", "pod", "container"]);

export async function GET(req: NextRequest) {
  if (!(await getSession())) return unauthorized();
  const q = req.nextUrl.searchParams;
  const window = isCostWindow(q.get("window") ?? "") ? (q.get("window") as string) : "30d";
  const aggregate = AGGREGATES.has(q.get("aggregate") ?? "") ? (q.get("aggregate") as string) : "workload";
  const base = upstreamBase("cp");

  if (!base) {
    const v = demoAllocation({ window, aggregate: "workload", shareIdle: "weighted" });
    return new Response(focusCsv(v.rows, v.startMs, v.endMs, "demo"), {
      headers: {
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": `attachment; filename="kubehero-focus-${window}-demo.csv"`,
        "Cache-Control": "no-store",
      },
    });
  }

  const cred = await resolveCredential("cp");
  const ctl = new AbortController();
  req.signal.addEventListener("abort", () => ctl.abort(), { once: true });
  const timer = setTimeout(() => ctl.abort(), 30_000);
  try {
    const upstream = await fetch(`${base}/api/v1/export/focus?${new URLSearchParams({ window, aggregate }).toString()}`, {
      headers: { Accept: "text/csv", ...(cred.header ? { Authorization: cred.header } : {}) },
      cache: "no-store",
      signal: ctl.signal,
    });
    clearTimeout(timer);
    if (!upstream.ok || !upstream.body) {
      const status = upstream.status === 401 || upstream.status === 403 ? upstream.status : 502;
      console.error("[control-plane] focus export", upstream.status);
      return new Response(`FOCUS export failed: control plane answered HTTP ${upstream.status}\n`, {
        status,
        headers: { "Content-Type": "text/plain; charset=utf-8" },
      });
    }
    return new Response(upstream.body, {
      headers: {
        "Content-Type": upstream.headers.get("content-type") ?? "text/csv; charset=utf-8",
        "Content-Disposition": `attachment; filename="kubehero-focus-${aggregate}-${window}.csv"`,
        "Cache-Control": "no-store",
      },
    });
  } catch (err) {
    clearTimeout(timer);
    console.error("[control-plane] focus export failed", err instanceof Error ? err.message : err);
    return new Response("FOCUS export failed: control plane unreachable\n", {
      status: 502,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  }
}
