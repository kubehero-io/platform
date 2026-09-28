// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Server-Sent Events, both directions, no dependencies:
//   · sseEvent() — what the route handlers write;
//   · SseParser  — what the browser reads out of a fetch() body (we use
//     fetch + ReadableStream rather than EventSource so the request can
//     be a POST, carry no cookies across origins, and be aborted cleanly).
// Implements the WHATWG event-stream rules we rely on: `event:`, `data:`
// (multi-line joined with \n), `id:`, comments (`:`), blank-line dispatch,
// CRLF/CR/LF line endings, chunk boundaries anywhere.

export function sseEvent(event: string, data: unknown, id?: string): string {
  const payload = typeof data === "string" ? data : JSON.stringify(data);
  const lines = payload.split(/\r\n|\r|\n/).map((l) => `data: ${l}`);
  return `${id ? `id: ${id}\n` : ""}event: ${event}\n${lines.join("\n")}\n\n`;
}

export const SSE_COMMENT_PING = ": ping\n\n";

export type SseMessage = { event: string; data: string; id?: string };

export class SseParser {
  private buf = "";
  private event = "";
  private data: string[] = [];
  private id: string | undefined;

  /** Feed decoded text; returns every message completed by this chunk. */
  push(chunk: string): SseMessage[] {
    this.buf += chunk;
    const out: SseMessage[] = [];
    for (;;) {
      const m = /\r\n|\r|\n/.exec(this.buf);
      if (!m) break;
      // A lone \r at the very end might be the first half of \r\n.
      if (m[0] === "\r" && m.index === this.buf.length - 1) break;
      const line = this.buf.slice(0, m.index);
      this.buf = this.buf.slice(m.index + m[0].length);
      if (line === "") {
        if (this.data.length > 0 || this.event) {
          out.push({ event: this.event || "message", data: this.data.join("\n"), id: this.id });
        }
        this.event = "";
        this.data = [];
        continue;
      }
      if (line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      const field = colon === -1 ? line : line.slice(0, colon);
      let value = colon === -1 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);
      if (field === "event") this.event = value;
      else if (field === "data") this.data.push(value);
      else if (field === "id") this.id = value;
    }
    // Guard: a peer that never sends a newline must not grow memory forever.
    if (this.buf.length > 4 * 1024 * 1024) this.buf = "";
    return out;
  }
}

/** Read an SSE response body, calling onMessage per event. Resolves at end. */
export async function readSse(res: Response, onMessage: (m: SseMessage) => void, signal?: AbortSignal): Promise<void> {
  if (!res.body) return;
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  const p = new SseParser();
  const onAbort = () => reader.cancel().catch(() => {});
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      for (const m of p.push(dec.decode(value, { stream: true }))) onMessage(m);
    }
    for (const m of p.push(dec.decode() + "\n\n")) onMessage(m);
  } finally {
    signal?.removeEventListener("abort", onAbort);
  }
}
