// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Flamegraph math, independent of rendering. The wire format
// (ProfilesService.GetFlamegraph) is a pre-order flattened call tree:
// nodes[0] is the synthetic root, every node names its parent by index.
//
//   layout     x-extent of every node in ROOT units ([0, 1]), computed
//              once; zooming is then just a viewport [a, b] over it —
//              which is what makes zoom animation cheap (one lerp).
//   search     matched nodes + the share of total time they cover,
//              counting nested matches once (a recursive fn doesn't
//              double its own share).
//   diff       per-node delta vs the baseline (both normalised to their
//              root), mapped to regression red / improvement green.
//   folded     build the flattened tree from "a;b;c 42" stacks (demo
//              fixtures + tests).
//
// Pure, DOM-free, unit-tested.

export type FlameNode = {
  name: string;
  parent: number; // -1 for the root
  depth: number;
  self: number;
  total: number;
  baselineSelf: number;
  baselineTotal: number;
};

export type Tree = {
  nodes: FlameNode[];
  children: number[][];
  /** Root-relative extent of each node. */
  x0: Float64Array;
  x1: Float64Array;
  maxDepth: number;
  /** Node indices per depth, sorted by x0 — for O(log n) hit-testing. */
  byDepth: number[][];
};

export function buildTree(nodes: FlameNode[]): Tree {
  const n = nodes.length;
  const children: number[][] = Array.from({ length: n }, () => []);
  let maxDepth = 0;
  for (let i = 1; i < n; i++) {
    const p = nodes[i].parent;
    if (p >= 0 && p < n && p !== i) children[p].push(i);
    if (nodes[i].depth > maxDepth) maxDepth = nodes[i].depth;
  }
  const x0 = new Float64Array(n);
  const x1 = new Float64Array(n);
  const rootTotal = n > 0 ? Math.max(1, nodes[0].total) : 1;
  if (n > 0) {
    x0[0] = 0;
    x1[0] = 1;
  }
  // Pre-order: parents are laid out before children.
  for (let i = 0; i < n; i++) {
    let cursor = x0[i];
    for (const c of children[i]) {
      x0[c] = cursor;
      cursor += nodes[c].total / rootTotal;
      x1[c] = cursor;
    }
  }
  const byDepth: number[][] = Array.from({ length: maxDepth + 1 }, () => []);
  for (let i = 0; i < n; i++) byDepth[Math.min(maxDepth, nodes[i].depth)]?.push(i);
  for (const row of byDepth) row.sort((a, b) => x0[a] - x0[b]);
  return { nodes, children, x0, x1, maxDepth, byDepth };
}

/** Path root → node (inclusive). */
export function ancestry(t: Tree, i: number): number[] {
  const out: number[] = [];
  let cur = i;
  let guard = 0;
  while (cur >= 0 && guard++ < 4096) {
    out.push(cur);
    cur = t.nodes[cur].parent;
  }
  return out.reverse();
}

/** Node under root-relative x at `depth`, or -1. */
export function hitTest(t: Tree, depth: number, x: number): number {
  const row = t.byDepth[depth];
  if (!row || row.length === 0) return -1;
  let lo = 0;
  let hi = row.length - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const i = row[mid];
    if (x < t.x0[i]) hi = mid - 1;
    else if (x >= t.x1[i]) lo = mid + 1;
    else return i;
  }
  return -1;
}

export type Rect = { i: number; x: number; w: number; depth: number };

/**
 * Visible rectangles in pixels for viewport [a, b] (root units), culling
 * nodes outside the viewport or narrower than `minPx`.
 */
export function visibleRects(t: Tree, a: number, b: number, width: number, minPx = 0.5): Rect[] {
  const span = Math.max(1e-12, b - a);
  const scale = width / span;
  const out: Rect[] = [];
  for (let d = 0; d < t.byDepth.length; d++) {
    for (const i of t.byDepth[d]) {
      const l = t.x0[i];
      const r = t.x1[i];
      if (r <= a || l >= b) continue;
      const x = Math.max(0, (l - a) * scale);
      const w = Math.min(width, (r - a) * scale) - x;
      if (w < minPx) continue;
      out.push({ i, x, w, depth: d });
    }
  }
  return out;
}

// ─── search ──────────────────────────────────────────────────────────────

export function compileSearch(q: string): ((name: string) => boolean) | null {
  const s = q.trim();
  if (!s) return null;
  const m = /^\/(.+)\/([i]*)$/.exec(s);
  if (m) {
    try {
      const re = new RegExp(m[1], m[2] || "i");
      return (name) => re.test(name);
    } catch {
      return null;
    }
  }
  const needle = s.toLowerCase();
  return (name) => name.toLowerCase().includes(needle);
}

/** Matched node set and the fraction of root time they cover (nested matches counted once). */
export function searchTree(t: Tree, match: (name: string) => boolean): { matched: Set<number>; fraction: number } {
  const matched = new Set<number>();
  let covered = 0;
  const rootTotal = t.nodes.length > 0 ? Math.max(1, t.nodes[0].total) : 1;
  // DFS carrying "an ancestor already matched".
  const stack: [number, boolean][] = t.nodes.length > 0 ? [[0, false]] : [];
  while (stack.length > 0) {
    const [i, under] = stack.pop()!;
    const hit = i !== 0 && match(t.nodes[i].name);
    if (hit) {
      matched.add(i);
      if (!under) covered += t.nodes[i].total;
    }
    for (const c of t.children[i]) stack.push([c, under || hit]);
  }
  return { matched, fraction: covered / rootTotal };
}

// ─── diff ────────────────────────────────────────────────────────────────

export type DiffInfo = { delta: number; tone: "regression" | "improvement" | "neutral"; intensity: number };

/**
 * Share-of-root delta between the current and baseline profile. Both are
 * normalised to their own root, so a busier window doesn't paint every
 * frame red. `scale` is the delta that saturates the colour (default 5
 * percentage points of total).
 */
export function diffOf(n: FlameNode, rootTotal: number, baselineRootTotal: number, scale = 0.05): DiffInfo {
  const cur = rootTotal > 0 ? n.total / rootTotal : 0;
  const base = baselineRootTotal > 0 ? n.baselineTotal / baselineRootTotal : 0;
  const delta = cur - base;
  const intensity = Math.min(1, Math.abs(delta) / scale);
  if (Math.abs(delta) < 0.001) return { delta, tone: "neutral", intensity: 0 };
  return { delta, tone: delta > 0 ? "regression" : "improvement", intensity };
}

/** Relative change of a frame vs baseline (+38% = regression). Infinity when new. */
export function relativeChange(n: FlameNode, rootTotal: number, baselineRootTotal: number): number {
  const cur = rootTotal > 0 ? n.total / rootTotal : 0;
  const base = baselineRootTotal > 0 ? n.baselineTotal / baselineRootTotal : 0;
  if (base === 0) return cur > 0 ? Infinity : 0;
  return cur / base - 1;
}

// ─── cost ────────────────────────────────────────────────────────────────

/** What a frame's CPU costs per month: workload CPU $ × frame share of root. */
export function frameCost(value: number, rootTotal: number, costUsdMonth: number): number {
  return rootTotal > 0 ? (costUsdMonth * value) / rootTotal : 0;
}

// ─── colours ─────────────────────────────────────────────────────────────

const PKG_HUES = [212, 28, 160, 42, 330, 120, 250, 0];

/** Package part of a frame: "encoding/json.Marshal" → "encoding/json". */
export function packageOf(name: string): string {
  const paren = name.indexOf("(");
  const base = paren > 0 ? name.slice(0, paren) : name;
  const lastSlash = base.lastIndexOf("/");
  const dot = base.indexOf(".", lastSlash + 1);
  if (dot > 0) return base.slice(0, dot);
  const colons = base.indexOf("::");
  if (colons > 0) return base.slice(0, colons);
  return base.split(/[ .]/)[0] ?? base;
}

function hashStr(s: string): number {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

/** Muted, stable colour per package (identity only — the numbers are in the tooltip). */
export function frameColor(name: string, dimmed = false): string {
  const h = PKG_HUES[hashStr(packageOf(name)) % PKG_HUES.length];
  const l = dimmed ? 18 : 34 + (hashStr(name) % 9);
  const s = dimmed ? 8 : 38;
  return `hsl(${h} ${s}% ${l}%)`;
}

export function diffColor(d: DiffInfo): string {
  if (d.tone === "neutral") return "hsl(40 4% 26%)";
  const l = 24 + d.intensity * 22;
  const s = 30 + d.intensity * 50;
  return d.tone === "regression" ? `hsl(2 ${s}% ${l}%)` : `hsl(146 ${s}% ${l - 4}%)`;
}

// ─── folded stacks → flattened tree ─────────────────────────────────────

export type Folded = { frames: string[]; value: number; baseline?: number };

/** "a;b;c 42" lines → Folded (malformed lines skipped). */
export function parseFolded(text: string): Folded[] {
  const out: Folded[] = [];
  for (const line of text.split("\n")) {
    const m = /^(.+?)\s+(\d+)(?:\s+(\d+))?\s*$/.exec(line.trim());
    if (!m) continue;
    out.push({ frames: m[1].split(";"), value: Number(m[2]), baseline: m[3] !== undefined ? Number(m[3]) : undefined });
  }
  return out;
}

/** Merge stacks into a pre-order flattened tree (root "total"); children sorted by total desc. */
export function flattenFolded(stacks: Folded[]): FlameNode[] {
  type N = { name: string; self: number; total: number; bSelf: number; bTotal: number; kids: Map<string, N> };
  const mk = (name: string): N => ({ name, self: 0, total: 0, bSelf: 0, bTotal: 0, kids: new Map() });
  const root = mk("total");
  for (const s of stacks) {
    const v = Math.max(0, s.value);
    const b = Math.max(0, s.baseline ?? 0);
    root.total += v;
    root.bTotal += b;
    let cur = root;
    for (const f of s.frames) {
      let next = cur.kids.get(f);
      if (!next) {
        next = mk(f);
        cur.kids.set(f, next);
      }
      next.total += v;
      next.bTotal += b;
      cur = next;
    }
    cur.self += v;
    cur.bSelf += b;
  }
  const out: FlameNode[] = [];
  const walk = (n: N, parent: number, depth: number) => {
    const idx = out.length;
    out.push({ name: n.name, parent, depth, self: n.self, total: n.total, baselineSelf: n.bSelf, baselineTotal: n.bTotal });
    const kids = [...n.kids.values()].sort((a, b) => b.total - a.total || a.name.localeCompare(b.name));
    for (const k of kids) walk(k, idx, depth + 1);
  };
  walk(root, -1, 0);
  return out;
}

export type TopFn = { name: string; self: number; total: number; selfPct: number; totalPct: number };

/** Self/total per function name; a function's total counts once per root path (recursion-safe). */
export function topFunctions(nodes: FlameNode[]): TopFn[] {
  const t = buildTree(nodes);
  const rootTotal = nodes.length > 0 ? Math.max(1, nodes[0].total) : 1;
  const self = new Map<string, number>();
  const total = new Map<string, number>();
  const stack: [number, Set<string>][] = nodes.length > 0 ? [[0, new Set()]] : [];
  while (stack.length > 0) {
    const [i, onPath] = stack.pop()!;
    const n = nodes[i];
    let path = onPath;
    if (i !== 0) {
      self.set(n.name, (self.get(n.name) ?? 0) + n.self);
      if (!onPath.has(n.name)) {
        total.set(n.name, (total.get(n.name) ?? 0) + n.total);
        path = new Set(onPath).add(n.name);
      }
    }
    for (const c of t.children[i]) stack.push([c, path]);
  }
  return [...total.keys()].map((name) => ({
    name,
    self: self.get(name) ?? 0,
    total: total.get(name) ?? 0,
    selfPct: ((self.get(name) ?? 0) / rootTotal) * 100,
    totalPct: ((total.get(name) ?? 0) / rootTotal) * 100,
  }));
}
