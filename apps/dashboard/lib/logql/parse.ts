// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// A small LogQL parser for DEMO mode only: it lets the demo log store
// answer the same queries the control plane would (selectors, line
// filters, json/logfmt + label filters, range functions, vector
// aggregations, comparisons with a scalar). Live mode never uses it —
// the control plane's parser is the real one. Errors carry a position so
// the editor can point at them.

import { AGG_FUNCTIONS, RANGE_FUNCTIONS, tokenize, type Token } from "./tokenize";

export type Matcher = { label: string; op: "=" | "!=" | "=~" | "!~"; value: string };
export type LineFilter = { op: "|=" | "!=" | "|~" | "!~"; value: string };
export type LabelFilter = { label: string; op: "=" | "!=" | "=~" | "!~" | ">" | ">=" | "<" | "<=" | "=="; value: string | number };
export type Stage =
  | { kind: "line"; filter: LineFilter }
  | { kind: "parser"; parser: "json" | "logfmt" }
  | { kind: "label"; filter: LabelFilter }
  | { kind: "noop"; name: string };

export type LogExpr = { type: "log"; matchers: Matcher[]; stages: Stage[] };
export type RangeExpr = { type: "range"; fn: string; log: LogExpr; rangeMs: number };
export type AggExpr = { type: "agg"; fn: string; by: string[]; without: boolean; param?: number; expr: MetricExpr };
export type CmpExpr = { type: "binop"; op: string; expr: MetricExpr; scalar: number };
export type MetricExpr = RangeExpr | AggExpr | CmpExpr;
export type Query = LogExpr | MetricExpr;

export class LogQLError extends Error {
  constructor(
    message: string,
    public pos: number,
  ) {
    super(message);
  }
}

export function parseDuration(s: string): number | null {
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h|d|w|y)/g;
  let total = 0;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s))) {
    const n = Number(m[1]);
    const unit = { ms: 1, s: 1e3, m: 6e4, h: 3.6e6, d: 8.64e7, w: 6.048e8, y: 3.1536e10 }[m[2] as "ms"];
    total += n * unit;
    consumed += m[0].length;
  }
  return consumed === s.length && total > 0 ? total : null;
}

function unquote(t: Token): string {
  if (t.text.startsWith("`")) return t.text.slice(1, t.text.endsWith("`") && t.text.length > 1 ? -1 : undefined);
  const inner = t.text.slice(1, t.unterminated ? undefined : -1);
  return inner.replace(/\\(.)/g, "$1");
}

export function parseLogQL(input: string): Query {
  const toks = tokenize(input).filter((t) => t.kind !== "space");
  let i = 0;
  const peek = () => toks[i];
  const end = input.length;
  const fail = (msg: string, t?: Token): never => {
    throw new LogQLError(msg, t ? t.start : end);
  };
  const expect = (pred: (t: Token) => boolean, what: string): Token => {
    const t = toks[i];
    if (!t || !pred(t)) fail(`expected ${what}${t ? `, found "${t.text}"` : " at end of query"}`, t);
    i++;
    return t!;
  };
  const isText = (s: string) => (t: Token) => t.text === s;

  function parseSelector(): Matcher[] {
    expect(isText("{"), "{");
    const ms: Matcher[] = [];
    while (peek() && peek().text !== "}") {
      const l = expect((t) => t.kind === "label" || t.kind === "text", "a label name");
      const op = expect((t) => ["=", "!=", "=~", "!~"].includes(t.text), "=, !=, =~ or !~");
      const v = expect((t) => t.kind === "string", "a quoted value");
      if (v.unterminated) fail("unterminated string", v);
      ms.push({ label: l.text, op: op.text as Matcher["op"], value: unquote(v) });
      if (peek()?.text === ",") i++;
      else break;
    }
    expect(isText("}"), "}");
    if (ms.length === 0) fail("a stream selector needs at least one matcher", toks[i - 1]);
    if (!ms.some((m) => (m.op === "=" && m.value !== "") || (m.op === "=~" && !new RegExp(`^(?:${m.value})$`).test("")))) {
      fail("queries require at least one matcher that does not match empty values", toks[i - 1]);
    }
    return ms;
  }

  function parseStages(): Stage[] {
    const st: Stage[] = [];
    for (;;) {
      const t = peek();
      if (!t) break;
      if (t.kind === "pipe" && t.text !== "|") {
        i++;
        const v = expect((x) => x.kind === "string", "a quoted filter string");
        if (v.unterminated) fail("unterminated string", v);
        const value = unquote(v);
        if (t.text === "|~" || t.text === "!~") {
          try {
            new RegExp(value);
          } catch {
            fail(`invalid regex ${JSON.stringify(value)}`, v);
          }
        }
        st.push({ kind: "line", filter: { op: t.text as LineFilter["op"], value } });
        continue;
      }
      if (t.text === "|") {
        i++;
        const n = expect((x) => x.kind === "stage" || x.kind === "label" || x.kind === "text", "a parser or label filter");
        if (n.text === "json" || n.text === "logfmt") {
          st.push({ kind: "parser", parser: n.text });
          continue;
        }
        if (n.kind === "stage") {
          // line_format "…" / label_format a=b / drop a,b / keep a / decolorize / regexp "…" / pattern "…"
          while (peek() && peek().text !== "|" && peek().kind !== "bracket" && peek().text !== ")" && !(peek().kind === "pipe")) i++;
          st.push({ kind: "noop", name: n.text });
          continue;
        }
        const op = expect((x) => ["=", "!=", "=~", "!~", ">", ">=", "<", "<=", "=="].includes(x.text), "a comparison operator");
        const v = expect((x) => x.kind === "string" || x.kind === "number" || x.kind === "duration", "a value");
        const value = v.kind === "string" ? unquote(v) : v.kind === "duration" ? (parseDuration(v.text) ?? 0) : Number(v.text);
        st.push({ kind: "label", filter: { label: n.text, op: op.text as LabelFilter["op"], value } });
        continue;
      }
      break;
    }
    return st;
  }

  function parseLog(): LogExpr {
    return { type: "log", matchers: parseSelector(), stages: parseStages() };
  }

  function parseMetric(): MetricExpr {
    const f = expect((t) => t.kind === "function", "a function");
    let expr: MetricExpr;
    if (RANGE_FUNCTIONS.has(f.text)) {
      expect(isText("("), "(");
      const log = parseLog();
      expect(isText("["), "[ range ]");
      const d = expect((t) => t.kind === "duration" || t.kind === "number", "a duration like 5m");
      const rangeMs = parseDuration(d.text);
      if (!rangeMs) fail(`invalid duration "${d.text}"`, d);
      expect(isText("]"), "]");
      expect(isText(")"), ")");
      if (!["count_over_time", "rate", "bytes_over_time", "bytes_rate", "absent_over_time"].includes(f.text)) {
        fail(`${f.text} needs an unwrapped label; the demo store supports count_over_time, rate, bytes_over_time, bytes_rate`, f);
      }
      expr = { type: "range", fn: f.text, log, rangeMs: rangeMs! };
    } else if (AGG_FUNCTIONS.has(f.text)) {
      let by: string[] = [];
      let without = false;
      const grouping = () => {
        const k = peek();
        if (k && k.kind === "keyword" && (k.text === "by" || k.text === "without")) {
          i++;
          without = k.text === "without";
          expect(isText("("), "(");
          by = [];
          while (peek() && peek().text !== ")") {
            by.push(expect((t) => t.kind === "label" || t.kind === "text", "a label name").text);
            if (peek()?.text === ",") i++;
          }
          expect(isText(")"), ")");
        }
      };
      grouping();
      expect(isText("("), "(");
      let param: number | undefined;
      if (f.text === "topk" || f.text === "bottomk") {
        param = Number(expect((t) => t.kind === "number", "k").text);
        expect(isText(","), ",");
      }
      const inner = parseMetric();
      expect(isText(")"), ")");
      grouping();
      expr = { type: "agg", fn: f.text, by, without, param, expr: inner };
    } else {
      return fail(`unknown function ${f.text}`, f);
    }
    const t = peek();
    if (t && t.kind === "operator" && [">", ">=", "<", "<=", "==", "!=", "*", "/", "+", "-"].includes(t.text)) {
      i++;
      const n = expect((x) => x.kind === "number", "a number");
      expr = { type: "binop", op: t.text, expr, scalar: Number(n.text) };
    }
    return expr;
  }

  if (toks.length === 0) fail("empty query");
  const q: Query = toks[0].kind === "function" ? parseMetric() : parseLog();
  if (i < toks.length) fail(`unexpected "${toks[i].text}"`, toks[i]);
  return q;
}

// ─── evaluation helpers (shared by the demo store) ─────────────────────

const reCache = new Map<string, RegExp>();
function re(src: string, anchored: boolean): RegExp {
  const key = `${anchored ? "^" : ""}${src}`;
  let r = reCache.get(key);
  if (!r) {
    r = new RegExp(anchored ? `^(?:${src})$` : src);
    if (reCache.size > 500) reCache.clear();
    reCache.set(key, r);
  }
  return r;
}

export function matchLabels(ms: Matcher[], labels: Record<string, string>): boolean {
  for (const m of ms) {
    const v = labels[m.label] ?? "";
    switch (m.op) {
      case "=":
        if (v !== m.value) return false;
        break;
      case "!=":
        if (v === m.value) return false;
        break;
      case "=~":
        if (!re(m.value, true).test(v)) return false;
        break;
      case "!~":
        if (re(m.value, true).test(v)) return false;
        break;
    }
  }
  return true;
}

export function matchLine(f: LineFilter, line: string): boolean {
  switch (f.op) {
    case "|=":
      return line.includes(f.value);
    case "!=":
      return !line.includes(f.value);
    case "|~":
      return re(f.value, false).test(line);
    case "!~":
      return !re(f.value, false).test(line);
  }
}

/** Extract json / logfmt fields (flat, string values) from a line. */
export function extractFields(parser: "json" | "logfmt", line: string): Record<string, string> {
  const out: Record<string, string> = {};
  if (parser === "json") {
    try {
      const v = JSON.parse(line) as unknown;
      if (v && typeof v === "object") {
        for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
          if (val === null || typeof val === "object") continue;
          out[k.replace(/[^A-Za-z0-9_]/g, "_")] = String(val);
        }
      }
    } catch {
      out.__error__ = "JSONParserErr";
    }
    return out;
  }
  const re2 = /([A-Za-z_][A-Za-z0-9_.]*)=("(?:[^"\\]|\\.)*"|\S*)/g;
  let m: RegExpExecArray | null;
  while ((m = re2.exec(line))) out[m[1].replace(/\./g, "_")] = m[2].startsWith('"') ? m[2].slice(1, -1) : m[2];
  return out;
}

export function matchLabelFilter(f: LabelFilter, labels: Record<string, string>): boolean {
  const raw = labels[f.label];
  if (typeof f.value === "number") {
    if (raw === undefined) return false;
    const n = parseDuration(raw) ?? Number(raw);
    if (!Number.isFinite(n)) return false;
    switch (f.op) {
      case ">":
        return n > f.value;
      case ">=":
        return n >= f.value;
      case "<":
        return n < f.value;
      case "<=":
        return n <= f.value;
      case "==":
      case "=":
        return n === f.value;
      case "!=":
        return n !== f.value;
      default:
        return false;
    }
  }
  const v = raw ?? "";
  switch (f.op) {
    case "=":
    case "==":
      return v === f.value;
    case "!=":
      return v !== f.value;
    case "=~":
      return re(f.value, true).test(v);
    case "!~":
      return !re(f.value, true).test(v);
    default:
      return false;
  }
}

/** Run a log pipeline against one line; returns the (possibly extended) labels or null. */
export function runPipeline(log: LogExpr, labels: Record<string, string>, body: string): Record<string, string> | null {
  if (!matchLabels(log.matchers, labels)) return null;
  let ls = labels;
  for (const s of log.stages) {
    if (s.kind === "line") {
      if (!matchLine(s.filter, body)) return null;
    } else if (s.kind === "parser") {
      ls = { ...ls, ...extractFields(s.parser, body) };
    } else if (s.kind === "label") {
      if (!matchLabelFilter(s.filter, ls)) return null;
    }
  }
  return ls;
}

export function logExprOf(q: Query): LogExpr {
  if (q.type === "log") return q;
  if (q.type === "range") return q.log;
  return logExprOf(q.expr);
}
