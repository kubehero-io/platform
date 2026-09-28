// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo ProfilesService: realistic folded stacks per runtime (Go HTTP,
// Python/PyTorch serving, Node SSR, Rust/tokio, JVM/Spark), scaled to
// each demo workload's measured CPU and priced with its CPU spend. The
// Go services carry a baseline so diff mode has a story: checkout's
// json.Marshal regressed (+~40%) while gzip got cheaper.

import { flattenFolded, topFunctions, type Folded } from "@/lib/flame/tree";
import type { Flamegraph, ProfileTarget, TopFunction } from "@/lib/profiles/types";
import { namedWorkloads, type DemoWorkload, type ProfileKind } from "./world";

// "frames value baseline" — values are relative weights.
const GO_HTTP = (svc: string, handler: string) => `
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*API).priceCart;encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue;encoding/json.structEncoder.encode 300 205
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*API).priceCart;encoding/json.Marshal;encoding/json.(*encodeState).marshal;encoding/json.(*encodeState).reflectValue;encoding/json.mapEncoder.encode;sort.Strings 85 60
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*API).priceCart;encoding/json.Marshal;runtime.mallocgc 45 30
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*API).priceCart;main.(*Pricer).Quote;main.(*Pricer).applyPromotions 140 140
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};database/sql.(*DB).QueryContext;github.com/jackc/pgx/v5.(*Conn).Query;github.com/jackc/pgx/v5/pgconn.(*PgConn).receiveMessage;net.(*conn).Read;internal/poll.(*FD).Read;syscall.Syscall 160 150
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*${svc}Client).Call;net/http.(*Client).Do;net/http.(*Transport).roundTrip;crypto/tls.(*Conn).Write;crypto/aes.gcmAsmSeal 70 70
runtime.goexit;net/http.(*conn).serve;net/http.serverHandler.ServeHTTP;github.com/go-chi/chi/v5.(*Mux).ServeHTTP;github.com/go-chi/chi/v5/middleware.RequestID.func1;main.(*API).${handler};main.(*API).writeJSON;compress/gzip.(*Writer).Write;compress/flate.(*compressor).deflate 95 150
runtime.goexit;net/http.(*conn).serve;net/http.(*conn).readRequest;net/textproto.(*Reader).ReadMIMEHeader;bufio.(*Reader).ReadLine 40 40
runtime.gcBgMarkWorker;runtime.gcDrain;runtime.scanobject 190 160
runtime.gcBgMarkWorker;runtime.gcDrain;runtime.greyobject 55 50
runtime.mcall;runtime.park_m;runtime.schedule;runtime.findRunnable;runtime.netpoll 80 85
runtime.morestack;runtime.newstack;runtime.copystack 18 18
`;

const PYTHON_ML = `
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;app.server.infer;app.model.generate;torch.nn.modules.module.Module._call_impl;transformers.models.llama.modeling_llama.LlamaForCausalLM.forward;transformers.models.llama.modeling_llama.LlamaDecoderLayer.forward;transformers.models.llama.modeling_llama.LlamaAttention.forward;torch._C._nn.scaled_dot_product_attention 210
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;app.server.infer;app.model.generate;torch.nn.modules.module.Module._call_impl;transformers.models.llama.modeling_llama.LlamaForCausalLM.forward;transformers.models.llama.modeling_llama.LlamaDecoderLayer.forward;transformers.models.llama.modeling_llama.LlamaMLP.forward;torch._C._nn.linear 180
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;app.server.infer;app.model.generate;torch.cuda.synchronize;[native] cudaStreamSynchronize 260
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;app.server.infer;app.tokenize;tokenizers.Tokenizer.encode_batch 70
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;app.server.infer;app.postprocess;json.dumps;json.encoder.JSONEncoder.iterencode 30
<module>;uvicorn.main.run;asyncio.runners.run;asyncio.base_events.BaseEventLoop.run_forever;asyncio.selector_events._SelectorSocketTransport._read_ready 25
[native] python_gc_collect 40
`;

const NODE_WEB = `
node::Start;node::SpinEventLoop;uv_run;processTicksAndRejections;renderToPipeableStream;renderRootSync;renderElement;ProductPage;ProductCard;formatPrice 90
node::Start;node::SpinEventLoop;uv_run;processTicksAndRejections;renderToPipeableStream;renderRootSync;renderElement;ProductPage;ProductCard;Image 60
node::Start;node::SpinEventLoop;uv_run;processTicksAndRejections;renderToPipeableStream;renderRootSync;renderElement;CheckoutPage;useCartTotals 70
node::Start;node::SpinEventLoop;uv_run;processTicksAndRejections;fetchJson;JSON.parse 65
node::Start;node::SpinEventLoop;uv_run;uv__io_poll;zlib::DeflateStream::Write 80
node::Start;node::SpinEventLoop;uv_run;uv__io_poll;tls::TLSWrap::DoWrite 35
v8::internal::Heap::CollectGarbage;v8::internal::MarkCompactCollector::CollectGarbage 110
v8::internal::Compiler::Compile;v8::internal::Parser::ParseProgram 20
`;

const RUST_DB = `
tokio::runtime::task::harness::Harness::poll;vectordb::ingest::handle_batch;vectordb::index::hnsw::Hnsw::insert;vectordb::index::hnsw::Hnsw::search_layer;vectordb::distance::cosine;core::core_arch::x86::avx2::_mm256_fmadd_ps 380
tokio::runtime::task::harness::Harness::poll;vectordb::ingest::handle_batch;vectordb::index::hnsw::Hnsw::insert;vectordb::index::hnsw::Hnsw::connect_neighbours 120
tokio::runtime::task::harness::Harness::poll;vectordb::ingest::handle_batch;serde_json::de::from_slice;serde_json::de::Deserializer::parse_whitespace 80
tokio::runtime::task::harness::Harness::poll;vectordb::wal::Wal::append;std::fs::File::sync_data 45
tokio::runtime::task::harness::Harness::poll;tokio::net::tcp::stream::TcpStream::poll_read 60
tokio::runtime::scheduler::multi_thread::worker::Context::park;parking_lot_core::parking_lot::park 70
`;

const JVM_BATCH = `
java.lang.Thread.run;java.util.concurrent.ThreadPoolExecutor$Worker.run;org.apache.spark.executor.Executor$TaskRunner.run;org.apache.spark.scheduler.ResultTask.runTask;org.apache.spark.sql.execution.WholeStageCodegenExec.doExecute;org.apache.spark.sql.execution.aggregate.HashAggregateExec.doConsume;org.apache.spark.unsafe.map.BytesToBytesMap.lookup 260
java.lang.Thread.run;java.util.concurrent.ThreadPoolExecutor$Worker.run;org.apache.spark.executor.Executor$TaskRunner.run;org.apache.spark.scheduler.ShuffleMapTask.runTask;org.apache.spark.shuffle.sort.UnsafeShuffleWriter.write;org.apache.spark.util.collection.unsafe.sort.UnsafeExternalSorter.spill 180
java.lang.Thread.run;java.util.concurrent.ThreadPoolExecutor$Worker.run;org.apache.spark.executor.Executor$TaskRunner.run;org.apache.spark.sql.execution.datasources.parquet.ParquetFileFormat.buildReader;org.apache.parquet.column.impl.ColumnReaderImpl.readValue 150
java.lang.Thread.run;org.apache.spark.sql.execution.datasources.FileFormatWriter.write;org.apache.parquet.hadoop.ParquetOutputFormat.write;com.github.luben.zstd.Zstd.compress 90
G1_Young_Gen;G1ParScanThreadState::trim_queue 120
`;

const STACKS: Record<ProfileKind, (w: DemoWorkload) => string> = {
  "go-http": (w) => GO_HTTP(w.name === "checkout" ? "Payments" : "Upstream", w.name === "checkout" ? "Checkout" : "Handle"),
  "python-ml": () => PYTHON_ML,
  "node-web": () => NODE_WEB,
  "rust-db": () => RUST_DB,
  "jvm-batch": () => JVM_BATCH,
};

const TYPES: Record<ProfileKind, string[]> = {
  "go-http": ["cpu", "alloc_space", "alloc_objects", "inuse_space", "goroutines"],
  "python-ml": ["cpu"],
  "node-web": ["cpu", "alloc_space"],
  "rust-db": ["cpu"],
  "jvm-batch": ["cpu", "alloc_space"],
};

const ORIGIN: Record<ProfileKind, string> = {
  "go-http": "pprof-scrape",
  "python-ml": "ebpf",
  "node-web": "pyroscope",
  "rust-db": "ebpf",
  "jvm-batch": "ebpf",
};

function cores(w: DemoWorkload): number {
  return w.containers.reduce((s, c) => s + c.cpuP50, 0) * w.replicas;
}

export function demoProfileTargets(now = Date.now(), namespace?: string): ProfileTarget[] {
  return namedWorkloads()
    .filter((w) => w.profile && (!namespace || w.namespace === namespace))
    .map((w) => ({
      service: w.name,
      namespace: w.namespace,
      workload: w.name,
      types: TYPES[w.profile!],
      origin: ORIGIN[w.profile!],
      lastSeenMs: now - 4_000 - (w.name.length % 9) * 1_000,
      cpuCoresAvg: cores(w),
      costUsdMonth: w.cpuUsd,
    }))
    .sort((a, b) => b.costUsdMonth - a.costUsdMonth);
}

function findTarget(service: string, namespace?: string): DemoWorkload | undefined {
  return namedWorkloads().find((w) => w.profile && w.name === service && (!namespace || w.namespace === namespace));
}

/** Allocation profiles lean on allocation-heavy frames; reshape weights by type. */
function reweight(stacks: Folded[], type: string): Folded[] {
  if (type === "cpu") return stacks;
  return stacks.map((s) => {
    const leaf = s.frames[s.frames.length - 1];
    const allocHeavy = /json|mallocgc|Marshal|parse|Deserializer|growSlice|BytesToBytesMap|renderElement/i.test(s.frames.join(";"));
    const gc = /gc|Garbage|G1_/i.test(leaf);
    const f = gc ? 0 : allocHeavy ? 3 : 0.4;
    return { frames: s.frames, value: Math.round(s.value * f), baseline: s.baseline !== undefined ? Math.round(s.baseline * f) : undefined };
  });
}

const UNIT: Record<string, string> = {
  cpu: "nanoseconds",
  alloc_space: "bytes",
  inuse_space: "bytes",
  alloc_objects: "count",
  goroutines: "count",
};

export function demoFlamegraph(
  q: { service: string; namespace?: string; type?: string; startMs: number; endMs: number; diff?: boolean },
): Flamegraph | null {
  const w = findTarget(q.service, q.namespace);
  if (!w || !w.profile) return null;
  const type = TYPES[w.profile].includes(q.type ?? "cpu") ? (q.type ?? "cpu") : "cpu";
  const raw = STACKS[w.profile](w)
    .trim()
    .split("\n")
    .map((l) => {
      const m = /^(.+?)\s+(\d+)(?:\s+(\d+))?$/.exec(l.trim());
      return m ? { frames: m[1].split(";"), value: Number(m[2]), baseline: m[3] ? Number(m[3]) : Number(m[2]) } : null;
    })
    .filter((x): x is { frames: string[]; value: number; baseline: number } => x !== null);
  const stacks = reweight(raw, type);
  const weight = stacks.reduce((s, x) => s + x.value, 0) || 1;
  const bWeight = stacks.reduce((s, x) => s + (x.baseline ?? 0), 0) || 1;
  // Scale weights to the window: CPU ns = cores × seconds; allocations ~ 40 MiB/s/core.
  const seconds = Math.max(1, (q.endMs - q.startMs) / 1000);
  const target =
    type === "cpu" ? cores(w) * seconds * 1e9 : type.endsWith("space") ? cores(w) * seconds * 40 * 1024 * 1024 : type === "goroutines" ? 1800 * w.replicas : cores(w) * seconds * 90_000;
  const scaled: Folded[] = stacks.map((s) => ({
    frames: s.frames,
    value: Math.round((s.value / weight) * target),
    baseline: q.diff ? Math.round(((s.baseline ?? 0) / bWeight) * target * 0.97) : 0,
  }));
  const nodes = flattenFolded(scaled);
  return {
    nodes,
    total: nodes[0]?.total ?? 0,
    unit: UNIT[type] ?? "count",
    type,
    baselineTotal: q.diff ? (nodes[0]?.baselineTotal ?? 0) : 0,
    samples: Math.round(seconds * 100 * cores(w)),
    costUsdMonth: type === "cpu" ? w.cpuUsd : 0,
  };
}

export function demoTopFunctions(q: { service: string; namespace?: string; type?: string; startMs: number; endMs: number; limit?: number; orderBy?: "self" | "total" }): TopFunction[] {
  const fg = demoFlamegraph(q);
  if (!fg) return [];
  const fns = topFunctions(fg.nodes);
  const key = q.orderBy === "total" ? "total" : "self";
  return fns
    .sort((a, b) => b[key] - a[key])
    .slice(0, q.limit ?? 25)
    .map((f) => ({ ...f, selfCostUsdMonth: fg.total > 0 ? (fg.costUsdMonth * f.self) / fg.total : 0 }));
}
