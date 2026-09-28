// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Connect protocol streaming envelopes — the wire format of server-stream
// RPCs (TailLogs, InvestigateStream) with Content-Type
// application/connect+json:
//
//   ┌────────┬──────────────────────┬──────────────────┐
//   │ flags  │ length (uint32, BE)  │ payload (length) │
//   │ 1 byte │ 4 bytes              │ JSON bytes       │
//   └────────┴──────────────────────┴──────────────────┘
//
// flags 0x01 = compressed (we never negotiate compression, so it is an
// error), 0x02 = end-of-stream: the payload is an EndStreamResponse
// `{"error"?: {code, message, details}, "metadata"?: {...}}` instead of a
// message. Fetch hands us arbitrary chunk boundaries, so the decoder is
// incremental: it buffers partial headers/payloads across push() calls.
//
// Pure and DOM-free (Uint8Array + TextEncoder only) so it is unit-tested
// under vitest and usable from route handlers.

export const FLAG_COMPRESSED = 0x01;
export const FLAG_END_STREAM = 0x02;

/** Refuse single messages above this — a hostile or broken upstream must
 *  not make the dashboard buffer unbounded memory. */
export const MAX_ENVELOPE_BYTES = 8 * 1024 * 1024;

export type Envelope = { flags: number; data: Uint8Array };

export type EndStream = {
  error?: { code: string; message: string };
  metadata?: Record<string, string[]>;
};

const enc = new TextEncoder();
const dec = new TextDecoder();

export function encodeEnvelope(message: unknown, flags = 0): Uint8Array {
  const payload = enc.encode(JSON.stringify(message ?? {}));
  const out = new Uint8Array(5 + payload.length);
  out[0] = flags;
  new DataView(out.buffer).setUint32(1, payload.length, false);
  out.set(payload, 5);
  return out;
}

export class EnvelopeDecoder {
  private buf = new Uint8Array(0);

  /** Feed a chunk; returns every envelope that is now complete. */
  push(chunk: Uint8Array): Envelope[] {
    if (chunk.length > 0) {
      const merged = new Uint8Array(this.buf.length + chunk.length);
      merged.set(this.buf, 0);
      merged.set(chunk, this.buf.length);
      this.buf = merged;
    }
    const out: Envelope[] = [];
    let off = 0;
    while (this.buf.length - off >= 5) {
      const flags = this.buf[off];
      const len = new DataView(this.buf.buffer, this.buf.byteOffset + off + 1, 4).getUint32(0, false);
      if (len > MAX_ENVELOPE_BYTES) {
        throw new Error(`connect envelope of ${len} bytes exceeds the ${MAX_ENVELOPE_BYTES}-byte cap`);
      }
      if (this.buf.length - off - 5 < len) break; // wait for the rest
      out.push({ flags, data: this.buf.slice(off + 5, off + 5 + len) });
      off += 5 + len;
    }
    this.buf = off === 0 ? this.buf : this.buf.slice(off);
    return out;
  }

  /** Bytes buffered but not yet forming a complete envelope. */
  get pending(): number {
    return this.buf.length;
  }
}

export function isEndStream(e: Envelope): boolean {
  return (e.flags & FLAG_END_STREAM) === FLAG_END_STREAM;
}

export function parseJson<T>(data: Uint8Array): T {
  return JSON.parse(dec.decode(data)) as T;
}

/** Decode an end-of-stream payload; malformed JSON reads as an internal error. */
export function parseEndStream(data: Uint8Array): EndStream {
  if (data.length === 0) return {};
  try {
    const v = parseJson<EndStream>(data);
    if (v && typeof v === "object") {
      if (v.error && typeof v.error === "object") {
        return {
          error: {
            code: typeof v.error.code === "string" ? v.error.code : "unknown",
            message: typeof v.error.message === "string" ? v.error.message : "",
          },
          metadata: v.metadata,
        };
      }
      return { metadata: v.metadata };
    }
  } catch {
    /* fall through */
  }
  return { error: { code: "internal", message: "malformed end-of-stream message" } };
}
