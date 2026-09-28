// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  EnvelopeDecoder,
  FLAG_END_STREAM,
  MAX_ENVELOPE_BYTES,
  encodeEnvelope,
  isEndStream,
  parseEndStream,
  parseJson,
} from "./connect-stream";

function concat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

describe("encodeEnvelope", () => {
  it("writes flags + big-endian length + JSON", () => {
    const e = encodeEnvelope({ query: "{app=\"x\"}" });
    const json = '{"query":"{app=\\"x\\"}"}';
    expect(e[0]).toBe(0);
    expect(new DataView(e.buffer).getUint32(1, false)).toBe(json.length);
    expect(new TextDecoder().decode(e.subarray(5))).toBe(json);
  });

  it("sets the end-stream flag", () => {
    expect(encodeEnvelope({}, FLAG_END_STREAM)[0]).toBe(0x02);
  });
});

describe("EnvelopeDecoder", () => {
  const a = encodeEnvelope({ lines: [{ body: "one", tsUnixNano: "1727500000123456789" }] });
  const b = encodeEnvelope({ lines: [], dropped: "3" });
  const end = encodeEnvelope({}, FLAG_END_STREAM);
  const all = concat(a, b, end);

  it("decodes several envelopes from one chunk", () => {
    const d = new EnvelopeDecoder();
    const out = d.push(all);
    expect(out).toHaveLength(3);
    expect(parseJson<{ lines: { body: string }[] }>(out[0].data).lines[0].body).toBe("one");
    expect(parseJson<{ dropped: string }>(out[1].data).dropped).toBe("3");
    expect(isEndStream(out[2])).toBe(true);
    expect(d.pending).toBe(0);
  });

  it("handles every possible split point, byte by byte", () => {
    for (let cut = 0; cut <= all.length; cut++) {
      const d = new EnvelopeDecoder();
      const got = [...d.push(all.subarray(0, cut)), ...d.push(all.subarray(cut))];
      expect(got.map((e) => e.flags)).toEqual([0, 0, 2]);
    }
    const d = new EnvelopeDecoder();
    const got = [];
    for (const byte of all) got.push(...d.push(new Uint8Array([byte])));
    expect(got).toHaveLength(3);
  });

  it("keeps a partial header buffered", () => {
    const d = new EnvelopeDecoder();
    expect(d.push(a.subarray(0, 3))).toEqual([]);
    expect(d.pending).toBe(3);
  });

  it("refuses oversized envelopes", () => {
    const hdr = new Uint8Array(5);
    new DataView(hdr.buffer).setUint32(1, MAX_ENVELOPE_BYTES + 1, false);
    expect(() => new EnvelopeDecoder().push(hdr)).toThrow(/cap/);
  });
});

describe("parseEndStream", () => {
  const enc = (s: string) => new TextEncoder().encode(s);

  it("reads a clean end", () => {
    expect(parseEndStream(enc("{}"))).toEqual({ metadata: undefined });
    expect(parseEndStream(new Uint8Array(0))).toEqual({});
  });

  it("reads an error end", () => {
    expect(parseEndStream(enc('{"error":{"code":"permission_denied","message":"nope"}}'))).toEqual({
      error: { code: "permission_denied", message: "nope" },
      metadata: undefined,
    });
  });

  it("treats garbage as an internal error", () => {
    expect(parseEndStream(enc("{not json"))).toEqual({
      error: { code: "internal", message: "malformed end-of-stream message" },
    });
  });
});
