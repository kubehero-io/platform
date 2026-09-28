// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  applyCompletion,
  completionContext,
  innerLogQuery,
  isMetricQuery,
  patternToRegex,
  tokenize,
  withMatcher,
  type TokenKind,
} from "./tokenize";

const kinds = (q: string) =>
  tokenize(q)
    .filter((t) => t.kind !== "space")
    .map((t) => [t.kind, t.text] as [TokenKind, string]);

describe("tokenize", () => {
  const SAMPLES = [
    '{namespace="shop", pod=~"cart-.*"} |= "timeout" != "debug" | json | status >= 500 | line_format "{{.msg}}"',
    'sum by (namespace) (count_over_time({level="error"}[5m])) > 100',
    'topk(5, sum by (workload) (rate({cluster="eks"} |~ "(?i)oom" [1m])))',
    '{app="x"} |= "unterminated',
    "{",
    "",
    '{a="\\"quoted\\""} | logfmt | duration > 1.5s',
    "`raw {string}`",
    "rate({a=\"b\"}[5m] offset 1h) * -1",
  ];

  it.each(SAMPLES)("round-trips %s", (q) => {
    expect(tokenize(q).map((t) => t.text).join("")).toBe(q);
  });

  it("classifies a log query", () => {
    expect(kinds('{namespace="shop", pod=~"cart-.*"} |= "timeout" != "debug" | json | status >= 500')).toEqual([
      ["brace", "{"],
      ["label", "namespace"],
      ["matcher", "="],
      ["string", '"shop"'],
      ["comma", ","],
      ["label", "pod"],
      ["matcher", "=~"],
      ["string", '"cart-.*"'],
      ["brace", "}"],
      ["pipe", "|="],
      ["string", '"timeout"'],
      ["pipe", "!="],
      ["string", '"debug"'],
      ["pipe", "|"],
      ["stage", "json"],
      ["pipe", "|"],
      ["label", "status"],
      ["operator", ">="],
      ["number", "500"],
    ]);
  });

  it("classifies a metric query", () => {
    expect(kinds('sum by (namespace) (count_over_time({level="error"}[5m])) > 100')).toEqual([
      ["function", "sum"],
      ["keyword", "by"],
      ["paren", "("],
      ["label", "namespace"],
      ["paren", ")"],
      ["paren", "("],
      ["function", "count_over_time"],
      ["paren", "("],
      ["brace", "{"],
      ["label", "level"],
      ["matcher", "="],
      ["string", '"error"'],
      ["brace", "}"],
      ["bracket", "["],
      ["duration", "5m"],
      ["bracket", "]"],
      ["paren", ")"],
      ["paren", ")"],
      ["operator", ">"],
      ["number", "100"],
    ]);
  });

  it("flags unterminated strings instead of throwing", () => {
    const t = tokenize('{app="x"} |= "oops').find((x) => x.kind === "string" && x.unterminated);
    expect(t?.text).toBe('"oops');
  });
});

describe("metric detection", () => {
  it.each([
    ['{app="x"}', false],
    ['  count_over_time({app="x"}[5m])', true],
    ['sum by (ns) (rate({a="b"}[1m]))', true],
    ["sum", false],
  ])("isMetricQuery(%s) = %s", (q, want) => {
    expect(isMetricQuery(q)).toBe(want);
  });

  it("extracts the inner log query", () => {
    expect(innerLogQuery('sum by (ns) (count_over_time({app="x"} |= "err" [5m])) > 3')).toBe('{app="x"} |= "err"');
    expect(innerLogQuery('rate({a="b"})')).toBe('{a="b"}');
    expect(innerLogQuery('  {a="b"} |= "x" ')).toBe('{a="b"} |= "x"');
  });
});

describe("completionContext", () => {
  it("label name inside a selector", () => {
    expect(completionContext("{na", 3)).toEqual({ kind: "label-name", where: "selector", prefix: "na", from: 1, to: 3 });
    expect(completionContext('{namespace="shop", ', 19)).toEqual({ kind: "label-name", where: "selector", prefix: "", from: 19, to: 19 });
  });

  it("label value inside quotes", () => {
    expect(completionContext('{namespace="sh', 14)).toEqual({ kind: "label-value", label: "namespace", prefix: "sh", from: 12, to: 14 });
    expect(completionContext('{namespace="shop"}', 14)).toEqual({ kind: "label-value", label: "namespace", prefix: "sh", from: 12, to: 16 });
  });

  it("label name in a by() list", () => {
    const q = "sum by (wor";
    expect(completionContext(q, q.length)).toMatchObject({ kind: "label-name", where: "by", prefix: "wor" });
  });

  it("nothing inside a line-filter string", () => {
    const q = '{a="b"} |= "time';
    expect(completionContext(q, q.length)).toBeNull();
  });
});

describe("applyCompletion", () => {
  it("completes a label name into name=\"\" with the cursor between quotes", () => {
    const ctx = completionContext("{na", 3)!;
    expect(applyCompletion("{na", ctx, "namespace")).toEqual({ query: '{namespace=""', cursor: 12 });
  });

  it("completes and closes a value", () => {
    const q = '{namespace="sh';
    expect(applyCompletion(q, completionContext(q, q.length)!, "shop")).toEqual({ query: '{namespace="shop"', cursor: 17 });
  });

  it("escapes quotes in values", () => {
    const q = '{msg="';
    expect(applyCompletion(q, completionContext(q, q.length)!, 'say "hi"').query).toBe('{msg="say \\"hi\\""');
  });
});

describe("withMatcher", () => {
  it("adds a matcher and keeps the pipeline", () => {
    expect(withMatcher('{app="x"} |= "y"', "pod", "cart-7d9f")).toBe('{app="x", pod="cart-7d9f"} |= "y"');
  });
  it("replaces an existing matcher for the same label", () => {
    expect(withMatcher('{app="x", pod="old"} | json', "pod", "new")).toBe('{app="x", pod="new"} | json');
  });
  it("creates a selector when there is none", () => {
    expect(withMatcher("", "level", "error")).toBe('{level="error"}');
    expect(withMatcher("{}", "level", "error")).toBe('{level="error"}');
  });
  it("supports negative matchers", () => {
    expect(withMatcher('{app="x"}', "level", "debug", "!=")).toBe('{app="x", level!="debug"}');
  });
});

describe("patternToRegex", () => {
  it("escapes literals and joins wildcards", () => {
    expect(patternToRegex("POST /api/checkout <_> in <_>ms (id=<_>)")).toBe("POST /api/checkout .*? in .*?ms \\(id=.*?\\)");
    expect(new RegExp(patternToRegex("a.b <_> c")).test("a.b 123 c")).toBe(true);
  });
});
