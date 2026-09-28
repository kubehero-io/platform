# KubeHero — Architecture

> The public architecture overview for KubeHero: open-source, self-hosted Kubernetes observability and FinOps in one install. This is the map; the implementation lives in this repo.

---

## 0. North Star

**Every signal. Every dollar. One agent.**

Teams run five tools to understand a Kubernetes fleet — a log store, a cost allocator, a continuous profiler, a network observer, an alerting stack — each with its own agent on every node, its own UI and its own bill. None of them can answer the question that matters at 3 AM: *what changed, what does it cost, and what exactly should we do about it?*

KubeHero collects every signal once, stores it once, prices it, and puts a read-only agent on top that explains it and proposes guarded changes:

1. **Attribution** — which pod, workload, team and cluster is spending (compute, GPUs, network, logs) and wasting — right now and over time.
2. **Explanation** — logs, profiles, flows, events and spend correlated, so a cost spike comes with its cause.
3. **Guarded action** — rightsizing and spend ceilings expressed as policy CRDs that a human arms and the operator executes, bounded and reversible.

---

## 1. Repository layout

```
kubehero-platform/
├── apps/
│   ├── dashboard/          # Next.js 16 — the UI
│   └── docs/               # Next.js + Fumadocs — docs site
├── services/
│   ├── control-plane/      # Go — APIs, ingest, query engines, alerting, compat shims
│   ├── collector/          # Go — node agent (DaemonSet): cost, logs, pprof, eBPF
│   ├── operator/           # Go — controller-runtime, guarded policy CRDs
│   ├── advisor/            # Go — agentic layer: briefings, investigations
│   └── pricing-engine/     # Go — AWS / GCP / Azure price catalog
├── cli/kubehero/           # Go — CLI + MCP server
├── packages/
│   ├── proto/              # Protobuf / Connect contracts (Go + TS generated)
│   ├── cost-model/         # Canonical cost math (Go)
│   └── tsconfig/           # Shared TS configs
├── deploy/
│   ├── helm/               # The kubehero chart (embedded Postgres + ClickHouse)
│   └── terraform/          # Reference infra (GKE + Cloud SQL + ClickHouse)
└── infra/
    ├── demo/               # compose / kind demos, demo telemetry generator, e2e harness
    └── dagger/             # CI as code
```

Licensing: Apache 2.0 — CLI, collector, cost-model, proto. BSL 1.1 — control plane, operator, advisor, pricing engine, dashboard (source-available, free to self-host).

---

## 2. System diagram

```
┌──────────────────────────── EVERY CLUSTER (K8s ≥ 1.28 · AKS · GKE · EKS) ───────────────────────────┐
│                                                                                                     │
│  collector (DaemonSet, one per node — reports ONLY its node)          operator (Deployment)        │
│  ├─ cost: pods × node price, measured usage (kubelet Summary API)     ├─ BudgetPolicy              │
│  ├─ usage history per container (rightsizing)                          ├─ CeilingPolicy (burn rate) │
│  ├─ logs: tails /var/log/pods (CRI + json-file), levels, trace ids     ├─ RightsizingPolicy         │
│  ├─ profiles: pprof scraping of annotated pods                         │   recommend·shadow·apply   │
│  ├─ eBPF: cgroup_skb flow accounting, TCP retransmits, CPU sampling    └─ human-armed, bounded,     │
│  └─ events: OOM kills, crash loops, restarts (+ leader: unschedulable      audited, reversible      │
│     pods, Warning events)                                                                           │
└──────────────┬──────────────────────────────────────────────────────────────────────▲──────────────┘
               │ Connect-RPC (protobuf, gzip, per-cluster token)                       │ burn rate,
               ▼                                                                       │ rightsizing
┌──────────────────────────────────── CONTROL PLANE ──────────────────────────────────┴──────────────┐
│  ingest: IngestPodCost + TelemetryService    compat: Loki push/query · OTLP/HTTP logs ·            │
│  engines: LogQL · Drain patterns · allocation (OpenCost-compatible) · FOCUS export ·               │
│           rightsizing percentiles · flamegraphs · service map · anomalies · alert evaluator        │
│  auth: API keys, cluster tokens, OIDC (JWKS) · audit log (HMAC-signed, append-only)                │
│        ┌────────────────────────┐                        ┌──────────────────┐                      │
│        │ ClickHouse             │  every time series      │ Postgres         │ clusters, policies,  │
│        │ cost · logs · profiles │  (TTLs + rollups)       │ audit · alerts   │ silences, users      │
│        │ flows · usage · events │                         └──────────────────┘                      │
│        └────────────────────────┘                                                                   │
└───────▲───────────────────▲─────────────────────▲──────────────────────────▲──────────────────────┘
        │ read RPCs          │                      │                          │ Loki API / OpenCost API
  ┌─────┴──────┐     ┌───────┴────────┐     ┌──────┴───────┐          ┌───────┴──────────┐
  │ dashboard  │     │ advisor        │     │ CLI · MCP    │          │ Grafana · OpenCost│
  │ (Next.js)  │────▶│ briefings ·    │◀────│ server       │          │ consumers · agents│
  └────────────┘     │ investigations │     └──────────────┘          └──────────────────┘
                     │ read-only tools│
                     └───────┬────────┘
                             │ optional (BYO key)
                             ▼
                        Claude API
```

---

## 3. Components

### 3.1 Collector (DaemonSet, Go)

One pod per node. Each collector reports **only the node it runs on** (`NODE_NAME` from the downward API): its pods' cost and usage, its container logs, its eBPF flows and CPU samples. Cluster-scoped scans — unschedulable pods and Kubernetes Warning events — run on a single leader elected through a `coordination.k8s.io` Lease. (Before 0.3 every collector reported every pod in the cluster, so an N-node cluster was billed N times; see the changelog.)

- **Cost** — pods are priced from their node: `max(requests, measured usage)` share of allocatable × the node's hourly price, split into CPU / memory / GPU parts by `packages/cost-model` (GPU nodes attribute a documented share of the price to accelerators). Node price comes from the `kubehero.io/node-hourly-usd` annotation, the pricing engine (live cloud price lists), or an estimate — the source is recorded on every node sample. Each sample carries the interval it covers, so spend is `rate × interval`, and each collector reports its node's price and allocation so idle cost (price − allocated) is exact.
- **Usage history** — per-container CPU and working-set memory from the kubelet Summary API, with requests, limits, restarts and the last termination reason. This is what rightsizing percentiles are computed from.
- **Logs** — tails `/var/log/pods` (CRI and docker json-file formats), reassembles partial lines, survives rotation and truncation with checkpointed offsets, detects levels and trace ids, rate-limits per container and ships batches. A drop-in for Promtail / Alloy log collection.
- **Profiles (pull)** — scrapes `/debug/pprof` from pods annotated the Grafana/Pyroscope way (`profiles.grafana.com/cpu.scrape: "true"` + port) or with `kubehero.io/profile*`.
- **eBPF** (Linux ≥ 5.8, cgroup v2, privileged): `cgroup_skb` ingress/egress programs on the root cgroup account bytes and packets per endpoint pair; the `tcp:tcp_retransmit_skb` tracepoint counts retransmits; `perf_event` sampling at 49 Hz captures kernel + user stacks for every container on the node, symbolised from ELF symbol tables (and Go's pclntab). Addresses resolve to pods, Services and nodes from informer caches; cgroup ids resolve to containers. No CO-RE/BTF dependency: programs use stable UAPI contexts only. When the kernel or privileges don't allow it, the collector logs one line and keeps collecting everything else.
- **Events** — OOM kills, crash loops, image pull failures, restarts, evictions, node pressure, unschedulable pods.

RBAC is read-only: pods, nodes, services, owners (ReplicaSets, Jobs), events, `nodes/proxy` for kubelet stats, and a namespaced Lease for leader election. The collector never writes to customer resources.

### 3.2 Control plane (Go, Connect-RPC)

- **Transport**: [Connect-RPC](https://connectrpc.com/) — the same `.proto` contracts serve Go, TypeScript, the CLI and plain `curl`.
- **Stores**: ClickHouse for every time series, Postgres for metadata. ClickHouse schema is versioned (`internal/clickhouse/migrations`, applied once, recorded in `kubehero_schema_migrations`); Postgres uses golang-migrate. `control-plane migrate` applies both and exits. On startup a configured store that isn't reachable yet is retried for `KUBEHERO_STORE_WAIT` before the process exits — never a silent fallback to demo data.
- **Signals and engines**
  - *Cost* — `workload_cost_1h` / `node_cost_1h` rollups priced as rate × interval. `CostService.GetAllocation` aggregates by any dimension (namespace, workload, controller, pod, node, nodepool, team, cost center, zone, `label:<key>`) with idle rows, idle sharing (weighted / even) and shared-namespace redistribution, in OpenCost's model; `/allocation/compute` serves the same data in OpenCost's wire format. `/api/v1/export/focus` streams a FinOps FOCUS CSV. Time series with a month-end forecast and a fleet efficiency score round it out.
  - *Rightsizing* — per-container p50/p95/p99/max from 5-minute t-digest rollups; CPU sized to a percentile plus headroom, memory never below observed max and never down after an OOM kill; confidence from coverage; savings priced from the workload's own cost per core-hour and GiB-hour.
  - *Logs* — a LogQL engine (selectors, line filters, `json`/`logfmt`/`regexp` parsers, label filters, `line_format`, metric queries with range aggregations and `sum/avg/min/max/topk by`) compiled to parameterised ClickHouse SQL, with per-minute volume rollups for cheap metric queries; Drain pattern mining; live tail. The Loki push and query APIs make Promtail, Alloy, Fluent Bit and Grafana's Loki datasource work unchanged; OTLP/HTTP logs are accepted too.
  - *Profiles* — content-addressed stacks (`profile_stacks`) and samples; merged flamegraphs, diff flamegraphs against a baseline window, top functions, and $/month per function from the workload's CPU spend. Pyroscope's `/ingest` accepts pushes from Pyroscope SDKs.
  - *Network* — flows priced at ingest (internet egress and cross-zone $/GB per cloud, configurable); workload service map with bytes, retransmits and cost per edge; per-workload network spend.
  - *Alerting* — one evaluator for every signal: LogQL metric queries, spend rate, budget burn, anomalies, network spend and cluster events. Pending → firing → resolved state machine, silences, re-notification; channels: Slack, PagerDuty, Opsgenie, Teams, Discord, generic webhooks, Alertmanager.
  - *Anomalies* — rolling z-scores of each workload's spend against its trailing baseline.
- **Auth**: static API keys with roles (owner / admin / auditor / member / viewer), per-cluster enrollment tokens (only their SHA-256 is stored), OIDC with JWKS verification and group → role mapping, SCIM 2.0 provisioning. `WhoAmI` echoes the resolved caller. The Helm chart requires auth by default and wires generated tokens into every in-cluster client.
- **Audit**: every policy change, arming, enforcement and apply writes an HMAC-signed, append-only row.
- **Demo mode**: RPCs whose store isn't configured serve clearly-labelled fixtures (`source: "demo"`) so a fresh install and the docs never look empty. `KUBEHERO_DEMO_MODE=false` turns fixtures off; those RPCs then fail with `FailedPrecondition`.

### 3.3 Operator (Go, controller-runtime)

Three CRDs, all conservative by default:

```yaml
apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata: { name: shop-rightsizing, namespace: kubehero-system }
spec:
  scope:
    namespaceSelector: { matchLabels: { kubehero.io/rightsizing: enabled } }
  mode: recommend            # recommend | shadow | apply
  safety:
    observationWindow: 7d
    p95HeadroomPct: 15
    maxChangePerDay: 1
    minReplicas: 2
```

- **RightsizingPolicy** — `recommend` writes per-container recommendations into status; `shadow` records the changes it *would* make (and audits them); `apply` patches requests only when the policy is armed, confidence is sufficient, the step is bounded, the workload isn't mid-rollout, no VPA owns it, and memory never goes below observed max or down after OOM kills. Previous resources are recorded so `kubehero undo` restores them.
- **BudgetPolicy / CeilingPolicy** — burn-rate triggered escalations (`alert` → `hpa.cap` → `pod.evict` → `nodepool.cordon`), dry-run by default, `humanArm: true` by default, every action reversible within the cooldown and audited.

### 3.4 Advisor (Go, Connect-RPC)

The agentic layer, **read-only by construction**: its only inputs are the control plane's read RPCs; it holds no Kubernetes credentials and calls no mutation RPCs.

- **Briefings** — headline, markdown report, TTS-ready script (the dashboard reads it aloud with the browser's SpeechSynthesis), and proposed actions.
- **Investigations** (`Investigate` / `InvestigateStream`) — "why did checkout's spend jump?" runs a bounded tool loop over allocation, anomalies, LogQL queries and patterns, flamegraph summaries, the service map, network costs, alerts and rightsizing, and returns an answer that cites its evidence with deep links, plus proposals.
- **Brains** — Claude (bring your own Anthropic API key; adaptive thinking; server-side refusal fallbacks) with a deterministic rules brain as the offline default and the fallback on any error. Without a key nothing leaves the cluster.
- **Guardrail** — every proposal from any brain is validated: whitelisted action kinds, finite non-negative impact, and a manifest that must parse to a BudgetPolicy / CeilingPolicy / RightsizingPolicy. Anything else is downgraded to investigate-only. Proposals are applied by humans through the operator's arming flow.

### 3.5 CLI and MCP server

`kubehero` covers every surface from a terminal — `cost`, `logs` (with `-f` live tail and `--patterns`), `profile top|flame`, `network`, `rightsize`, `alerts`, `ask`, `cap --arm`, `undo` — and `kubehero mcp` serves the same read-only tools over the Model Context Protocol (stdio or streamable HTTP) so Claude and other agents can query a fleet safely.

### 3.6 Dashboard (Next.js 16)

Server components fetch over Connect; streaming endpoints (log tail, investigations) are proxied as SSE. Sign-in is token-based: each user acts with their own token and role (`WhoAmI`). Every page degrades to labelled demo data when a signal isn't configured. Explorers: overview, allocation, rightsizing, logs (LogQL, volume, patterns, live tail), profiles (flamegraph, diff, top functions), network map, alerts (rules, silences, test), Ask, advisor briefings with voice, budgets and ceilings, posture, GPU and capacity, and a per-workload correlation page.

### 3.7 Pricing engine

`PricingService.Quote` over a layered catalog: in-memory cache → the cloud's live price API (AWS Pricing, GCP Cloud Billing Catalog, Azure Retail Prices) → a static built-in table, so a quote never fails because a cloud API is down.

---

## 4. Deployment

- **One chart, real data from minute one.** `helm install kubehero oci://ghcr.io/kubehero-io/charts/kubehero -n kubehero-system --create-namespace` installs every component plus single-replica Postgres 18 and ClickHouse 26.8 LTS (official images, persistent volumes). Production installs point at managed stores with a DSN Secret (`values.production.yaml`).
- **Federated collectors.** Run collector + operator only in workload clusters (`controlPlane.enabled=false`, `controlPlane.url`, `cluster.tokenSecret` from `kubehero cluster add`) and one control plane in a hub.
- **Air-gapped.** Every image mirrors to your registry (`values.airgap.yaml`); no phone-home.
- **Local.** `docker compose up` runs the stack with a synthetic three-cluster fleet streamed through the real ingest APIs; `infra/demo/e2e-kind.sh` installs the real chart on kind and asserts every signal end to end.

---

## 5. Security & trust

1. **No telemetry leaves your infrastructure.** The only optional exception is the advisor's LLM brain: with an Anthropic key set, the telemetry snapshot and tool results it reasons over go to Anthropic's API. Leave it unset and nothing leaves.
2. **Read-only collection.** The collector has no write permissions on customer resources. eBPF requires a privileged container on each node — scoped to reading; the programs never drop or alter traffic.
3. **Guarded mutation only.** The operator is the only component that changes workloads, only through policy CRDs, only when armed, bounded per step and per day, audited, reversible.
4. **The advisor cannot act.** No Kubernetes credentials, no mutation RPCs, CRD-whitelisted proposals.
5. **Auth on by default** in the chart; per-cluster tokens stored as hashes; OIDC with signature verification; audit rows HMAC-signed.
6. **Supply chain.** Images are signed with cosign (keyless), ship SPDX SBOM attestations, and are scanned with Trivy on release.

---

## 6. Honest limits

- eBPF telemetry needs Linux ≥ 5.8 with cgroup v2 (the default on current AKS, GKE and EKS node images). User-space stacks rely on frame pointers — excellent for Go, Rust and the JVM with `-XX:+PreserveFramePointer`; C/C++ built without frame pointers yields truncated stacks, and JIT frames without perf maps show as `[unknown]`.
- Allocation is an estimate reconciled to node prices (list or pricing-engine), not your cloud invoice; commitments and discounts are not yet replayed onto allocations.
- KubeHero keeps Prometheus as the metrics TSDB (it exposes metrics, ships dashboards and rules) and does not store distributed traces; trace ids in logs link out.

---

## 7. What we are explicitly NOT building

- A general-purpose metrics database — Prometheus / Mimir do that well; KubeHero integrates.
- A tracing backend (yet) — OTLP traces are a direction, not a feature.
- Automation that isn't armed by a human. Guardrails first, always.
