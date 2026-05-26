# KubeHero — Product Architecture

> The public architecture overview for KubeHero, an open-source, self-hosted Kubernetes cost-monitoring platform. This is a high-level map; the implementation lives in this repo.

---

## 0. North Star

**One pane of glass, for every cluster, showing every dollar — with a trigger you can pull when things go wrong.**

We are not building another metrics dashboard. Metrics dashboards are commodity. We are building the *control surface* that an on-call operator actually needs at 3 AM when a bad deploy is spawning 400 GPU nodes.

The three hard problems we solve, in order of priority:

1. **Attribution** — which pod, on which node, in which cluster, belonging to which team, is wasting money *right now*.
2. **Recommendation** — the precise config change that recovers it without breaking SLOs.
3. **Enforcement** — when human action isn't fast enough, the policy engine fires and stops the bleeding.

---

## 1. Repository layout

This monorepo holds everything the product is made of.

- Apache 2.0 — agent, CLI, collector, cost-model, proto.
- BSL 1.1 — control-plane, operator, pricing-engine, dashboard (source-available, self-hostable, free to run).

The structure:

```
kubehero-platform/
├── apps/
│   ├── dashboard/          # Next.js 15 — app.kubehero.io
│   └── docs/               # Starlight or Nextra — docs.kubehero.io
├── services/
│   ├── control-plane/      # Go — API server (Connect-RPC), policy engine
│   ├── collector/          # Go — gRPC ingress for agent telemetry
│   ├── pricing-engine/     # Go — AWS/GCP/Azure pricing catalog + discounts
│   ├── operator/           # Go — controller-runtime, enforces CRDs
│   └── agent/              # Go + eBPF — DaemonSet, kernel-level telemetry
├── cli/
│   └── kubehero/           # Go — the operator's command-line tool
├── packages/
│   ├── proto/              # Protobuf / Connect schemas (Go + TS generated)
│   ├── ui/                 # Shared React components (dashboard + docs)
│   └── cost-model/         # Canonical cost calculation library (Go)
├── deploy/
│   ├── helm/               # Helm charts (full stack + federated agent)
│   └── terraform/          # Reference infra (GKE + ClickHouse)
└── infra/
    └── dagger/             # CI as code (Dagger.io, language-agnostic pipelines)
```

---

## 2. System diagram

```
┌─────────────────────────── CUSTOMER CLUSTER (any K8s 1.28+) ───────────────────────────┐
│                                                                                          │
│   ┌─────────────────┐    ┌─────────────────┐    ┌──────────────────────┐                │
│   │  Agent          │    │  Operator       │    │  Apps (customer's)   │                │
│   │  DaemonSet      │    │  Deployment     │    │  Deployments/STS/... │                │
│   │  ──────────     │    │  ──────────     │    │                      │                │
│   │  eBPF probes    │    │  watches CRDs   │    │                      │                │
│   │  cAdvisor read  │    │  enforces kill  │    │                      │                │
│   │  DCGM (GPU)     │    │  switch tiers   │    │                      │                │
│   │  proto-encodes  │    │  audits actions │    │                      │                │
│   └────────┬────────┘    └──────▲──────────┘    └──────────────────────┘                │
│            │ gRPC (mTLS)        │ K8s API                                                │
│            │                    │                                                        │
└────────────┼────────────────────┼────────────────────────────────────────────────────────┘
             │                    │
             ▼                    │         ┌─────────────────── DASHBOARD + CLI ─────┐
      ┌──────────────┐            │         │  Next.js @ app.kubehero.io              │
      │  Collector   │            │         │  `kubehero` CLI binary                   │
      │  (in-cluster │            │         └───────────▲──────────────────────────────┘
      │   OR cloud)  │            │                     │ Connect-RPC / WebSocket
      └──────┬───────┘            │                     │
             │ batch writes       │                     │
             ▼                    │                     │
      ┌──────────────┐            │         ┌───────────┴──────────────┐
      │  ClickHouse  │            └─────────┤  Control Plane API       │
      │  (time-series│◄───────────────────  │  (Go, Connect-RPC)       │
      │   at scale)  │                      │  ──────────────────      │
      └──────────────┘                      │  reads: ClickHouse + PG  │
             ▲                              │  writes: PG              │
             │                              │  auth: Clerk / Dex       │
      ┌──────┴───────┐                      └──────┬───────────────────┘
      │  Pricing Eng │                             │
      │  (cron → PG) │                             ▼
      │  AWS/GCP/AZ  │                      ┌──────────────┐
      └──────────────┘                      │  PostgreSQL  │
                                            │  metadata    │
                                            │  policies    │
                                            │  audit log   │
                                            └──────────────┘
```

---

## 3. Component detail

### 3.1 Agent (DaemonSet, Go + eBPF)

**Why eBPF?** Traditional `metrics-server` and Prometheus cadvisor scrapes average CPU over a window (usually 15s–1m). That's **useless** for right-sizing — it hides bursts, misattributes noisy-neighbor steal, and can't see per-syscall or I/O pressure. eBPF lets us attach probes to the scheduler, attribute CPU with cgroup-accurate precision, and see memory pressure events as they happen.

**Libraries**: [`cilium/ebpf`](https://github.com/cilium/ebpf) for the BPF loader, [`libbpfgo`](https://github.com/aquasecurity/libbpfgo) as a backup path for older kernels. Fallback to cAdvisor scraping for nodes where BPF is unavailable (e.g. Windows nodes, managed AKS with restricted BPF).

**GPU**: DCGM Exporter scrape → we parse into our own schema. For MIG slices, read `nvidia-smi mig` tree. For AMD (rocM), parallel path via `rocm-smi`.

**TPU**: GCP only. Pull `cloudtpu.googleapis.com` metrics via the GCP SDK + scrape `libtpu` exported metrics.

**Emission**: agents batch 1-second ticks into 5s gRPC frames, LZ4-compressed. Per-node overhead target: **< 0.5% CPU, < 50 MB RSS**. Benchmark against this on every PR.

**RBAC**: strict read-only by default. Never has write perms on customer resources. Enforcement is done exclusively by the Operator pod, under a separate ServiceAccount whose permissions are opt-in per `RightsizingPolicy`.

### 3.2 Collector (Go, gRPC)

Thin ingress service. Authenticates via mTLS (cert-manager in self-hosted, our root CA in cloud, rotated weekly). Deduplicates, aggregates 5s frames into 1min rollups, writes to ClickHouse.

Horizontal by design — stateless, shard by cluster_id. Stateless makes rolling upgrades trivial.

### 3.3 Control Plane API (Go, Connect-RPC)

- **Transport**: [Connect-RPC](https://connectrpc.com/) (not plain gRPC). Why: same `.proto` files work from Go → TS via `connect-es`, no gRPC-Web transcoding headache, works over plain HTTP/2 through any load balancer.
- **Stores**: PostgreSQL for metadata (users, orgs, clusters, policies, audit), ClickHouse for time-series. No attempt to do both in one DB — TimescaleDB was considered and rejected at expected data volumes (billions of data points/day at scale).
- **Auth**: 
  - Cloud: [Clerk](https://clerk.com/) or [WorkOS](https://workos.com/) — SSO-friendly, cheap for B2B SaaS.
  - Self-hosted: Dex as OIDC proxy to customer's IdP.
- **Audit**: every policy evaluation, every enforcement action, every rightsizing recommendation writes to `audit_log` table. Append-only. Exported to customer's SIEM via syslog/Webhook.

### 3.4 Operator (Go, controller-runtime)

Watches three CRDs:

```yaml
# ── Budget: declarative spending intent ──
apiVersion: kubehero.io/v1
kind: Budget
metadata: { name: prod-monthly }
spec:
  scope:
    clusterSelector: { matchLabels: { env: prod } }
    namespaceSelector: { matchExpressions: [...] }
  limits:
    monthly: { amount: 100000, currency: USD }
    hourly: { amount: 300 }
  alerting:
    channels: [slack://ops, pagerduty://prod-p1]
    thresholds: [50, 80, 95, 100]   # percentage of budget

# ── CeilingPolicy: what to do when budget breached ──
apiVersion: kubehero.io/v1
kind: CeilingPolicy
metadata: { name: prod-hard-ceiling }
spec:
  budgetRef: { name: prod-monthly }
  trigger:
    burnRate: 1.5x       # cost growth > 1.5× budgeted rate
    window: 5m           # sustained over 5 minutes
  escalation:
    - { action: hpa.cap, ratio: 0.5, waitAfter: 2m }
    - { action: pod.evict, selector: { priorityClassName: "low" }, waitAfter: 3m }
    - { action: nodepool.cordon, selector: { label: "workload=batch" }, waitAfter: 5m }
    - { action: alert, channels: [slack://ops-oncall] }
  cooldown: 10m
  require:
    humanArm: true       # requires dashboard "arm" toggle before auto-firing

# ── RightsizingPolicy: how aggressively to recommend / auto-apply ──
apiVersion: kubehero.io/v1
kind: RightsizingPolicy
metadata: { name: non-prod-auto }
spec:
  scope:
    namespaceSelector: { matchLabels: { env: dev } }
  mode: automatic        # automatic | suggest | shadow
  safety:
    minReplicas: 1
    p95HeadroomPct: 40
    observationWindow: 14d
    maxChangePerDay: 3
```

The operator is paranoid by design:
- Every action has a **dry-run mode**, enabled by default.
- Hard-stop escalations require `humanArm: true` in spec, unless disabled at org level (requires admin).
- All actions are reversible; every `pod.evict` is logged with the restored pod spec attached, so an operator can `kubehero undo <audit-id>` within the cooldown window.

### 3.5 Dashboard (Next.js 15)

Deployed at `app.kubehero.io` (cloud) or behind the customer's ingress (self-hosted).

Stack:
- Next.js 15 (server components where possible; client for live data panels)
- TanStack Query for async state, TanStack Table for data grids
- **ECharts** for time-series (Recharts is too limited for the density we need; Observable Plot considered for future)
- WebSocket subscription for live metrics (via Connect-RPC streams)
- Same monospace aesthetic as marketing — continuity is a feature

Key screens:
1. **Fleet** — all clusters, at-a-glance health + cost delta
2. **Cluster** — node grid (same visualizer from marketing, but with real data + drill-in)
3. **Waste** — ranked list of recoverable dollars, with one-click fix / rightsize
4. **GPU Panel** — dedicated view for GPU/TPU utilization with per-process breakdown
5. **Budgets** — CRUD for BudgetPolicy CRDs (visual editor that writes YAML)
6. **Ceiling Log** — audit trail of every policy firing, reversible
7. **Org settings** — SSO, RBAC, integrations

### 3.6 CLI (`kubehero`)

A Go binary. Same API as the dashboard. For ops folks who live in a terminal (i.e. our audience).

```bash
kubehero cluster list
kubehero scan --cluster prod-use1 --report waste
kubehero rightsize --apply --dry-run=false vectordb-ingress
kubehero budget apply -f budgets/prod.yaml
kubehero cap --policy prod-hard-ceiling --arm
kubehero undo <audit-id>
```

Distribution: `brew install kubehero`, `apt`, `yum`, single static binary on GitHub Releases.

### 3.7 Pricing Engine

Nightly cron fetches:
- AWS EC2 public pricing + Spot + Savings Plans discounts (published via AWS Pricing API + Spot history)
- GCP Compute pricing + Spot VMs + Committed Use Discounts
- Azure VM pricing + Spot + Reserved Instances

Normalizes to a canonical cost-per-second-per-pod given a node's actual SKU and the pod's share of that node's resources.

**Non-obvious detail**: we need to handle *mid-month reservations*. If a customer buys a 1-year Savings Plan mid-month, all attributed cost-per-pod for covered usage drops retroactively in the UI. This is where most cost tools quietly fail.

---

## 4. Deployment modes

### 4.1 Full stack (single cluster)

- Single `helm install kubehero kubehero/kubehero` installs everything: Agent + Collector + Control Plane + PG + ClickHouse + Operator + Dashboard.
- Air-gap capable — all images mirrorable to your registry, no phone-home.
- No tiers, no gating: every CRD, the CLI, the dashboard, SSO, RBAC, and audit export are all included.

### 4.2 Federated agent (multi-cluster)

- Run only the **Agent DaemonSet** in a workload cluster and point it at a control plane you operate in a hub cluster.
- Agent authenticates with a per-cluster mTLS cert (issued via `kubehero cluster add`).
- Telemetry streams to your own hub endpoint — nothing leaves infrastructure you control.
- Same CRDs, same CLI, same dashboard across every registered cluster.

---

## 5. Security & trust

Non-negotiable commitments:

1. **No telemetry leaves your cluster.** Ever. Even "anonymous product analytics" require explicit opt-in.
2. **Agent is read-only by default.** Enforcement requires a separate `RightsizingPolicy` CRD you apply yourself. Nothing can push changes into your cluster from outside.
3. **All policy actions are reversible within cooldown** (default 10m). Every eviction logs the pod spec so it can be restored.
4. **mTLS end-to-end** for agent ↔ ingest telemetry. Certs rotated weekly. `cert-manager` handles it automatically.
5. **Audit log append-only**, exportable to any SIEM via syslog, webhook, or S3 dump.
6. **Runs inside your own compliance boundary.** Everything is self-hosted, so KubeHero inherits your cluster's controls — there's no third-party data processor to certify.

---

## 6. Roadmap

Shipped today: AKS / GKE / EKS support, the eBPF collector, control plane, operator with reversible enforcement, GPU/TPU telemetry, the CLI, the dashboard, SSO/RBAC/audit, and the Helm chart.

Directions we're exploring next (undated, contributions welcome):

- Multi-cluster federation improvements
- ML-driven rightsizing recommendations
- Budget-aware scheduler plugin
- Serverless-container support (ECS / Cloud Run)

---

## 7. What we are explicitly NOT building

Staying focused is the job. We will not:

- Rebuild Grafana. We expose Prometheus-compatible metrics for anyone who wants custom dashboards. Our dashboard is opinionated, not general.
- Rebuild Datadog APM. Spans, traces, logs — out of scope. Integrate with your existing stack.
- Chase every cloud. AKS/GKE/EKS only for now. Oracle/IBM/Alibaba when someone contributes support.
- Build a CMDB. Reading cluster inventory is a side effect, not a product.
- Automate what shouldn't be automated. Spend ceiling needs `humanArm: true` by default. We optimize for *operators not getting fired*, not for magical self-healing.
