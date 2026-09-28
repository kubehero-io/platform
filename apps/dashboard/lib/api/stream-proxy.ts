// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Connect server-stream → browser SSE. The route handlers (live tail,
// /ask) open the upstream stream with the user's credentials (rpc.ts),
// then hand the body to connectToSse(), which:
//   · decodes 5-byte-framed envelopes incrementally (connect-stream.ts),
//   · maps each JSON message to zero or more SSE events,
//   · turns the end-of-stream envelope into `end` (or `error` + `end`),
//   · reports a stream that dies without an end-stream frame as an error,
//   · sends comment heartbeats so idle proxies don't cut the connection,
//   · stops reading upstream the moment the browser goes away.
// Pure web-streams code — unit-tested with synthetic upstream bodies.

import { EnvelopeDecoder, FLAG_COMPRESSED, isEndStream, parseEndStream, parseJson } from "./connect-stream";
import { SSE_COMMENT_PING, sseEvent } from "@/lib/sse";

export type SseOut = { event: string; data: unknown };

export function connectToSse(
  upstream: ReadableStream<Uint8Array>,
  opts: {
    map: (message: unknown) => SseOut[];
    signal?: AbortSignal;
    heartbeatMs?: number;
    /** Hard cap on how long one stream may stay open (client reconnects). */
    maxDurationMs?: number;
  },
): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  const decoder = new EnvelopeDecoder();
  const reader = upstream.getReader();
  let heartbeat: ReturnType<typeof setInterval> | undefined;
  let deadline: ReturnType<typeof setTimeout> | undefined;
  let finished = false;

  return new ReadableStream<Uint8Array>({
    start(controller) {
      const write = (s: string) => {
        if (!finished) controller.enqueue(enc.encode(s));
      };
      const close = () => {
        if (finished) return;
        finished = true;
        clearInterval(heartbeat);
        clearTimeout(deadline);
        reader.cancel().catch(() => {});
        controller.close();
      };
      const fail = (code: string, message: string) => {
        write(sseEvent("error", { code, message }));
        write(sseEvent("end", {}));
        close();
      };

      heartbeat = setInterval(() => write(SSE_COMMENT_PING), opts.heartbeatMs ?? 15_000);
      if (opts.maxDurationMs) {
        deadline = setTimeout(() => {
          write(sseEvent("end", { reason: "max-duration" }));
          close();
        }, opts.maxDurationMs);
      }
      opts.signal?.addEventListener("abort", close, { once: true });

      (async () => {
        try {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            for (const env of decoder.push(value)) {
              if (env.flags & FLAG_COMPRESSED) {
                fail("internal", "compressed envelopes are not negotiated");
                return;
              }
              if (isEndStream(env)) {
                const end = parseEndStream(env.data);
                if (end.error) fail(end.error.code, end.error.message);
                else {
                  write(sseEvent("end", {}));
                  close();
                }
                return;
              }
              let msg: unknown;
              try {
                msg = parseJson(env.data);
              } catch {
                fail("internal", "malformed stream message");
                return;
              }
              for (const out of opts.map(msg)) write(sseEvent(out.event, out.data));
            }
          }
          // Body ended without the mandatory end-of-stream frame.
          if (!finished) fail("unavailable", "upstream stream ended unexpectedly");
        } catch (err) {
          if (!finished) fail("unavailable", err instanceof Error ? err.message : "stream failed");
        }
      })();
    },
    cancel() {
      finished = true;
      clearInterval(heartbeat);
      clearTimeout(deadline);
      reader.cancel().catch(() => {});
    },
  });
}

export const SSE_HEADERS: Record<string, string> = {
  "Content-Type": "text/event-stream; charset=utf-8",
  "Cache-Control": "no-cache, no-transform",
  Connection: "keep-alive",
  // nginx / ingress-nginx: don't buffer the stream.
  "X-Accel-Buffering": "no",
};
