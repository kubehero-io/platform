// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { SseParser, readSse, sseEvent } from "./sse";

describe("sseEvent", () => {
  it("frames JSON and splits multi-line strings", () => {
    expect(sseEvent("step", { a: 1 })).toBe('event: step\ndata: {"a":1}\n\n');
    expect(sseEvent("note", "one\ntwo", "7")).toBe("id: 7\nevent: note\ndata: one\ndata: two\n\n");
  });
});

describe("SseParser", () => {
  const stream = sseEvent("lines", { n: 1 }) + ": ping\n\n" + sseEvent("end", "") + "data: plain\r\n\r\n";

  it("parses events, skips comments, defaults the event name", () => {
    const p = new SseParser();
    expect(p.push(stream)).toEqual([
      { event: "lines", data: '{"n":1}', id: undefined },
      { event: "end", data: "", id: undefined },
      { event: "message", data: "plain", id: undefined },
    ]);
  });

  it("is chunk-boundary agnostic", () => {
    for (let cut = 0; cut < stream.length; cut++) {
      const p = new SseParser();
      const got = [...p.push(stream.slice(0, cut)), ...p.push(stream.slice(cut))];
      expect(got.map((m) => m.event)).toEqual(["lines", "end", "message"]);
    }
  });

  it("joins multi-line data", () => {
    expect(new SseParser().push("data: a\ndata: b\n\n")).toEqual([{ event: "message", data: "a\nb", id: undefined }]);
  });
});

describe("readSse", () => {
  it("reads a streamed Response body", async () => {
    const enc = new TextEncoder();
    const parts = [sseEvent("a", 1), sseEvent("b", 2).slice(0, 5), sseEvent("b", 2).slice(5)];
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        for (const p of parts) c.enqueue(enc.encode(p));
        c.close();
      },
    });
    const got: string[] = [];
    await readSse(new Response(body), (m) => got.push(`${m.event}=${m.data}`));
    expect(got).toEqual(["a=1", "b=2"]);
  });
});
