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

- Apache 2.0 — CLI, collector, cost-model, proto.
- BSL 1.1 — control-plane, operator, pricing-engine, advisor, dashboard (source-available, self-hostable, free to run).

The structure:

```
kubehero-platform/
├── apps/
│   ├── dashboard/          # Next.js 15 — app.kubehero.io
│   └── docs/               # Next.js 15 + Fumadocs — docs.kubehero.io
├── services/
│   ├── control-plane/      # Go — API server (Connect-RPC), policy engine, anomaly detection
│   ├── collector/          # Go — DaemonSet: measured utilisation + cost ingest
│   ├── pricing-engine/     # Go — AWS/GCP/Azure pricing catalog + discounts
│   ├── operator/           # Go — controller-runtime, enforces CRDs
│   └── advisor/            # Go — agentic layer: briefings + proposed guarded actions
├── cli/
│   └── kubehero/           # Go — the operator's command-line tool
├── packages/
│   ├── proto/              # Protobuf / Connect schemas (Go + TS generated)
│   ├── tsconfig/           # Shared TypeScript configs (dashboard + docs)
│   └── cost-model/         # Canonical cost calculation library (Go)
├── deploy/
│   ├── helm/               # Helm charts (full stack + federated collector)
│   └── terraform/          # Reference infra (GKE + Cloud SQL + ClickHouse)
└── infra/
    └── dagger/             # CI as code (Dagger.io, language-agnostic pipelines)
```

---

## 2. System diagram

```
┌─────────────────────────── CUSTOMER CLUSTER (any K8s 1.28+) ───────────────────────────┐
│                                                                                          │
│   ┌─────────────────┐    ┌─────────────────┐    ┌──────────────────────┐                │
│   │  Collector      │    │  Operator       │    │  Apps (customer's)   │                │
│   │  DaemonSet      │    │  Deployment     │    │  Deployments/STS/... │                │
│   │  ──────────     │    │  ──────────     │    │                      │                │
│   │  pod/node scan  │    │  watches CRDs   │    │                      │                │
│   │  kubelet stats  │    │  enforces kill  │    │                      │                │
│   │  cost-model $   │    │  switch tiers   │    │                      │                │
│   │  (eBPF: future) │    │  audits actions │    │                      │                │
│   └────────┬────────┘    └──────▲──────────┘    └──────────────────────┘                │
│            │ Connect-RPC        │ K8s API                                                │
│            │ (5s cost batches)  │                                                        │
└────────────┼────────────────────┼────────────────────────────────────────────────────────┘
             │                    │
             │                    │     ┌───────────── DASHBOARD + CLI ────────────┐
             │                    │     │  Next.js @ app.kubehero.io               │
             │                    │     │  `kubehero` CLI binary                   │
             │                    │     └──────▲──────────────────────▲────────────┘
             │                    │            │ Connect-RPC          │ GetBriefing /
             │                    │            │                      │ ListAdvice
             ▼                    │            ▼                      ▼
      ┌───────────────────────────┴──────────────┐         ┌──────────────────────┐
      │  Control Plane API (Go, Connect-RPC)     │         │  Advisor             │
      │  ──────────────────────────────────      │  read   │  (Go, Connect-RPC)   │
      │  ingest → ClickHouse · metadata → PG     │  RPCs   │  ──────────────────  │
      │  z-score anomaly detection · burn rate   │◄────────│  brains: llm → rules │
      │  auth: OIDC (Dex) / API keys             │  only   │  (demo w/o a CP)     │
      └───┬──────────────▲───────────────┬───────┘         │  guardrail-validated │
          │              │ Quote RPC     │                 │  CRD proposals       │
          ▼              │               ▼                 └──────────────────────┘
   ┌──────────────┐   ┌──┴───────────┐   ┌──────────────┐
   │  ClickHouse  │   │  Pricing Eng │   │  PostgreSQL  │
   │  (time-series│   │  live cloud  │   │  metadata    │
   │   at scale)  │   │  APIs with a │   │  policies    │
   └──────────────┘   │  static tbl  │   │  audit log   │
                      └──────────────┘   └──────────────┘
```

---

## 3. Component detail

### 3.1 Collector (DaemonSet, Go)

The telemetry and attribution layer. One service fills the role earlier drafts of this doc split into a kernel "agent" plus a separate ingress tier — the collector is both.

**What it measures today**: every 5 seconds the collector lists pods and nodes from the kube API and blends resource requests with **measured utilisation from the kubelet Summary API** (`/api/v1/nodes/{node}/proxy/stats/summary` — `cpu.usageNanoCores` and `memory.workingSetBytes`, the number the OOM killer actually cares about). Cost per pod is `max(requests, measured)` × the node's hourly rate, computed by `packages/cost-model` so the collector, control plane, and CLI all price a pod identically. Where kubelet stats are unavailable (RBAC, Windows kubelets), it degrades to a request-based estimate for that node's pods — never crashes.

**eBPF is future work — being honest about that.** The plan is still scheduler-attached probes ([`cilium/ebpf`](https://github.com/cilium/ebpf)) for 1-second, cgroup-accurate burst visibility; the build-tag stubs sit in `probes_linux.go` waiting for them. Until they land, the numbers are the kubelet's own measurements — real utilisation, not request-based guesses, just at the kubelet's aggregation resolution rather than per-tick kernel truth. GPU (DCGM/MIG) and TPU telemetry ride the same roadmap.

**Emission**: cost batches ship to the control plane over Connect-RPC each scan, stamped with the cluster id and authenticated with the per-cluster token minted by `kubehero cluster add`. Per-node overhead target: **< 0.5% CPU, < 50 MB RSS**.

**RBAC**: strict read-only — list pods/nodes plus `get` on `nodes/proxy` for the kubelet summaries. Never has write perms on customer resources. Enforcement is done exclusively by the Operator pod, under a separate ServiceAccount whose permissions are opt-in per `RightsizingPolicy`.

### 3.2 Control Plane API (Go, Connect-RPC)

- **Transport**: [Connect-RPC](https://connectrpc.com/) (not plain gRPC). Why: same `.proto` files work from Go → TS via `connect-es`, no gRPC-Web transcoding headache, works over plain HTTP/2 through any load balancer.
- **Stores**: PostgreSQL for metadata (users, orgs, clusters, policies, audit), ClickHouse for time-series. No attempt to do both in one DB — TimescaleDB was considered and rejected at expected data volumes (billions of data points/day at scale).
- **Auth**: 
  - Cloud: [Clerk](https://clerk.com/) or [WorkOS](https://workos.com/) — SSO-friendly, cheap for B2B SaaS.
  - Self-hosted: Dex as OIDC proxy to customer's IdP.
- **Audit**: every policy evaluation, every enforcement action, every rightsizing recommendation writes to `audit_log` table. Append-only. Exported to customer's SIEM via syslog/Webhook.
- **Anomaly detection**: real rolling z-scores, not vibes — each workload's current spend bucket is scored against its trailing baseline (default |z| ≥ 3, tunable via `KUBEHERO_ANOMALY_Z_THRESHOLD`). Series with too little baseline are reported unscoreable rather than guessed at.
- **Demo mode**: RPCs without a backing store fall back to built-in demo fixtures so the stack demos out of the box. Set `KUBEHERO_DEMO_MODE=false` in production to fail loudly (`FailedPrecondition`) instead of serving fake data.

### 3.3 Operator (Go, controller-runtime)

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

### 3.4 Advisor (Go, Connect-RPC)

The agentic layer. It reads the control plane's telemetry and answers the question the raw dashboards don't: *what changed, why, and what should I do about it?* Output is a briefing — a headline, a full markdown report, a TTS-ready spoken script — plus a set of **proposed** actions, each carrying a ready-to-apply policy CRD manifest. Served over Connect-RPC (`GetBriefing`, `ListAdvice`, default `:8083`) to the dashboard's Advisor page and the CLI.

**Three brains, one contract.** Chosen at startup, degrading gracefully:

- **llm** — Claude via `anthropic-sdk-go`, active when `ANTHROPIC_API_KEY` is set. Any failure — API down, timeout, unparseable output — falls back to rules, so briefings never 500 because Anthropic is down.
- **rules** — deterministic: same snapshot in, same briefing out. The offline default and what the tests exercise.
- **demo** — with no `CONTROL_PLANE_URL` the advisor runs over a built-in fixture and stamps every briefing `source: "demo"`, so nobody mistakes it for real data.

**The guardrail is the point.** Every proposed action passes the same validation regardless of which brain produced it: action kinds are whitelisted (`rightsize.requests`, `ceiling.arm`, `nodepool.consolidate`, `workload.investigate`), impact numbers must be finite and ≥ 0, and the attached `crd_yaml` must parse to a `BudgetPolicy` / `CeilingPolicy` / `RightsizingPolicy`. Anything else is downgraded to an investigate-only proposal with no manifest — never silently passed through as something applyable.

**Read-only by design.** The advisor's only inputs are the control plane's read RPCs (`ListClusters`, `ListWasteRecommendations`, `ListAnomalies`, `GetTeamSpend`, `GetBurnRate`). It never talks to the Kubernetes API and never calls mutation RPCs. Applying a proposal is a human act, through the operator's existing arming flow. Advisor proposes · humans arm · operator executes.

**Cache**: briefings are held in a 10-minute in-memory TTL cache per cluster + window, so repeated dashboard loads don't re-bill the LLM tier.

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
7. **Advisor** — the daily briefing (with voice playback via the browser's SpeechSynthesis API — zero external TTS deps) + the proposed-action queue
8. **Org settings** — SSO, RBAC, integrations

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

Serves per-SKU quotes (`Quote` RPC) from a layered catalog: an in-memory cache first, then the cloud's **live pricing API** on a miss, then the static built-in table when the live source fails or isn't wired:

- AWS EC2 public pricing + Spot (AWS Pricing API)
- GCP Compute pricing (Cloud Billing Catalog — needs `GCP_BILLING_API_KEY`; static table otherwise)
- Azure VM pricing + Spot (Azure Retail Prices API)

A quote never fails because a cloud API is down — it just gets staler. Normalizes to a canonical cost-per-second-per-pod given a node's actual SKU and the pod's share of that node's resources.

**Non-obvious detail**: we need to handle *mid-month reservations*. If a customer buys a 1-year Savings Plan mid-month, all attributed cost-per-pod for covered usage drops retroactively in the UI. This is where most cost tools quietly fail.

---

## 4. Deployment modes

### 4.1 Full stack (single cluster)

- Single `helm install kubehero kubehero/kubehero` installs everything: Collector + Control Plane + PG + ClickHouse + Operator + Advisor + Dashboard.
- Air-gap capable — all images mirrorable to your registry, no phone-home.
- No tiers, no gating: every CRD, the CLI, the dashboard, SSO, RBAC, and audit export are all included.

### 4.2 Federated collector (multi-cluster)

- Run only the **Collector DaemonSet** in a workload cluster and point it at a control plane you operate in a hub cluster.
- Collector authenticates with a per-cluster credential (issued via `kubehero cluster add`).
- Telemetry streams to your own hub endpoint — nothing leaves infrastructure you control.
- Same CRDs, same CLI, same dashboard across every registered cluster.

---

## 5. Security & trust

Non-negotiable commitments:

1. **No telemetry leaves your cluster.** Ever. Even "anonymous product analytics" require explicit opt-in.
2. **Collector is read-only by default.** Enforcement requires a separate `RightsizingPolicy` CRD you apply yourself. Nothing can push changes into your cluster from outside.
3. **All policy actions are reversible within cooldown** (default 10m). Every eviction logs the pod spec so it can be restored.
4. **mTLS end-to-end** for collector ↔ ingest telemetry. Certs rotated weekly. `cert-manager` handles it automatically.
5. **Audit log append-only**, exportable to any SIEM via syslog, webhook, or S3 dump.
6. **Runs inside your own compliance boundary.** Everything is self-hosted, so KubeHero inherits your cluster's controls — there's no third-party data processor to certify. The one optional exception is the advisor's LLM tier: set `ANTHROPIC_API_KEY` and briefing inputs (the telemetry snapshot) go to Anthropic's API. Leave it unset and nothing ever leaves — the deterministic rules brain makes no external calls.
7. **The advisor cannot act.** It has no Kubernetes credentials and no mutation RPCs. Every LLM-proposed action is validated against a CRD whitelist and downgraded to investigate-only if it doesn't conform; applying anything still goes through the human arming flow.

---

## 6. Roadmap

Shipped today: AKS / GKE / EKS support; the collector with measured utilisation from the kubelet Summary API; the control plane with z-score spend-anomaly detection, burn rate, and an explicit demo-mode flag (`KUBEHERO_DEMO_MODE=false` fails loudly instead of serving fixtures); the operator with reversible enforcement; live cloud pricing in the serve path with a static-catalog fallback; the advisor (llm / rules / demo briefing tiers behind one guardrail); the CLI; the dashboard with voice briefings; SSO/RBAC/audit; the Helm chart; and reference Terraform for GKE + Cloud SQL + ClickHouse.

Directions we're exploring next (undated, contributions welcome):

- eBPF probes for 1-second, cgroup-accurate burst attribution (the collector's build-tag stubs are waiting)
- GPU/TPU telemetry (DCGM / MIG / `libtpu`)
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
