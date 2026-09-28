// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Live tail: LogsService.TailLogs (a Connect server stream) re-framed as
// Server-Sent Events for the browser. Auth is the caller's own session
// (API routes are outside proxy.ts's matcher, so we check here) and the
// upstream call carries the user's token. Without a control plane (or
// with one that has no LogsService yet) the demo store is tailed instead
// — one batch per second.
//
// GET /api/logs/tail?q=<logql>[&cluster=<id>]
//   event: lines  data: {lines: LogLine[], dropped: number}
//   event: error  data: {code, message}
//   event: end    data: {}

import { type NextRequest } from "next/server";
import { demoTail } from "@/lib/demo/logs";
import { unauthorized } from "@/lib/api/guard";
import { openServerStream, upstreamBase } from "@/lib/api/rpc";
import { connectToSse, SSE_HEADERS } from "@/lib/api/stream-proxy";
import { LogQLError, parseLogQL } from "@/lib/logql/parse";
import { isMetricQuery } from "@/lib/logql/tokenize";
import { lineId, toLogLines } from "@/lib/logs/map";
import type { LogLine, TailLogsResponseJson } from "@/lib/logs/types";
import { getSession } from "@/lib/session";
import { SSE_COMMENT_PING, sseEvent } from "@/lib/sse";

export const dynamic = "force-dynamic";

const MAX_QUERY = 4096;
const MAX_STREAM_MS = 30 * 60_000;
const MAX_LINES_PER_EVENT = 500;

export async function GET(req: NextRequest) {
  if (!(await getSession())) return unauthorized();
  const query = (req.nextUrl.searchParams.get("q") ?? "").trim();
  const cluster = (req.nextUrl.searchParams.get("cluster") ?? "").slice(0, 253);
  if (!query || query.length > MAX_QUERY) {
    return Response.json({ error: { code: "invalid_argument", message: "q must be a LogQL log query" } }, { status: 400 });
  }
  if (isMetricQuery(query)) {
    return Response.json({ error: { code: "invalid_argument", message: "live tail needs a log query, not a metric query" } }, { status: 400 });
  }

  if (!upstreamBase("cp")) return demoTailResponse(query, req.signal);

  const opened = await openServerStream(
    "cp",
    "kubehero.v1.LogsService",
    "TailLogs",
    { query, startUnixMs: String(Date.now() - 10_000), clusterId: cluster },
    { signal: req.signal },
  );
  if (!opened.ok) {
    // A control plane that predates LogsService: tail the demo store, as
    // the page's own queries already fell back to it (its badge says demo).
    if (opened.error.code === "unimplemented") return demoTailResponse(query, req.signal);
    const status = opened.error.code === "unauthenticated" ? 401 : opened.error.code === "permission_denied" ? 403 : opened.error.code === "invalid_argument" ? 400 : 502;
    return Response.json({ error: { code: opened.error.code, message: opened.error.message } }, { status });
  }
  const upstream = opened.res.body;
  if (!upstream) return Response.json({ error: { code: "unavailable", message: "empty upstream body" } }, { status: 502 });

  const stream = connectToSse(upstream, {
    signal: req.signal,
    maxDurationMs: MAX_STREAM_MS,
    map: (m) => {
      const msg = m as TailLogsResponseJson;
      const lines = toLogLines(msg.lines, "forward").slice(-MAX_LINES_PER_EVENT);
      const dropped = Number(msg.dropped ?? 0) || 0;
      return lines.length > 0 || dropped > 0 ? [{ event: "lines", data: { lines, dropped } }] : [];
    },
  });
  return new Response(stream, { headers: SSE_HEADERS });
}

function demoTailResponse(query: string, signal: AbortSignal): Response {
  try {
    parseLogQL(query);
  } catch (e) {
    const message = e instanceof LogQLError ? e.message : "invalid query";
    return Response.json({ error: { code: "invalid_argument", message } }, { status: 400 });
  }
  const enc = new TextEncoder();
  let timer: ReturnType<typeof setInterval> | undefined;
  let ping: ReturnType<typeof setInterval> | undefined;
  const started = Date.now();
  let cursor = started - 10_000;
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const stop = () => {
        clearInterval(timer);
        clearInterval(ping);
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      };
      signal.addEventListener("abort", stop, { once: true });
      const tick = () => {
        const now = Date.now();
        if (now - started > MAX_STREAM_MS) {
          controller.enqueue(enc.encode(sseEvent("end", { reason: "max-duration" })));
          stop();
          return;
        }
        const lines: LogLine[] = demoTail(query, cursor, now, now).map((l) => ({
          id: lineId(l.tsNs, l.labels, l.body),
          tsMs: l.tsMs,
          tsNs: l.tsNs,
          body: l.body,
          level: l.level,
          labels: l.labels,
        }));
        cursor = now;
        if (lines.length > 0) {
          controller.enqueue(enc.encode(sseEvent("lines", { lines: lines.slice(-MAX_LINES_PER_EVENT), dropped: Math.max(0, lines.length - MAX_LINES_PER_EVENT), demo: true })));
        }
      };
      tick();
      timer = setInterval(tick, 1_000);
      ping = setInterval(() => controller.enqueue(enc.encode(SSE_COMMENT_PING)), 15_000);
    },
    cancel() {
      clearInterval(timer);
      clearInterval(ping);
    },
  });
  return new Response(stream, { headers: SSE_HEADERS });
}
