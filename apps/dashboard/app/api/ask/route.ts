// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /ask transport: AdvisorService.InvestigateStream (a Connect server
// stream) re-framed as Server-Sent Events. POST so the question stays out
// of URLs and logs. The call carries the signed-in user's token (see
// lib/api/rpc.ts). Without an advisor the scripted demo agent answers,
// paced like the real one, labelled source "demo".
//
// POST /api/ask  {question, context?, window?, clusterId?}
//   event: step      {tool, input, summary, durationMs, error}
//   event: progress  {text}
//   event: result    Investigation
//   event: error     {code, message}
//   event: end       {}

import { type NextRequest } from "next/server";
import { streamMessageToEvents, type InvestigateStreamResponseJson } from "@/lib/advisor/investigate";
import { demoInvestigation } from "@/lib/demo/investigate";
import { unauthorized } from "@/lib/api/guard";
import { openServerStream, upstreamBase } from "@/lib/api/rpc";
import { connectToSse, SSE_HEADERS } from "@/lib/api/stream-proxy";
import { safeLinkPath } from "@/lib/alerts/rules";
import { getSession } from "@/lib/session";
import { sseEvent } from "@/lib/sse";

export const dynamic = "force-dynamic";

const MAX_QUESTION = 2000;
const WINDOWS = new Set(["1h", "24h", "7d"]);

function bad(message: string, status = 400) {
  return Response.json({ error: { code: "invalid_argument", message } }, { status });
}

export async function POST(req: NextRequest) {
  if (!(await getSession())) return unauthorized();
  const len = Number(req.headers.get("content-length") ?? "0");
  if (len > 16 * 1024) return bad("request too large", 413);
  let body: { question?: unknown; context?: unknown; window?: unknown; clusterId?: unknown };
  try {
    body = (await req.json()) as typeof body;
  } catch {
    return bad("body must be JSON");
  }
  const question = typeof body.question === "string" ? body.question.trim() : "";
  if (!question || question.length > MAX_QUESTION) return bad(`question must be 1–${MAX_QUESTION} characters`);
  const context = typeof body.context === "string" ? safeLinkPath(body.context.trim()) : "";
  const window = typeof body.window === "string" && WINDOWS.has(body.window) ? body.window : "24h";
  const clusterId = typeof body.clusterId === "string" && /^[a-z0-9-]{0,63}$/.test(body.clusterId) ? body.clusterId : "";

  if (!upstreamBase("advisor")) return demoStream(question, req.signal);

  const opened = await openServerStream(
    "advisor",
    "kubehero.v1.AdvisorService",
    "InvestigateStream",
    { request: { question, clusterId, window, context } },
    { signal: req.signal, timeoutMs: 30_000 },
  );
  if (!opened.ok) {
    // Before any byte was streamed we can still degrade to the demo agent,
    // clearly labelled — except for auth errors, which the user must see.
    if (opened.error.code === "unauthenticated" || opened.error.code === "permission_denied") {
      return Response.json({ error: { code: opened.error.code, message: opened.error.message } }, { status: opened.error.code === "unauthenticated" ? 401 : 403 });
    }
    return demoStream(question, req.signal, `advisor unavailable (${opened.error.code}) — answering with the demo agent`);
  }
  if (!opened.res.body) return demoStream(question, req.signal, "advisor sent an empty stream — answering with the demo agent");
  const stream = connectToSse(opened.res.body, {
    signal: req.signal,
    maxDurationMs: 150_000,
    map: (m) => streamMessageToEvents(m as InvestigateStreamResponseJson),
  });
  return new Response(stream, { headers: SSE_HEADERS });
}

function demoStream(question: string, signal: AbortSignal, note?: string): Response {
  const { steps, progress, result } = demoInvestigation(question);
  const enc = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const send = (event: string, data: unknown) => {
        if (!signal.aborted) controller.enqueue(enc.encode(sseEvent(event, data)));
      };
      const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
      if (note) send("progress", { text: note });
      send("progress", { text: progress[0] });
      await sleep(250);
      send("progress", { text: progress[1] });
      for (const s of steps) {
        if (signal.aborted) break;
        // Pace like the real agent, capped so the demo stays snappy.
        await sleep(Math.min(700, Math.max(180, s.durationMs)));
        send("step", s);
      }
      await sleep(300);
      send("progress", { text: progress[2] });
      await sleep(350);
      send("result", result);
      send("end", {});
      try {
        controller.close();
      } catch {
        /* client went away */
      }
    },
  });
  return new Response(stream, { headers: SSE_HEADERS });
}
