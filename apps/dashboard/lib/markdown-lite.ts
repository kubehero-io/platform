// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Deliberately tiny markdown parser for advisor briefings. Supports the
// subset the advisor emits — headings, paragraphs, bullet / ordered lists,
// fenced code blocks, and **bold** / *italic* / `code` inline spans.
// Pure and DOM-free so it runs under vitest's node environment; the React
// side (components/advisor/briefing-markdown.tsx) just maps blocks to JSX.
// Not a general markdown implementation — do not grow it into one; if the
// advisor ever needs tables/links/images, pick up a real dep instead.

export type InlineSpan =
  | { type: "text"; text: string }
  | { type: "bold"; text: string }
  | { type: "italic"; text: string }
  | { type: "code"; text: string };

export type Block =
  | { type: "heading"; level: 1 | 2 | 3 | 4; spans: InlineSpan[] }
  | { type: "paragraph"; spans: InlineSpan[] }
  | { type: "list"; ordered: boolean; items: InlineSpan[][] }
  | { type: "code"; text: string };

/** Split inline markdown into styled spans: **bold**, *italic*, `code`. */
export function parseInline(text: string): InlineSpan[] {
  const spans: InlineSpan[] = [];
  // Longest-first so ** wins over *.
  const re = /(\*\*([^*]+)\*\*|\*([^*]+)\*|`([^`]+)`)/g;
  let last = 0;
  for (let m = re.exec(text); m !== null; m = re.exec(text)) {
    if (m.index > last) spans.push({ type: "text", text: text.slice(last, m.index) });
    if (m[2] !== undefined) spans.push({ type: "bold", text: m[2] });
    else if (m[3] !== undefined) spans.push({ type: "italic", text: m[3] });
    else if (m[4] !== undefined) spans.push({ type: "code", text: m[4] });
    last = m.index + m[0].length;
  }
  if (last < text.length) spans.push({ type: "text", text: text.slice(last) });
  return spans;
}

const HEADING = /^(#{1,4})\s+(.*)$/;
const BULLET = /^[-*]\s+(.*)$/;
const ORDERED = /^\d+[.)]\s+(.*)$/;

/** Parse a markdown string into a flat list of blocks. */
export function parseMarkdown(md: string): Block[] {
  const blocks: Block[] = [];
  const lines = md.replace(/\r\n/g, "\n").split("\n");

  let para: string[] = [];
  let list: { ordered: boolean; items: string[] } | null = null;
  let code: string[] | null = null;

  const flushPara = () => {
    if (para.length > 0) {
      blocks.push({ type: "paragraph", spans: parseInline(para.join(" ")) });
      para = [];
    }
  };
  const flushList = () => {
    if (list) {
      blocks.push({
        type: "list",
        ordered: list.ordered,
        items: list.items.map(parseInline),
      });
      list = null;
    }
  };

  for (const raw of lines) {
    const line = raw.trimEnd();

    // Fenced code blocks swallow everything until the closing fence.
    if (code !== null) {
      if (line.trim().startsWith("```")) {
        blocks.push({ type: "code", text: code.join("\n") });
        code = null;
      } else {
        code.push(raw);
      }
      continue;
    }
    if (line.trim().startsWith("```")) {
      flushPara();
      flushList();
      code = [];
      continue;
    }

    if (line.trim() === "") {
      flushPara();
      flushList();
      continue;
    }

    const h = HEADING.exec(line);
    if (h) {
      flushPara();
      flushList();
      blocks.push({
        type: "heading",
        level: Math.min(h[1].length, 4) as 1 | 2 | 3 | 4,
        spans: parseInline(h[2]),
      });
      continue;
    }

    const b = BULLET.exec(line.trim());
    const o = b ? null : ORDERED.exec(line.trim());
    if (b || o) {
      flushPara();
      const ordered = !!o;
      if (!list || list.ordered !== ordered) {
        flushList();
        list = { ordered, items: [] };
      }
      list.items.push((b?.[1] ?? o?.[1] ?? "").trim());
      continue;
    }

    // Plain text: part of the current paragraph.
    flushList();
    para.push(line.trim());
  }

  // EOF flushes.
  if (code !== null) blocks.push({ type: "code", text: code.join("\n") });
  flushPara();
  flushList();
  return blocks;
}
