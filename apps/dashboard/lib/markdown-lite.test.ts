// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { parseInline, parseMarkdown } from "./markdown-lite";
import { DEMO_BRIEFING } from "./advisor-demo";

describe("parseInline", () => {
  it("passes plain text through as a single span", () => {
    expect(parseInline("hello world")).toEqual([{ type: "text", text: "hello world" }]);
  });

  it("splits bold, italic, and code spans", () => {
    expect(parseInline("a **b** *c* `d` e")).toEqual([
      { type: "text", text: "a " },
      { type: "bold", text: "b" },
      { type: "text", text: " " },
      { type: "italic", text: "c" },
      { type: "text", text: " " },
      { type: "code", text: "d" },
      { type: "text", text: " e" },
    ]);
  });

  it("handles spans at the start and end of the line", () => {
    expect(parseInline("**start** and `end`")).toEqual([
      { type: "bold", text: "start" },
      { type: "text", text: " and " },
      { type: "code", text: "end" },
    ]);
  });

  it("returns an empty array for the empty string", () => {
    expect(parseInline("")).toEqual([]);
  });
});

describe("parseMarkdown", () => {
  it("parses headings at levels 1-4", () => {
    const blocks = parseMarkdown("# one\n\n## two\n\n#### four");
    expect(blocks).toEqual([
      { type: "heading", level: 1, spans: [{ type: "text", text: "one" }] },
      { type: "heading", level: 2, spans: [{ type: "text", text: "two" }] },
      { type: "heading", level: 4, spans: [{ type: "text", text: "four" }] },
    ]);
  });

  it("joins consecutive lines into one paragraph and splits on blanks", () => {
    const blocks = parseMarkdown("line one\nline two\n\nsecond para");
    expect(blocks).toHaveLength(2);
    expect(blocks[0]).toMatchObject({ type: "paragraph" });
    expect(blocks[0]).toEqual({
      type: "paragraph",
      spans: [{ type: "text", text: "line one line two" }],
    });
  });

  it("groups bullet items into a single unordered list", () => {
    const blocks = parseMarkdown("- a\n- b\n* c");
    expect(blocks).toEqual([
      {
        type: "list",
        ordered: false,
        items: [
          [{ type: "text", text: "a" }],
          [{ type: "text", text: "b" }],
          [{ type: "text", text: "c" }],
        ],
      },
    ]);
  });

  it("parses ordered lists separately from bullets", () => {
    const blocks = parseMarkdown("1. first\n2. second\n\n- bullet");
    expect(blocks).toHaveLength(2);
    expect(blocks[0]).toMatchObject({ type: "list", ordered: true });
    expect(blocks[1]).toMatchObject({ type: "list", ordered: false });
  });

  it("keeps inline markup inside list items", () => {
    const blocks = parseMarkdown("- **bold** item");
    expect(blocks[0]).toEqual({
      type: "list",
      ordered: false,
      items: [
        [
          { type: "bold", text: "bold" },
          { type: "text", text: " item" },
        ],
      ],
    });
  });

  it("captures fenced code blocks verbatim, including an unclosed fence at EOF", () => {
    const closed = parseMarkdown("```\nkubectl apply -f x.yaml\n```");
    expect(closed).toEqual([{ type: "code", text: "kubectl apply -f x.yaml" }]);

    const unclosed = parseMarkdown("```\nno closing fence");
    expect(unclosed).toEqual([{ type: "code", text: "no closing fence" }]);
  });

  it("returns no blocks for empty input", () => {
    expect(parseMarkdown("")).toEqual([]);
    expect(parseMarkdown("\n\n\n")).toEqual([]);
  });

  it("parses the demo briefing without dropping content", () => {
    const blocks = parseMarkdown(DEMO_BRIEFING.markdown);
    const headings = blocks.filter((b) => b.type === "heading");
    const paragraphs = blocks.filter((b) => b.type === "paragraph");
    const lists = blocks.filter((b) => b.type === "list");
    expect(headings.length).toBeGreaterThanOrEqual(3);
    expect(paragraphs.length).toBeGreaterThanOrEqual(6);
    expect(lists.length).toBeGreaterThanOrEqual(2);
    // Nothing in the fixture should parse to an empty paragraph.
    for (const p of paragraphs) {
      expect(p.type === "paragraph" && p.spans.length).toBeTruthy();
    }
  });
});
