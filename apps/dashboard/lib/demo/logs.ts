// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo log store. Logs are a pure function of time: for every (stream,
// minute) the line count is a deterministic stochastic rounding of the
// stream's rate at that minute, so a 7-day volume histogram and a
// zoomed-in 5-minute line list agree with each other without storing a
// single line. Bodies are rendered on demand, only for the lines a view
// actually shows.
//
// The story (anchored to "now", stable for 5 minutes at a time):
//   · shop/payments: Stripe timeouts spiking since ~70 minutes ago and
//     still ongoing; checkout surfaces "payment authorization failed",
//     storefront warns;
//   · shop/cart: periodic "runtime: out of memory" fatals;
//   · everything else hums along at believable rates.

import { normalizeLevel, type LogLevel } from "@/lib/chart/palette";
import {
  logExprOf,
  matchLabels,
  parseLogQL,
  runPipeline,
  type LogExpr,
  type MetricExpr,
  type Query,
} from "@/lib/logql/parse";
import { hash32, hex, podSuffix, rngFor } from "./rng";
import { namedWorkloads, type DemoWorkload } from "./world";

const MIN = 60_000;

type Ctx = { r: () => number; minute: number };
type Template = {
  id: string;
  level: LogLevel;
  /** Drain-style pattern (what GetLogPatterns would return). */
  pattern: string;
  render: (c: Ctx) => string;
  /** lines / minute / pod at baseline */
  rate: number;
  stream?: "stdout" | "stderr";
  /** Multiplier as a function of minutes relative to the incident anchor. */
  shape?: (relMin: number, utcHour: number) => number;
};

const uuid = (r: () => number) => `${hex(r, 8)}-${hex(r, 4)}-4${hex(r, 3)}-a${hex(r, 3)}-${hex(r, 12)}`;
const ms = (r: () => number, lo: number, hi: number) => Math.round(lo + Math.pow(r(), 2) * (hi - lo));
const pick = <T,>(r: () => number, xs: T[]) => xs[Math.floor(r() * xs.length)];

// Incident window (relative minutes to the anchor): payments started
// failing ~70 minutes ago and still is — the alert on /alerts is firing
// and /ask has something live to investigate.
const INCIDENT = (rel: number) => (rel >= -70 ? 1 : 0);
const incident = (base: number, peak: number) => (rel: number) => (INCIDENT(rel) ? peak / base : 1);

const TEMPLATES: Record<string, Template[]> = {
  "shop/payments": [
    { id: "pay-charge", level: "info", rate: 14, pattern: "charge created amount=<_> currency=usd customer=<_> latency_ms=<_>", render: ({ r }) => `charge created amount=${ms(r, 500, 19999)} currency=usd customer=cus_${hex(r, 14)} latency_ms=${ms(r, 80, 900)}`, shape: incident(14, 5) },
    { id: "pay-stripe-timeout", level: "error", rate: 0.15, stream: "stderr", pattern: "stripe: request to POST /v1/payment_intents timed out after 5000ms idempotency_key=<_> attempt=<_>", render: ({ r }) => `stripe: request to POST /v1/payment_intents timed out after 5000ms idempotency_key=${hex(r, 24)} attempt=${1 + Math.floor(r() * 3)}`, shape: incident(0.15, 22) },
    { id: "pay-retry", level: "warn", rate: 0.3, pattern: "retrying stripe call attempt=<_> backoff_ms=<_>", render: ({ r }) => `retrying stripe call attempt=${2 + Math.floor(r() * 2)} backoff_ms=${pick(r, [200, 400, 800, 1600])}`, shape: incident(0.3, 14) },
    { id: "pay-webhook", level: "info", rate: 3, pattern: "webhook received type=<_> event=<_>", render: ({ r }) => `webhook received type=${pick(r, ["payment_intent.succeeded", "charge.refunded", "payment_intent.created"])} event=evt_${hex(r, 20)}` },
  ],
  "shop/checkout": [
    { id: "co-req", level: "info", rate: 40, pattern: "POST /api/checkout <_> in <_>ms order_id=<_>", render: ({ r, minute }) => `POST /api/checkout ${INCIDENT(minute) && r() < 0.3 ? 502 : 200} in ${ms(r, 40, INCIDENT(minute) ? 5600 : 480)}ms order_id=${uuid(r)}` },
    { id: "co-payfail", level: "error", rate: 0.1, stream: "stderr", pattern: "payment authorization failed: upstream payments returned 502 after <_>ms order_id=<_>", render: ({ r }) => `payment authorization failed: upstream payments returned 502 after ${ms(r, 5000, 5400)}ms order_id=${uuid(r)}`, shape: incident(0.1, 9) },
    { id: "co-slowq", level: "warn", rate: 0.8, pattern: "slow query to postgres took <_>ms query=<_>", render: ({ r }) => `slow query to postgres took ${ms(r, 500, 1900)}ms query=${pick(r, ["SELECT_cart_items", "UPDATE_inventory", "INSERT_order"])}` },
    { id: "co-cache", level: "debug", rate: 12, pattern: "cache hit key=<_> ttl=<_>s", render: ({ r }) => `cache hit key=cart:${hex(r, 12)} ttl=${ms(r, 10, 300)}s` },
  ],
  "shop/storefront": [
    { id: "sf-get", level: "info", rate: 70, pattern: "GET <_> <_> <_>ms", render: ({ r }) => `GET ${pick(r, ["/", "/products/" + ms(r, 1000, 9999), "/cart", "/search?q=" + pick(r, ["boots", "jacket", "lamp", "desk"])])} ${r() < 0.02 ? 404 : 200} ${ms(r, 8, 240)}ms` },
    { id: "sf-slow", level: "warn", rate: 0.4, pattern: "render took <_>ms route=/checkout upstream=checkout", render: ({ r }) => `render took ${ms(r, 1200, 6200)}ms route=/checkout upstream=checkout`, shape: incident(0.4, 6) },
    { id: "sf-ssr", level: "error", rate: 0.05, stream: "stderr", pattern: "TypeError: Cannot read properties of undefined (reading '<_>') at ProductCard (<_>)", render: ({ r }) => `TypeError: Cannot read properties of undefined (reading '${pick(r, ["price", "images", "variants"])}') at ProductCard (webpack://storefront/app/product-card.tsx:${ms(r, 20, 140)})` },
  ],
  "shop/cart": [
    { id: "cart-op", level: "info", rate: 25, pattern: "cart <_> items=<_> session=<_>", render: ({ r }) => `cart ${pick(r, ["add", "remove", "view", "merge"])} items=${ms(r, 1, 14)} session=${hex(r, 16)}` },
    { id: "cart-oom", level: "fatal", rate: 0.035, stream: "stderr", pattern: "fatal error: runtime: out of memory: cannot allocate <_>-byte block (<_> in use)", render: ({ r }) => `fatal error: runtime: out of memory: cannot allocate ${pick(r, [134217728, 268435456])}-byte block (${ms(r, 520000000, 536000000)} in use)`, shape: (rel) => (rel > -180 ? 2.5 : 1) },
    { id: "cart-gc", level: "warn", rate: 0.5, pattern: "GC pause <_>ms heap=<_>MiB (limit 512MiB)", render: ({ r }) => `GC pause ${ms(r, 12, 180)}ms heap=${ms(r, 420, 511)}MiB (limit 512MiB)` },
  ],
  "shop/catalog": [
    { id: "cat-img", level: "info", rate: 18, pattern: "fetched image s3://catalog-images-us-east-1/<_> bytes=<_> az=<_>", render: ({ r }) => `fetched image s3://catalog-images-us-east-1/${hex(r, 10)}.webp bytes=${ms(r, 20000, 900000)} az=${pick(r, ["us-east-1a", "us-east-1b"])}` },
    { id: "cat-q", level: "info", rate: 9, pattern: "GET /v1/products page=<_> took=<_>ms", render: ({ r }) => `GET /v1/products page=${ms(r, 1, 40)} took=${ms(r, 6, 90)}ms` },
  ],
  "shop/postgres": [
    { id: "pg-slow", level: "warn", rate: 0.5, pattern: "LOG: duration: <_> ms statement: <_>", render: ({ r }) => `LOG:  duration: ${ms(r, 510, 2400)}.${ms(r, 100, 999)} ms  statement: ${pick(r, ["SELECT * FROM cart_items WHERE session_id = $1", "UPDATE inventory SET qty = qty - $1 WHERE sku = $2"])}`, shape: incident(0.5, 3) },
    { id: "pg-ckpt", level: "info", rate: 0.2, pattern: "LOG: checkpoint complete: wrote <_> buffers (<_>%)", render: ({ r }) => `LOG:  checkpoint complete: wrote ${ms(r, 900, 9000)} buffers (${(r() * 9).toFixed(1)}%)` },
  ],
  "shop/redis": [{ id: "redis-save", level: "info", rate: 0.1, pattern: "Background saving terminated with success", render: () => "Background saving terminated with success" }],
  "retrieval/vectordb-ingress": [
    { id: "vdb-batch", level: "info", rate: 30, pattern: "indexed batch size=<_> took=<_>ms collection=<_>", render: ({ r }) => `indexed batch size=${pick(r, [256, 512, 1024])} took=${ms(r, 30, 220)}ms collection=${pick(r, ["docs_v3", "tickets", "kb_articles"])}` },
    { id: "vdb-merge", level: "debug", rate: 2, pattern: "segment merge finished segments=<_> duration=<_>ms", render: ({ r }) => `segment merge finished segments=${ms(r, 2, 9)} duration=${ms(r, 100, 3000)}ms` },
  ],
  "retrieval/retrieval-indexer": [{ id: "ri-doc", level: "info", rate: 11, pattern: "embedded <_> chunks from <_> in <_>ms", render: ({ r }) => `embedded ${ms(r, 4, 80)} chunks from doc_${hex(r, 10)} in ${ms(r, 60, 900)}ms` }],
  "data/queue-consumer": [{ id: "qc-idle", level: "info", rate: 2, pattern: "no messages on queue=<_>, sleeping 30s", render: ({ r }) => `no messages on queue=${pick(r, ["events-legacy", "exports-v1"])}, sleeping 30s` }],
  "ml-inference/model-server-a100": [
    { id: "ms-infer", level: "info", rate: 20, pattern: "inference model=<_> tokens=<_> latency_ms=<_> gpu=<_>", render: ({ r }) => `inference model=${pick(r, ["llama-70b-instruct", "reranker-v2"])} tokens=${ms(r, 32, 2048)} latency_ms=${ms(r, 120, 2600)} gpu=${Math.floor(r() * 2)}` },
    { id: "ms-frag", level: "warn", rate: 0.2, pattern: "GPU memory fragmentation <_>% on device <_>", render: ({ r }) => `GPU memory fragmentation ${ms(r, 20, 45)}% on device ${Math.floor(r() * 2)}` },
  ],
  "ml-inference/embedding-batcher": [{ id: "eb", level: "info", rate: 8, pattern: "flushed batch n=<_> p95_ms=<_>", render: ({ r }) => `flushed batch n=${ms(r, 16, 256)} p95_ms=${ms(r, 20, 140)}` }],
  "edge/frontend-gateway": [
    { id: "fg-access", level: "info", rate: 80, pattern: '{"level":"info","method":"<_>","path":"<_>","status":<_>,"duration_ms":<_>,"upstream":"<_>"}', render: ({ r }) => JSON.stringify({ level: "info", method: pick(r, ["GET", "GET", "POST"]), path: pick(r, ["/", "/api/products", "/api/cart", "/healthz"]), status: 200, duration_ms: ms(r, 2, 180), upstream: pick(r, ["storefront", "catalog"]) }) },
    { id: "fg-5xx", level: "error", rate: 0.3, pattern: '{"level":"error","method":"<_>","path":"<_>","status":<_>,"duration_ms":<_>,"upstream":"<_>"}', render: ({ r }) => JSON.stringify({ level: "error", method: "POST", path: "/api/checkout", status: pick(r, [502, 503, 504]), duration_ms: ms(r, 5000, 10000), upstream: "checkout" }), shape: incident(0.3, 4) },
  ],
  "edge/api-ingress": [{ id: "ai-access", level: "info", rate: 45, pattern: "<_> <_> <_> upstream=<_> rt=<_>", render: ({ r }) => `${pick(r, ["GET", "POST", "PUT"])} ${pick(r, ["/v2/orders", "/v2/users/me", "/v2/search"])} ${r() < 0.01 ? 500 : 200} upstream=${pick(r, ["orders", "users", "search"])} rt=${(r() * 0.4).toFixed(3)}` }],
  "data/etl-nightly": [
    { id: "etl-stage", level: "info", rate: 3, pattern: "stage <_>/7 complete rows=<_> elapsed=<_>s", render: ({ r }) => `stage ${1 + Math.floor(r() * 7)}/7 complete rows=${(r() * 3).toFixed(1)}M elapsed=${ms(r, 30, 900)}s`, shape: (_rel, h) => (h >= 1 && h <= 5 ? 20 : 0.05) },
    { id: "etl-spill", level: "warn", rate: 0.2, pattern: "spilling <_> MiB to disk (executor memory pressure)", render: ({ r }) => `spilling ${ms(r, 256, 4096)} MiB to disk (executor memory pressure)`, shape: (_rel, h) => (h >= 1 && h <= 5 ? 10 : 0) },
  ],
  "data/feature-store": [{ id: "fs", level: "info", rate: 4, pattern: "materialized view <_> rows=<_>", render: ({ r }) => `materialized view ${pick(r, ["user_features", "item_features"])} rows=${ms(r, 1000, 90000)}` }],
  "platform/metrics-scraper": [{ id: "scrape-to", level: "warn", rate: 1, pattern: "scrape timeout target=<_> after 10s", render: ({ r }) => `scrape timeout target=${pick(r, ["10.2.3.4:9100", "10.2.7.12:8080", "10.2.9.1:9090"])} after 10s` }],
  "ml-training/llm-finetune": [{ id: "ft-step", level: "info", rate: 6, pattern: "step <_> loss=<_> lr=<_> tokens/s=<_>", render: ({ r, minute }) => `step ${minute + 500000} loss=${(1.2 + r() * 0.3).toFixed(4)} lr=2.0e-05 tokens/s=${ms(r, 38000, 42000)}` }],
  "dev/preview-envs": [{ id: "pv", level: "info", rate: 1, pattern: "preview <_> idle for <_>h", render: ({ r }) => `preview pr-${ms(r, 1000, 4999)} idle for ${ms(r, 1, 96)}h` }],
};

export type DemoStream = {
  key: string;
  /** hash32(key) — mixed with the minute for per-minute randomness. */
  seed: number;
  labels: Record<string, string>;
  template: Template;
  podRate: number;
  diurnal: boolean;
};

let streamsCache: DemoStream[] | null = null;

function podNames(w: DemoWorkload): string[] {
  const r = rngFor("pods", w.cluster, w.namespace, w.name);
  const n = Math.max(1, Math.min(3, w.replicas));
  if (w.kind === "StatefulSet") return Array.from({ length: n }, (_, i) => `${w.name}-${i}`);
  if (w.kind === "CronJob") return [`${w.name}-${29000000 + Math.floor(r() * 99999)}-${podSuffix(r)}`];
  const rs = hex(r, 10).replace(/[aeiou]/g, "b").slice(0, 9);
  return Array.from({ length: n }, () => `${w.name}-${rs}-${podSuffix(r)}`);
}

export function demoStreams(): DemoStream[] {
  if (streamsCache) return streamsCache;
  const out: DemoStream[] = [];
  for (const w of namedWorkloads()) {
    const templates = TEMPLATES[`${w.namespace}/${w.name}`];
    if (!templates) continue;
    const pods = podNames(w);
    pods.forEach((pod, pi) => {
      const node = `${w.nodepool}-node-${(hash32(pod) % 7) + 1}`;
      for (const t of templates) {
        const key = `${w.cluster}/${pod}/${t.id}`;
        out.push({
          key,
          seed: hash32(key),
          diurnal: w.team === "commerce" || w.namespace === "edge",
          labels: {
            cluster: w.cluster,
            namespace: w.namespace,
            workload: w.name,
            workload_kind: w.kind,
            pod,
            container: w.containers[0]?.name ?? w.name,
            node,
            team: w.team,
            app: w.labels.app ?? w.name,
            stream: t.stream ?? "stdout",
            level: t.level,
          },
          template: t,
          podRate: t.rate * (1 + (pi % 3) * 0.07),
        });
      }
    });
  }
  streamsCache = out;
  return out;
}

export function anchorOf(now: number): number {
  return Math.floor(now / (5 * MIN)) * 5 * MIN;
}

/** murmur3 finaliser → [0, 1). Cheap enough to call millions of times. */
function mix01(h: number): number {
  h ^= h >>> 16;
  h = Math.imul(h, 0x85ebca6b);
  h ^= h >>> 13;
  h = Math.imul(h, 0xc2b2ae35);
  h ^= h >>> 16;
  return (h >>> 0) / 4294967296;
}

/**
 * Deterministic line count for a stream in epoch-minute `minute`:
 * stochastic rounding of the expected rate, so counts are integers whose
 * long-run mean is exactly the rate. Nothing exists after `nowMin`.
 */
export function countAt(s: DemoStream, minute: number, anchorMin: number, nowMin = anchorMin + 5): number {
  if (minute > nowMin) return 0;
  const rel = minute - anchorMin;
  const hour = Math.floor((((minute % 1440) + 1440) % 1440) / 60);
  // Gentle diurnal cycle for user-facing traffic.
  const diurnal = s.diurnal ? 0.75 + 0.35 * Math.sin(((hour - 8) / 24) * 2 * Math.PI) : 1;
  const expected = s.podRate * diurnal * (s.template.shape ? s.template.shape(rel, hour) : 1);
  const whole = Math.floor(expected);
  const frac = expected - whole;
  return whole + (mix01(s.seed ^ Math.imul(minute, 0x9e3779b1)) < frac ? 1 : 0);
}

export type DemoLine = {
  tsMs: number;
  tsNs: string;
  body: string;
  level: string;
  labels: Record<string, string>;
  patternId: string;
};

function renderLine(s: DemoStream, minute: number, i: number, anchorMin: number): DemoLine {
  const r = rngFor(s.key, minute, i);
  const offsetMs = Math.floor(r() * 60_000);
  const tsMs = minute * MIN + offsetMs;
  const nsFrac = Math.floor(r() * 1e6);
  const body = s.template.render({ r, minute: minute - anchorMin });
  return {
    tsMs,
    tsNs: `${tsMs}${String(nsFrac).padStart(6, "0")}`,
    body,
    level: s.template.level,
    labels: s.labels,
    patternId: s.template.id,
  };
}

/** Representative line for count-level predicates (line filters on templates). */
function sampleLine(s: DemoStream, anchorMin: number): string {
  return renderLine(s, anchorMin - 30, 0, anchorMin).body;
}

type Candidate = { s: DemoStream; exact: boolean };

/** Streams whose labels match, split into "every line matches" vs "check per line". */
function candidates(log: LogExpr, anchorMin: number): Candidate[] {
  const out: Candidate[] = [];
  for (const s of demoStreams()) {
    if (!matchLabels(log.matchers, s.labels)) continue;
    const stagesOnlyLineFilters = log.stages.every((st) => st.kind === "line");
    if (log.stages.length === 0) {
      out.push({ s, exact: true });
      continue;
    }
    // Count-level evaluation uses a representative line; per-line lists re-check each line.
    const ok = runPipeline(log, s.labels, sampleLine(s, anchorMin)) !== null || !stagesOnlyLineFilters;
    if (ok) out.push({ s, exact: false });
  }
  return out;
}

// ─── public API (mirrors LogsService) ───────────────────────────────────

export type DemoQueryResult =
  | { resultType: "streams"; lines: DemoLine[] }
  | { resultType: "matrix"; series: { labels: Record<string, string>; points: { t: number; v: number }[] }[] };

export function demoQueryLogs(
  query: string,
  opts: { startMs: number; endMs: number; limit: number; direction: "backward" | "forward"; stepMs: number; now: number },
): DemoQueryResult {
  const q = parseLogQL(query);
  if (q.type === "log") return { resultType: "streams", lines: demoLines(q, opts) };
  return { resultType: "matrix", series: evalMetric(q, opts.startMs, opts.endMs, opts.stepMs, anchorOf(opts.now) / MIN) };
}

function demoLines(log: LogExpr, o: { startMs: number; endMs: number; limit: number; direction: "backward" | "forward"; now: number }): DemoLine[] {
  const anchorMin = anchorOf(o.now) / MIN;
  const nowMin = Math.floor(o.now / MIN);
  const cands = candidates(log, anchorMin);
  const out: DemoLine[] = [];
  const firstMin = Math.floor(o.startMs / MIN);
  const lastMin = Math.floor((o.endMs - 1) / MIN);
  const limit = Math.max(1, Math.min(5000, o.limit));
  const needsCheck = log.stages.length > 0;
  const step = o.direction === "forward" ? 1 : -1;
  // Bound the scan so a needle-in-haystack query can't spin forever.
  let scanned = 0;
  for (let m = o.direction === "forward" ? firstMin : lastMin; m >= firstMin && m <= lastMin && scanned < 20_000; m += step) {
    scanned++;
    const minute: DemoLine[] = [];
    for (const { s } of cands) {
      const n = countAt(s, m, anchorMin, nowMin);
      for (let i = 0; i < n; i++) {
        const line = renderLine(s, m, i, anchorMin);
        if (line.tsMs < o.startMs || line.tsMs >= o.endMs || line.tsMs > o.now) continue;
        if (needsCheck) {
          const ls = runPipeline(log, s.labels, line.body);
          if (!ls) continue;
          line.labels = ls;
        }
        minute.push(line);
      }
    }
    minute.sort((a, b) => (o.direction === "forward" ? a.tsMs - b.tsMs : b.tsMs - a.tsMs));
    for (const l of minute) {
      out.push(l);
      if (out.length >= limit) return out;
    }
  }
  return out;
}

/** Sum of per-minute counts over [fromMs, toMs) for candidate streams, split by a key. */
function countsBy(
  cands: Candidate[],
  fromMs: number,
  toMs: number,
  keyOf: (s: DemoStream) => string,
  anchorMin: number,
): Map<string, number> {
  const out = new Map<string, number>();
  const a = Math.floor(fromMs / MIN);
  const b = Math.ceil(toMs / MIN);
  for (const { s } of cands) {
    let c = 0;
    for (let m = a; m < b; m++) c += countAt(s, m, anchorMin);
    if (c > 0) out.set(keyOf(s), (out.get(keyOf(s)) ?? 0) + c);
  }
  return out;
}

const BYTES_PER_LINE = (s: DemoStream) => 120 + (hash32(s.template.id) % 180);

function evalMetric(q: MetricExpr, startMs: number, endMs: number, stepMs: number, anchorMin: number): { labels: Record<string, string>; points: { t: number; v: number }[] }[] {
  const steps: number[] = [];
  const first = Math.ceil(startMs / stepMs) * stepMs;
  for (let t = first; t <= endMs && steps.length < 2000; t += stepMs) steps.push(t);

  const evalRange = (r: Extract<MetricExpr, { type: "range" }>) => {
    const cands = candidates(r.log, anchorMin);
    // Series identity = all stream labels (like Loki) — aggregations regroup.
    const series = new Map<string, { labels: Record<string, string>; points: { t: number; v: number }[] }>();
    for (const t of steps) {
      for (const c of cands) {
        const n = countsBy([c], t - r.rangeMs, t, () => "x", anchorMin).get("x") ?? 0;
        if (n === 0 && r.fn !== "absent_over_time") continue;
        const bytes = n * BYTES_PER_LINE(c.s);
        const v =
          r.fn === "rate" ? n / (r.rangeMs / 1000) : r.fn === "bytes_over_time" ? bytes : r.fn === "bytes_rate" ? bytes / (r.rangeMs / 1000) : n;
        const key = c.s.key;
        let e = series.get(key);
        if (!e) {
          e = { labels: { ...c.s.labels }, points: [] };
          series.set(key, e);
        }
        e.points.push({ t, v });
      }
    }
    return [...series.values()];
  };

  const evalExpr = (e: MetricExpr): { labels: Record<string, string>; points: { t: number; v: number }[] }[] => {
    if (e.type === "range") return evalRange(e);
    if (e.type === "binop") {
      const inner = evalExpr(e.expr);
      const cmp = [">", ">=", "<", "<=", "==", "!="].includes(e.op);
      return inner
        .map((s) => ({
          labels: s.labels,
          points: s.points
            .map((p) => {
              if (!cmp) {
                const v = e.op === "*" ? p.v * e.scalar : e.op === "/" ? p.v / e.scalar : e.op === "+" ? p.v + e.scalar : p.v - e.scalar;
                return { t: p.t, v };
              }
              const keep =
                e.op === ">" ? p.v > e.scalar : e.op === ">=" ? p.v >= e.scalar : e.op === "<" ? p.v < e.scalar : e.op === "<=" ? p.v <= e.scalar : e.op === "==" ? p.v === e.scalar : p.v !== e.scalar;
              return keep ? p : null;
            })
            .filter((p): p is { t: number; v: number } => p !== null),
        }))
        .filter((s) => s.points.length > 0);
    }
    // aggregation
    const inner = evalExpr(e.expr);
    if (e.fn === "topk" || e.fn === "bottomk") {
      const k = Math.max(1, e.param ?? 5);
      const total = (s: (typeof inner)[number]) => s.points.reduce((a, p) => a + p.v, 0);
      const sorted = [...inner].sort((a, b) => (e.fn === "topk" ? total(b) - total(a) : total(a) - total(b)));
      return sorted.slice(0, k);
    }
    const groups = new Map<string, { labels: Record<string, string>; byT: Map<number, number[]> }>();
    for (const s of inner) {
      const labels: Record<string, string> = {};
      if (e.without) {
        for (const [k, v] of Object.entries(s.labels)) if (!e.by.includes(k)) labels[k] = v;
      } else {
        for (const k of e.by) if (s.labels[k] !== undefined) labels[k] = s.labels[k];
      }
      const key = JSON.stringify(labels);
      let g = groups.get(key);
      if (!g) {
        g = { labels, byT: new Map() };
        groups.set(key, g);
      }
      for (const p of s.points) {
        const arr = g.byT.get(p.t) ?? [];
        arr.push(p.v);
        g.byT.set(p.t, arr);
      }
    }
    return [...groups.values()].map((g) => ({
      labels: g.labels,
      points: [...g.byT.entries()]
        .sort((a, b) => a[0] - b[0])
        .map(([t, vs]) => ({
          t,
          v:
            e.fn === "sum"
              ? vs.reduce((a, b) => a + b, 0)
              : e.fn === "avg"
                ? vs.reduce((a, b) => a + b, 0) / vs.length
                : e.fn === "min"
                  ? Math.min(...vs)
                  : e.fn === "max"
                    ? Math.max(...vs)
                    : vs.length,
        })),
    }));
  };
  return evalExpr(q);
}

export type DemoVolume = {
  times: number[];
  stepMs: number;
  series: { key: string; values: number[] }[];
  totalLines: number;
  totalBytes: number;
};

export function demoLogVolume(query: string, o: { startMs: number; endMs: number; stepMs: number; groupBy: string; now: number }): DemoVolume {
  const q = parseLogQL(query);
  const log = logExprOf(q);
  const anchorMin = anchorOf(o.now) / MIN;
  const cands = candidates(log, anchorMin);
  const times: number[] = [];
  const first = Math.floor(o.startMs / o.stepMs) * o.stepMs;
  for (let t = first; t < o.endMs && times.length < 1000; t += o.stepMs) times.push(t);
  const byKey = new Map<string, number[]>();
  let lines = 0;
  let bytes = 0;
  const minuteStep = Math.max(1, Math.round(o.stepMs / MIN));
  for (const { s } of cands) {
    const key = o.groupBy === "level" ? normalizeLevel(s.labels.level) : (s.labels[o.groupBy] ?? "unknown");
    let vals = byKey.get(key);
    if (!vals) {
      vals = new Array(times.length).fill(0);
      byKey.set(key, vals);
    }
    const bpl = BYTES_PER_LINE(s);
    for (let i = 0; i < times.length; i++) {
      const a = Math.floor(Math.max(times[i], o.startMs) / MIN);
      const b = Math.min(a + minuteStep, Math.ceil(Math.min(times[i] + o.stepMs, o.endMs) / MIN));
      let c = 0;
      for (let m = a; m < b; m++) c += countAt(s, m, anchorMin);
      vals[i] += c;
      lines += c;
      bytes += c * bpl;
    }
  }
  return {
    times,
    stepMs: o.stepMs,
    series: [...byKey.entries()].map(([key, values]) => ({ key, values })),
    totalLines: lines,
    totalBytes: bytes,
  };
}

export type DemoPattern = { pattern: string; count: number; level: string; sharePct: number; trend: { t: number; v: number }[]; sample: string };

export function demoLogPatterns(query: string, o: { startMs: number; endMs: number; limit: number; now: number }): { patterns: DemoPattern[]; linesAnalyzed: number } {
  const q: Query = parseLogQL(query);
  const log = logExprOf(q);
  const anchorMin = anchorOf(o.now) / MIN;
  const cands = candidates(log, anchorMin);
  const stepMs = Math.max(MIN, Math.floor((o.endMs - o.startMs) / 24 / MIN) * MIN);
  const byTemplate = new Map<string, { t: Template; s: DemoStream; count: number; trend: Map<number, number> }>();
  for (const c of cands) {
    const a = Math.floor(o.startMs / MIN);
    const b = Math.ceil(o.endMs / MIN);
    let e = byTemplate.get(c.s.template.id);
    if (!e) {
      e = { t: c.s.template, s: c.s, count: 0, trend: new Map() };
      byTemplate.set(c.s.template.id, e);
    }
    for (let m = a; m < b; m++) {
      const n = countAt(c.s, m, anchorMin);
      if (n === 0) continue;
      e.count += n;
      const bucket = Math.floor((m * MIN) / stepMs) * stepMs;
      e.trend.set(bucket, (e.trend.get(bucket) ?? 0) + n);
    }
  }
  const total = [...byTemplate.values()].reduce((s, e) => s + e.count, 0);
  const buckets: number[] = [];
  for (let t = Math.floor(o.startMs / stepMs) * stepMs; t < o.endMs; t += stepMs) buckets.push(t);
  const patterns = [...byTemplate.values()]
    .filter((e) => e.count > 0)
    .sort((a, b) => b.count - a.count)
    .slice(0, Math.max(1, o.limit))
    .map((e) => ({
      pattern: e.t.pattern,
      count: e.count,
      level: e.t.level,
      sharePct: total > 0 ? (e.count / total) * 100 : 0,
      trend: buckets.map((t) => ({ t, v: e.trend.get(t) ?? 0 })),
      sample: renderLine(e.s, anchorMin - 3, 0, anchorMin).body,
    }));
  return { patterns, linesAnalyzed: total };
}

export function demoLogLabels(name?: string, query?: string): { names: string[]; values: string[] } {
  let streams = demoStreams();
  if (query) {
    try {
      const log = logExprOf(parseLogQL(query));
      streams = streams.filter((s) => matchLabels(log.matchers, s.labels));
    } catch {
      /* an unparseable scope query just means "unscoped" */
    }
  }
  if (!name) {
    const names = new Set<string>();
    for (const s of streams) for (const k of Object.keys(s.labels)) names.add(k);
    return { names: [...names].sort(), values: [] };
  }
  const values = new Set<string>();
  for (const s of streams) if (s.labels[name] !== undefined) values.add(s.labels[name]);
  return { names: [], values: [...values].sort().slice(0, 500) };
}

/** Lines just before and after a line in the same pod (for "show context"). */
export function demoContext(line: { tsMs: number; labels: Record<string, string> }, around: number, now: number): { before: DemoLine[]; after: DemoLine[] } {
  const sel = `{namespace="${line.labels.namespace}", pod="${line.labels.pod}"}`;
  const log = logExprOf(parseLogQL(sel));
  const before = demoLines(log, { startMs: line.tsMs - 30 * MIN, endMs: line.tsMs, limit: around, direction: "backward", now }).reverse();
  const after = demoLines(log, { startMs: line.tsMs + 1, endMs: line.tsMs + 30 * MIN, limit: around, direction: "forward", now });
  return { before, after };
}

/** New lines in (sinceMs, untilMs] for live tail. */
export function demoTail(query: string, sinceMs: number, untilMs: number, now: number): DemoLine[] {
  const log = logExprOf(parseLogQL(query));
  return demoLines(log, { startMs: sinceMs + 1, endMs: untilMs + 1, limit: 500, direction: "forward", now });
}
