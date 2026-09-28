# Changelog

Every release, newest first. The same notes, with more context, are at
[kubehero.io/changelog](https://kubehero.io/changelog). Images are published
to `ghcr.io/kubehero-io/<image>:<version>` and the chart to
`oci://ghcr.io/kubehero-io/charts/kubehero`, all signed with cosign.

## v0.3.0 — 2026-09-28

**Every signal, every dollar, one agent.** v0.3 turns KubeHero from a cost
tool into one install for the signals teams usually spread across five:
cost allocation, logs, continuous profiling, eBPF network cost and alerting.
Rightsizing now actually adjusts, and read-only agents tie it all together.
It also fixes the bugs that inflated spend on multi-node clusters. Upgrade
notes are at the end.

### Cost allocation (OpenCost-compatible)

- Allocation by any dimension: cluster, namespace, workload, controller, pod,
  container, node, nodepool, team, cost center, zone or `label:<key>`. Idle
  rows, idle sharing (weighted or even) and shared-namespace redistribution.
- Per-resource cost: CPU, RAM and GPU split by the cost model (GPU nodes
  attribute a documented share of the price to accelerators), plus network
  cost from eBPF flows and log volume per row.
- OpenCost-compatible `/allocation/compute` and `/allocation`. OpenCost and
  Kubecost API consumers, and the dashboards built on them, work unchanged.
- FinOps FOCUS 1.2 CSV export at `/api/v1/export/focus`, with allocated
  estimates labelled as such.
- Cost time series with a month-end forecast, chargeback by team and cost
  center, and a fleet efficiency score.

### Logs (Loki-class)

- The collector tails `/var/log/pods` on every node into ClickHouse. It
  handles CRI and docker json-file, reassembles partial lines, keeps
  rotation- and truncation-safe checkpoints, detects level and trace id, and
  rate-limits per container.
- LogQL: stream selectors, line filters, `json` / `logfmt` / `regexp`
  parsers, label filters, `line_format`, `drop` / `keep`, and metric queries
  (`count_over_time`, `rate`, `bytes_over_time`, `bytes_rate`,
  `absent_over_time` with `sum` / `avg` / `min` / `max` / `count` / `topk` /
  `bottomk` by or without), compiled to parameterised ClickHouse SQL.
- Live tail, Drain pattern mining, and log-volume cost per team.
- Drop-in Loki push (JSON and protobuf+snappy) and query APIs: Promtail,
  Grafana Alloy, Fluent Bit and Grafana's Loki datasource work unchanged.
  OTLP/HTTP logs at `/v1/logs`.

### Continuous profiling (Pyroscope-class)

- eBPF whole-node CPU profiling at 49 Hz: kernel and user stacks for every
  container, symbolised from ELF symbol tables and Go's pclntab. No code
  changes, no restarts.
- pprof scraping for pods annotated the Pyroscope/Alloy way
  (`profiles.grafana.com/*`) or with `kubehero.io/profile*`.
- A Pyroscope-compatible `/ingest` endpoint for SDK pushes (pprof and folded).
- Flamegraphs, diff flamegraphs against a baseline window, top functions, and
  $/month per function from the workload's CPU spend.

### eBPF network

- `cgroup_skb` ingress and egress programs account bytes and packets per
  endpoint pair; TCP retransmits come from the `tcp:tcp_retransmit_skb`
  tracepoint. The programs only observe: traffic is never dropped or altered.
- A workload service map with bytes, retransmits and cost per edge, and
  per-workload network spend.
- Cross-zone and internet egress priced per GB, with per-cloud defaults you
  can override.

### Rightsizing that adjusts

- Per-container p50 / p95 / p99 / max from 5-minute usage rollups. CPU is
  sized to a percentile plus headroom; memory never below the observed max
  and never down after an OOM kill; confidence comes from sample coverage;
  savings are priced from the workload's own cost.
- `RightsizingPolicy` now acts. `recommend` writes recommendations to status;
  `shadow` records and audits what it would change; `apply` patches requests
  only after a human arms the policy, with bounded steps, `maxChangePerDay`,
  a confidence floor, and never mid-rollout or on a container a VPA owns.
- The previous resources are recorded so `kubehero undo` restores them.

### Alerting

- One evaluator over logs (LogQL metric queries), spend rate, budget burn,
  anomalies, network spend and cluster events: OOM kills, crash loops,
  unschedulable pods.
- pending → firing → resolved with templated annotations, silences and
  re-notification. Test a rule before you save it.
- Channels: Slack, PagerDuty, Opsgenie, Microsoft Teams, Discord, generic
  webhooks and Alertmanager.
- A default rule set on first boot: error spikes, OOM kills, spend anomalies,
  budget burn and egress.

### Agents

- Ask KubeHero: investigations run a bounded, read-only tool loop over
  allocation, anomalies, logs and patterns, profiles, the service map,
  network costs, alerts and rightsizing, and answer with cited evidence, deep
  links and guarded proposals.
- Claude as the brain with your own Anthropic API key (Claude Opus 5 by
  default, set `KUBEHERO_ADVISOR_MODEL` to change it), and a deterministic
  rules brain as the offline default and the fallback on any error. Without a
  key, nothing leaves the cluster.
- `kubehero mcp`: an MCP server (stdio or streamable HTTP) exposing the same
  read-only tools to Claude and other agents.
- Every proposal from any brain is validated against the CRD whitelist;
  anything else is downgraded to investigate-only. The advisor holds no
  Kubernetes credentials.

### CLI, dashboard and chart

- CLI: `kubehero cost`, `logs` (`-f` live tail, `--patterns`),
  `profile top | flame`, `network`, `rightsize`, `alerts`, `ask` and `mcp`.
- Dashboard on Next.js 16: logs, profiles, network, allocation, rightsizing,
  alerts and Ask explorers, plus a per-workload correlation page. Token
  sign-in with per-user roles.
- Helm chart 0.3.0: embedded single-replica Postgres 18 and ClickHouse 26.8
  LTS by default (managed stores via a DSN Secret for production), generated
  and persisted tokens with auth required by default, network policies and
  restricted security contexts throughout.

### Fixes

- **Per-node collector attribution.** Every collector pod listed all pods in
  the cluster and fetched every node's kubelet summary, so an N-node cluster
  reported N× its real cost (and made N² kubelet calls). Each collector now
  reports only its own node; cluster-scoped scans run on an elected leader.
- **Spend priced as rate × interval.** Samples arrive every few seconds, but
  rollups summed per-second rates as if every row covered one second.
  Rollups and the burn rate now use `cost_usd_sec × interval_sec`.
- **A ClickHouse schema that actually applies.** Versioned migrations,
  applied once and recorded, and a control plane that waits for its stores
  instead of quietly serving demo data.
- **Enrollment tokens are verified.** A cluster token now authenticates its
  own cluster only, and can't write telemetry for another.
- **Public images and chart**, tagged to the app version. A Helm install now
  wires the stores into the control plane, so it runs on real data instead of
  demo fixtures.

### Upgrade notes

- Migrations run automatically on control-plane start (ClickHouse
  `0002_signals` and `0003_logs_exact_volume`, and the new Postgres
  migrations). `control-plane migrate` runs them on their own.
- The collector DaemonSet gains host mounts (`/var/log/pods` read-only,
  `/var/lib/kubehero`, the cgroup v2 filesystem) and, with eBPF on, runs
  privileged with `hostPID`. Set `collector.ebpf.enabled=false` to run it
  unprivileged without the kernel signals.
- Auth is required by default: read the admin token from the
  `kubehero-control-plane` Secret.
- The Bitnami Postgres / ClickHouse subcharts are gone: use the embedded
  stores, or point `postgresql.external` / `clickhouse.external` at your own.
- Coming from 0.1.x: v0.2.0 was a source milestone and never shipped images
  or a chart. Everything in it (the advisor image included) ships here.

## v0.2.0 — 2026-07-09 (source only, not tagged)

- New advisor service: agentic briefings (headline, markdown and a spoken
  script) generated by Claude with your Anthropic API key, or by a
  deterministic rules engine without one. Every proposed action is validated
  against the CRD schema; anything else is downgraded to investigate-only.
- Dashboard: Advisor page with voice playback and guarded-CRD action cards.
  Session cookies are HMAC-signed and httpOnly.
- Collector: measured utilization from the kubelet Summary API, blended with
  requests for attribution.
- Pricing engine: live AWS, GCP and Azure catalogs behind a TTL cache with
  request coalescing and stale-then-static fallback.
- Control plane: spend-anomaly detection (rolling z-scores per workload) and
  `KUBEHERO_DEMO_MODE=false` to hard-disable demo fixtures.
- Reference Terraform for GKE.

## v0.1.0 — 2026-05-22

Initial public release: six multi-arch images (control-plane, operator,
collector, pricing-engine, cli, dashboard), cosign-signed with SBOM
attestations, and the Helm chart. The operator reconciles BudgetPolicy,
CeilingPolicy and RightsizingPolicy (Ready / Armed / Tripped) with an HPA-cap,
evict and cordon actuator; the control plane has API-key and OIDC auth; the
CLI covers cluster enrollment, scan, rightsize, apply, `cap --arm`, undo and
quote.
