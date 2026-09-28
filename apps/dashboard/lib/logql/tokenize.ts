// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// LogQL tokenizer for the query editor: syntax highlighting, label
// autocomplete context and "is this a metric query?" — NOT a validator
// (the control plane's parser owns the grammar and its error messages).
//
// It is total: any input, including half-typed queries with unterminated
// strings, tokenizes without throwing, and concatenating the token texts
// reproduces the input exactly (the editor overlays them on a textarea).

export type TokenKind =
  | "brace" //       { }
  | "paren" //       ( )
  | "bracket" //     [ ]
  | "comma"
  | "label" //       label name (selector, by/without list, label filter)
  | "matcher" //     = != =~ !~ (in a selector or label filter)
  | "pipe" //        |= |~ != !~ | (line filters / stages)
  | "string" //      "…" or `…`
  | "number"
  | "duration" //    5m, 1h30m, 250ms (inside [ ] or after offset)
  | "function" //    count_over_time, rate, sum …
  | "keyword" //     by, without, on, ignoring, offset, and, or, unless, bool
  | "stage" //       json, logfmt, regexp, pattern, line_format, label_format, drop, keep, unwrap, decolorize
  | "operator" //    > >= < <= == + - * / % ^
  | "space"
  | "text"; //       anything else (identifiers mid-typing, stray chars)

export type Token = { kind: TokenKind; text: string; start: number; end: number; unterminated?: boolean };

export const RANGE_FUNCTIONS = new Set([
  "count_over_time", "rate", "bytes_over_time", "bytes_rate", "absent_over_time", "sum_over_time",
  "avg_over_time", "max_over_time", "min_over_time", "first_over_time", "last_over_time",
  "stdvar_over_time", "stddev_over_time", "quantile_over_time", "rate_counter",
]);
export const AGG_FUNCTIONS = new Set(["sum", "avg", "min", "max", "count", "stddev", "stdvar", "topk", "bottomk", "sort", "sort_desc"]);
const KEYWORDS = new Set(["by", "without", "on", "ignoring", "group_left", "group_right", "offset", "and", "or", "unless", "bool"]);
const STAGES = new Set(["json", "logfmt", "regexp", "pattern", "line_format", "label_format", "drop", "keep", "unwrap", "decolorize", "unpack", "ip"]);

const isIdentStart = (c: string) => /[A-Za-z_]/.test(c);
const isIdent = (c: string) => /[A-Za-z0-9_.]/.test(c);

export function tokenize(q: string): Token[] {
  const out: Token[] = [];
  let i = 0;
  // Context: are we inside { … } (a stream selector)? After a `|` stage
  // keyword like `json`, identifiers are label names in label filters.
  let braceDepth = 0;
  let bracketDepth = 0;
  let parenListFor: "by" | null = null;
  let parenDepth = 0;
  let byParenDepth = -1;

  const push = (kind: TokenKind, start: number, end: number, extra: Partial<Token> = {}) => {
    out.push({ kind, text: q.slice(start, end), start, end, ...extra });
  };

  while (i < q.length) {
    const c = q[i];
    const start = i;

    if (/\s/.test(c)) {
      while (i < q.length && /\s/.test(q[i])) i++;
      push("space", start, i);
      continue;
    }

    if (c === '"' || c === "`") {
      const quote = c;
      i++;
      let closed = false;
      while (i < q.length) {
        if (quote === '"' && q[i] === "\\" && i + 1 < q.length) {
          i += 2;
          continue;
        }
        if (q[i] === quote) {
          i++;
          closed = true;
          break;
        }
        i++;
      }
      push("string", start, i, closed ? {} : { unterminated: true });
      continue;
    }

    if (c === "{" || c === "}") {
      braceDepth += c === "{" ? 1 : -1;
      braceDepth = Math.max(0, braceDepth);
      push("brace", start, ++i);
      continue;
    }
    if (c === "(" || c === ")") {
      if (c === "(") {
        parenDepth++;
        if (parenListFor === "by") {
          byParenDepth = parenDepth;
          parenListFor = null;
        }
      } else {
        if (parenDepth === byParenDepth) byParenDepth = -1;
        parenDepth = Math.max(0, parenDepth - 1);
      }
      push("paren", start, ++i);
      continue;
    }
    if (c === "[" || c === "]") {
      bracketDepth += c === "[" ? 1 : -1;
      bracketDepth = Math.max(0, bracketDepth);
      push("bracket", start, ++i);
      continue;
    }
    if (c === ",") {
      push("comma", start, ++i);
      continue;
    }

    // Two-char operators first.
    const two = q.slice(i, i + 2);
    if (two === "|=" || two === "|~") {
      i += 2;
      push("pipe", start, i);
      continue;
    }
    if (two === "!=" || two === "!~" || two === "=~") {
      i += 2;
      // Outside a selector and not after a label, != / !~ are line filters.
      const prev = lastNonSpace(out);
      const isMatcher = braceDepth > 0 || prev?.kind === "label";
      push(isMatcher ? "matcher" : two === "=~" ? "matcher" : "pipe", start, i);
      continue;
    }
    if (two === "==" || two === ">=" || two === "<=") {
      i += 2;
      push(two === "==" && braceDepth > 0 ? "matcher" : "operator", start, i);
      continue;
    }
    if (c === "|") {
      push("pipe", start, ++i);
      continue;
    }
    if (c === "=") {
      push("matcher", start, ++i);
      continue;
    }
    if (c === ">" || c === "<" || c === "+" || c === "*" || c === "/" || c === "%" || c === "^" || c === "-") {
      // A leading "-" of a number stays with the number.
      if (c === "-" && /[0-9]/.test(q[i + 1] ?? "") && !isValueLike(lastNonSpace(out))) {
        // fallthrough to number
      } else {
        push("operator", start, ++i);
        continue;
      }
    }

    // Numbers and durations (5m, 1h30m, 250ms, 1.5).
    if (/[0-9-]/.test(c)) {
      i++;
      while (i < q.length && /[0-9.]/.test(q[i])) i++;
      if (i < q.length && /[smhdwy]/.test(q[i])) {
        while (i < q.length && /[0-9smhdwy]/.test(q[i])) i++;
        push("duration", start, i);
      } else {
        push(bracketDepth > 0 ? "duration" : "number", start, i);
      }
      continue;
    }

    if (isIdentStart(c)) {
      while (i < q.length && isIdent(q[i])) i++;
      const word = q.slice(start, i);
      const prev = lastNonSpace(out);
      if (braceDepth > 0) {
        push("label", start, i);
      } else if (byParenDepth > 0 && parenDepth >= byParenDepth) {
        push("label", start, i);
      } else if (KEYWORDS.has(word)) {
        if (word === "by" || word === "without") parenListFor = "by";
        push("keyword", start, i);
      } else if (prev?.kind === "pipe" && prev.text === "|" && STAGES.has(word)) {
        push("stage", start, i);
      } else if ((RANGE_FUNCTIONS.has(word) || AGG_FUNCTIONS.has(word)) && /^\s*\(/.test(q.slice(i)) ) {
        push("function", start, i);
      } else if ((RANGE_FUNCTIONS.has(word) || AGG_FUNCTIONS.has(word)) && /^\s*(by|without)\b/.test(q.slice(i))) {
        push("function", start, i);
      } else if (prev?.kind === "pipe" || prev?.kind === "keyword" || /^\s*(=~|!~|!=|==|>=|<=|=|>|<)/.test(q.slice(i))) {
        // Label filter after a parser stage: `| status >= 500`, `| level="error"`.
        push("label", start, i);
      } else {
        push("text", start, i);
      }
      continue;
    }

    push("text", start, ++i);
  }
  return out;
}

function lastNonSpace(ts: Token[]): Token | undefined {
  for (let k = ts.length - 1; k >= 0; k--) if (ts[k].kind !== "space") return ts[k];
  return undefined;
}

function isValueLike(t: Token | undefined): boolean {
  return !!t && (t.kind === "number" || t.kind === "duration" || t.kind === "string" || t.kind === "label" || t.kind === "text" || (t.kind === "paren" && t.text === ")") || (t.kind === "brace" && t.text === "}"));
}

/** A metric query starts with a function (count_over_time(…), sum by (…) (…)). */
export function isMetricQuery(q: string): boolean {
  const first = tokenize(q).find((t) => t.kind !== "space");
  return !!first && first.kind === "function";
}

/**
 * The log query inside a metric query — what GetLogVolume/GetLogPatterns
 * accept — e.g. `sum by (ns) (count_over_time({app="x"} |= "err" [5m]))`
 * → `{app="x"} |= "err"`. Returns the input for log queries.
 */
export function innerLogQuery(q: string): string {
  if (!isMetricQuery(q)) return q.trim();
  const toks = tokenize(q);
  const open = toks.findIndex((t) => t.kind === "brace" && t.text === "{");
  if (open < 0) return q.trim();
  // Up to the range vector `[` (or the closing paren of the range function).
  let depth = 0;
  let end = q.length;
  for (let k = open; k < toks.length; k++) {
    const t = toks[k];
    if (t.kind === "bracket" && t.text === "[") {
      end = t.start;
      break;
    }
    if (t.kind === "paren") {
      if (t.text === "(") depth++;
      else if (depth === 0) {
        end = t.start;
        break;
      } else depth--;
    }
  }
  return q.slice(toks[open].start, end).trim();
}

export type CompletionContext =
  | { kind: "label-name"; where: "selector" | "by"; prefix: string; from: number; to: number }
  | { kind: "label-value"; label: string; prefix: string; from: number; to: number };

/**
 * What to autocomplete at `cursor`: a label name inside `{…}` (or a
 * by/without list), or a label value inside the quotes after `label=`.
 */
export function completionContext(q: string, cursor: number): CompletionContext | null {
  const toks = tokenize(q);
  // Label value: cursor inside (or at the end of an unterminated) string
  // that follows `label <matcher>`.
  for (let k = 0; k < toks.length; k++) {
    const t = toks[k];
    if (t.kind !== "string") continue;
    const inside = cursor > t.start && (cursor < t.end || (t.unterminated === true && cursor === t.end));
    if (!inside) continue;
    const m = prevNonSpace(toks, k);
    const l = m !== undefined ? prevNonSpace(toks, m) : undefined;
    if (m !== undefined && l !== undefined && toks[m].kind === "matcher" && toks[l].kind === "label") {
      return {
        kind: "label-value",
        label: toks[l].text,
        prefix: q.slice(t.start + 1, cursor),
        from: t.start + 1,
        to: t.unterminated ? t.end : t.end - 1,
      };
    }
    return null;
  }

  // Label name: the token ending at the cursor is a label (or the cursor
  // sits right after `{` / `,` / `(` of a by-list).
  const where = insideSelectorOrBy(toks, cursor);
  const at = toks.find((t) => t.start < cursor && cursor <= t.end);
  if (at && at.kind === "label" && where) {
    return { kind: "label-name", where, prefix: q.slice(at.start, cursor), from: at.start, to: at.end };
  }
  const before = [...toks].reverse().find((t) => t.end <= cursor && t.kind !== "space");
  const opener = before && ((before.kind === "brace" && before.text === "{") || before.kind === "comma" || (before.kind === "paren" && before.text === "("));
  if (opener && where) return { kind: "label-name", where, prefix: "", from: cursor, to: cursor };
  return null;
}

function prevNonSpace(ts: Token[], k: number): number | undefined {
  for (let j = k - 1; j >= 0; j--) if (ts[j].kind !== "space") return j;
  return undefined;
}

function insideSelectorOrBy(toks: Token[], cursor: number): "selector" | "by" | null {
  let brace = 0;
  let byOpen = false;
  let paren = 0;
  let byParen = -1;
  let expectBy = false;
  for (const t of toks) {
    if (t.start >= cursor) break;
    if (t.kind === "brace") brace += t.text === "{" ? 1 : -1;
    if (t.kind === "keyword" && (t.text === "by" || t.text === "without")) expectBy = true;
    if (t.kind === "paren") {
      if (t.text === "(") {
        paren++;
        if (expectBy) {
          byParen = paren;
          expectBy = false;
          byOpen = true;
        }
      } else {
        if (paren === byParen) {
          byOpen = false;
          byParen = -1;
        }
        paren--;
      }
    }
  }
  return brace > 0 ? "selector" : byOpen ? "by" : null;
}

/**
 * Apply a completion. Values are escaped and closed with a quote; a label
 * name in a selector becomes `name=""` with the cursor between the quotes
 * (unless a matcher already follows); in a by(...) list it is just the name.
 */
export function applyCompletion(q: string, ctx: CompletionContext, value: string): { query: string; cursor: number } {
  const head = q.slice(0, ctx.from);
  const tail = q.slice(ctx.to);
  if (ctx.kind === "label-value") {
    const v = value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
    const close = tail.startsWith('"') ? "" : '"';
    return { query: head + v + close + tail, cursor: head.length + v.length + 1 };
  }
  if (ctx.where === "by" || /^\s*(=~|!~|!=|=)/.test(tail)) {
    return { query: head + value + tail, cursor: head.length + value.length };
  }
  return { query: `${head}${value}=""${tail}`, cursor: head.length + value.length + 2 };
}

/**
 * Add (or replace) one equality matcher in the stream selector:
 * `{app="x"} |= "y"` + (pod, "p-1") → `{app="x", pod="p-1"} |= "y"`.
 * Used by click-to-filter on labels. Falls back to prefixing a selector.
 */
export function withMatcher(q: string, label: string, value: string, op: "=" | "!=" = "="): string {
  const lit = `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
  const toks = tokenize(q);
  const open = toks.findIndex((t) => t.kind === "brace" && t.text === "{");
  const close = open >= 0 ? toks.findIndex((t, k) => k > open && t.kind === "brace" && t.text === "}") : -1;
  if (open < 0 || close < 0) return `{${label}${op}${lit}} ${q.trim()}`.trim();
  // Replace an existing matcher for the same label.
  for (let k = open + 1; k < close; k++) {
    if (toks[k].kind === "label" && toks[k].text === label) {
      const m = toks.findIndex((t, j) => j > k && t.kind === "matcher");
      const s = toks.findIndex((t, j) => j > m && t.kind === "string");
      if (m > 0 && s > 0 && s < close) {
        return q.slice(0, toks[k].start) + `${label}${op}${lit}` + q.slice(toks[s].end);
      }
    }
  }
  const inner = q.slice(toks[open].end, toks[close].start).trim();
  const sep = inner.length > 0 ? ", " : "";
  return q.slice(0, toks[open].end) + inner + sep + `${label}${op}${lit}` + q.slice(toks[close].start);
}

/** Regex that matches lines of a Drain pattern: literal parts escaped, <_> → .*? */
export function patternToRegex(pattern: string): string {
  return pattern
    .split("<_>")
    .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
    .join(".*?")
    .replace(/(\.\*\?)+/g, ".*?");
}
