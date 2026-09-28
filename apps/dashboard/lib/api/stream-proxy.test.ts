// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { FLAG_END_STREAM, encodeEnvelope } from "./connect-stream";
import { connectToSse } from "./stream-proxy";
import { SseParser } from "@/lib/sse";

function body(chunks: Uint8Array[], opts: { closeWithoutEnd?: boolean } = {}): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(c) {
      for (const ch of chunks) c.enqueue(ch);
      if (!opts.closeWithoutEnd) c.close();
      else c.close();
    },
  });
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

async function collect(s: ReadableStream<Uint8Array>) {
  const reader = s.getReader();
  const dec = new TextDecoder();
  const p = new SseParser();
  const events: { event: string; data: unknown }[] = [];
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    for (const m of p.push(dec.decode(value, { stream: true }))) events.push({ event: m.event, data: JSON.parse(m.data || "null") });
  }
  return events;
}

const tailMap = (m: unknown) => {
  const msg = m as { lines?: { body: string }[]; dropped?: string };
  return [{ event: "lines", data: { n: msg.lines?.length ?? 0, dropped: Number(msg.dropped ?? 0) } }];
};

describe("connectToSse", () => {
  it("maps messages and ends cleanly on an end-stream frame, across odd chunking", async () => {
    const wire = concat(
      encodeEnvelope({ lines: [{ body: "a" }, { body: "b" }] }),
      encodeEnvelope({ lines: [], dropped: "4" }),
      encodeEnvelope({}, FLAG_END_STREAM),
    );
    // Split into 3-byte chunks to exercise the incremental decoder.
    const chunks: Uint8Array[] = [];
    for (let i = 0; i < wire.length; i += 3) chunks.push(wire.slice(i, i + 3));
    const events = await collect(connectToSse(body(chunks), { map: tailMap }));
    expect(events).toEqual([
      { event: "lines", data: { n: 2, dropped: 0 } },
      { event: "lines", data: { n: 0, dropped: 4 } },
      { event: "end", data: {} },
    ]);
  });

  it("turns an end-stream error into error + end", async () => {
    const wire = concat(
      encodeEnvelope({ lines: [{ body: "a" }] }),
      encodeEnvelope({ error: { code: "permission_denied", message: "requires role member" } }, FLAG_END_STREAM),
    );
    const events = await collect(connectToSse(body([wire]), { map: tailMap }));
    expect(events.slice(1)).toEqual([
      { event: "error", data: { code: "permission_denied", message: "requires role member" } },
      { event: "end", data: {} },
    ]);
  });

  it("reports a stream that dies without an end-stream frame", async () => {
    const events = await collect(connectToSse(body([encodeEnvelope({ lines: [] })]), { map: tailMap }));
    expect(events.at(-2)).toEqual({ event: "error", data: { code: "unavailable", message: "upstream stream ended unexpectedly" } });
  });

  it("refuses compressed envelopes (never negotiated)", async () => {
    const env = encodeEnvelope({ lines: [] });
    env[0] = 0x01;
    const events = await collect(connectToSse(body([env]), { map: tailMap }));
    expect(events[0]).toEqual({ event: "error", data: { code: "internal", message: "compressed envelopes are not negotiated" } });
  });

  it("stops when the client aborts", async () => {
    const ctl = new AbortController();
    const never = new ReadableStream<Uint8Array>({ start() {} });
    const out = connectToSse(never, { map: tailMap, signal: ctl.signal, heartbeatMs: 10_000 });
    const reader = out.getReader();
    ctl.abort();
    const r = await reader.read();
    expect(r.done).toBe(true);
  });
});
