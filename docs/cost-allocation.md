# Cost allocation, rightsizing and exports

The control plane's `CostService` is an OpenCost-class allocation engine over the
ClickHouse cost plane. The same numbers are served three ways:

| Surface | Path | Auth |
|---|---|---|
| Connect RPC | `kubehero.v1.CostService/*` (`GetAllocation`, `GetCostTimeseries`, `ListRightsizing`, `GetEfficiency`) | viewer |
| OpenCost-compatible HTTP | `GET /allocation/compute`, `/allocation` (aliases `/model/allocation[/compute]`) | viewer |
| FinOps FOCUS 1.2 CSV | `GET /api/v1/export/focus` | viewer |

The HTTP routes accept exactly the credentials the Connect API does (API keys,
OIDC bearer tokens, cluster enrollment tokens, anonymous only in dev mode).
Credentials minted for one cluster (collector/operator enrollment tokens) can
only read that cluster.

Without ClickHouse every surface serves clearly labelled demo data
(`source: "demo"`, OpenCost `warning` + `X-KubeHero-Source: demo`); with
`KUBEHERO_DEMO_MODE=false` they fail instead (`FailedPrecondition` / HTTP 503).

All money is USD. "Per month" always means a 30-day month (720 hours), the same
convention as burn rate and anomaly impact.

## Windows

| Window | Meaning (UTC) |
|---|---|
| `30m`, `24h` | rolling: the last N minutes/hours |
| `1d`, `7d`, `2w` | OpenCost semantics: today plus the N−1 whole days before it (ends at tomorrow's midnight; data up to now) |
| `today`, `yesterday` | calendar days |
| `week`, `lastweek` | week-to-date / the previous full week (weeks start Sunday) |
| `month`, `lastmonth` | month-to-date / the previous calendar month |
| `2026-09-01T00:00:00Z,2026-09-08T00:00:00Z` | explicit RFC3339 pair (unix-seconds pairs work too) |

Windows are capped at 400 days (the hourly rollups' retention).

## Allocation

**Dimensions** (`aggregate`, composite keys joined with `/`): `cluster`,
`namespace`, `workload`, `controller` (`<Kind>/<name>`), `pod`, `container`,
`node`, `nodepool`, `team`, `cost_center`, `zone`, `label:<key>`, plus `cloud`,
`region`, `lifecycle`, `workload_kind`. Empty values render as `__unallocated__`
(which also works as a filter value). **Filters** use the same names; a comma
separates alternatives (`namespace: "checkout,edge"`).

**Query plan — exact to the second.** Whole hours come from the
`workload_cost_1h` rollup; the partial hours at the edges of a window that isn't
hour-aligned come from raw `pod_cost_1s`, so a window's total equals the sum of
`cost_usd_sec × interval_sec` of the samples inside it. Pod- and node-grained
requests (the rollup has neither column) and windows under two hours read
`pod_cost_1s` throughout. The ClickHouse server must run in UTC (or a whole-hour
offset) so rollup hours line up with UTC hours.

**Per row**: CPU/RAM/GPU cost, core-hours and byte-hours (billed basis:
`max(request, usage)`), request and usage averages, efficiencies, network cost,
shared cost, idle cost, recoverable cost and log ingest GB.

- *Efficiency* follows OpenCost: `usage / request` (0 with no usage, 1 with usage
  but no request); `total = (cpuEff·cpu$ + ramEff·ram$) / (cpu$ + ram$)`.
- *Network cost*: egress and cross-zone $ from `net_flows_1h`, attributed to the
  **source** workload. Pod→pod traffic is recorded at both ends; each
  (source, destination) pair is counted once — the receiver's ingress record when
  the destination is a pod, the sender's egress record otherwise, falling back to
  whichever side was observed.
- *Log ingest*: bytes from `log_volume_1m`, attributed to the workload.
- Network and log volume are spread across a workload's rows (nodepools, zones…)
  in proportion to their compute cost.
- *Containers*: cost tables are pod-grained, so `container` splits each pod's CPU
  side by the containers' CPU-request share and memory side by memory-request
  share (from `container_usage_5m`); everything else follows the resulting cost
  share. Workloads without container data stay `__unallocated__`.
- *Recoverable* on allocation rows is the collector's per-sample estimate
  (requested − used, priced). `ListRightsizing`/`GetEfficiency` use the
  percentile engine below.
- Collectors that predate the per-resource cost split report only total cost;
  the unsplit remainder is assigned 50/50 to CPU and RAM, mirroring the cost
  model's 50/50 CPU-share/memory-share blend.

**Idle and shared costs**, per cluster:

```
T_c   = compute $ of non-shared namespaces   (unfiltered)
S_c   = compute $ of shared namespaces       (unfiltered)
Idle_c = max(0, node $ (node_cost_1h) − T_c − S_c)

shared:    row.shared = S_c · row.cost / T_c
weighted:  row.idle   = Idle_c · row.cost / T_c
even:      Idle_c · F_c / T_c split equally over the result's rows in c
separate:  one __idle__ row per cluster worth Idle_c · F_c / T_c
```

`F_c` is the cost of the rows that survived the filters (`= T_c` unfiltered):
with filters only their proportional share of idle and shared cost is shown
(OpenCost's idle filtration). Idle can't go negative: when usage bursts above
requests push allocation past node cost, idle is 0. A cluster whose only cost is
in shared namespaces keeps it as a `<cluster>/__shared__` row.

**Limits**: at most 5 aggregate dimensions (3 labels), 10 filters, and 200k
fine-grained rows per query (`ResourceExhausted` beyond — narrow the window or
filter). Every query has a 30 s timeout.

## OpenCost-compatible API

`GET /allocation/compute?window=7d&aggregate=namespace&accumulate=true`

Parameters: `window` (required), `aggregate` (`cluster`, `node`, `namespace`,
`controllerKind`, `controller`, `pod`, `container`, `label:<key>`, and
KubeHero's `team`, `nodepool`, `zone`, `workload`, `cost_center`; none =
OpenCost's per-container key), `accumulate` (default `false`: one set per day;
`step=6h` etc. overrides), `includeIdle` / `idle`, `shareIdle` + `shareSplit`
(`weighted`|`even`), `shareNamespaces`, and the filter family
`filterClusters`, `filterNodes`, `filterNamespaces`, `filterControllerKinds`,
`filterControllers` (`deployment:api`), `filterPods`, `filterContainers`,
`filterLabels` (`app:web`). OpenCost's v2 `filter=` language is rejected with
400 rather than silently ignored.

The response is OpenCost's envelope and `AllocationJSON` field for field
(`{"code":200,"status":"success","data":[{"<name>":{…}}]}`): controllers are
keyed `kind:name`, idle is `__idle__` (or `<cluster>/__idle__` when aggregating
by cluster), shared idle is folded into `cpuCost`/`ramCost`/`gpuCost` and reported
in `*CostIdle`, numbers are rounded to six decimals, NaN/Inf are `null`.
Quantities KubeHero doesn't collect — persistent volumes, load balancers,
resource limits, network bytes — are `0`, never invented.

## FOCUS 1.2 export

`GET /api/v1/export/focus?window=30d&aggregate=workload`
(`aggregate`: `workload` | `namespace` | `cluster`; `idle`: `separate` (default)
| `share` | `none`; `shareNamespaces`; `cluster_id`)

One row per UTC day per resource, streamed a day at a time (windows ≤ 366 days);
network spend is a separate row with `ServiceCategory = Networking`. A failure
mid-export aborts the connection instead of leaving a silently truncated file.

| Column(s) | Value | Nature |
|---|---|---|
| `BilledCost`, `EffectiveCost`, `ListCost`, `ContractedCost` | allocated cost (node price × the pod's `max(request, usage)` share) | **allocated estimate** — KubeHero is not the invoice issuer; the four are equal because the node price KubeHero sees is already the effective rate. Reconcile with the cloud bill upstream. |
| `InvoiceIssuerName` | `KubeHero (allocated)` | marks the rows as allocations, never an invoice |
| `ConsumedQuantity`/`PricingQuantity`, `ConsumedUnit`/`PricingUnit` | allocated core-hours, `Core-Hours` | estimate (billed basis `max(request, usage)`) |
| `PricingCategory` | node lifecycle: on-demand → `Standard`, spot → `Dynamic`, savings plan / committed → `Committed` | exact when the collector reports lifecycle |
| `BillingCurrency` | `USD` | exact |
| `ChargePeriodStart/End`, `BillingPeriodStart/End` | the UTC day; its calendar month | exact |
| `ChargeCategory`, `ChargeClass`, `ChargeFrequency` | `Usage`, empty, `Usage-Based` | exact |
| `ProviderName` | `AWS`, `Google Cloud`, `Microsoft Azure`, else `Kubernetes` | exact |
| `PublisherName` | `KubeHero` | exact |
| `RegionId`, `RegionName`, `AvailabilityZone` | node region (id used as name) and zone | exact |
| `ResourceId`, `ResourceName`, `ResourceType` | `cluster/namespace/workload`, name, `Kubernetes Workload`/`Namespace`/`Cluster` (`Kubernetes Idle Capacity` for idle rows) | exact |
| `ServiceCategory`, `ServiceName` | `Compute` / `Kubernetes` (`Networking` / `Kubernetes Network`) | exact |
| `SubAccountId`, `SubAccountName` | cluster id / registered name | exact |
| `Tags` | JSON: `k8s.cluster`, `k8s.namespace`, `k8s.workload`, `k8s.workload.kind`, `kubehero.io/team`, `kubehero.io/cost-center`, plus the workload's pod labels (≤ 50) | exact |
| `x_CostSource` | `KubeHero allocated estimate` (or `KubeHero demo data`) | — |
| `x_CpuCost`, `x_RamCost`, `x_GpuCost`, `x_NetworkCost`, `x_IdleCost`, `x_SharedCost` | the allocation's components | estimates, as above |
| `x_CpuEfficiency`, `x_RamEfficiency`, `x_CpuCoreHours`, `x_RamGiBHours`, `x_GpuHours`, `x_RecoverableCost` | KubeHero extensions | measured / estimated |

## Time series and forecast

`GetCostTimeseries` returns allocated spend (compute + network; idle excluded —
see `include_idle`) per step (`1h` for windows ≤ 48 h, else `1d` on UTC
midnights), optionally grouped (`namespace`, `team`, `cluster`, `nodepool`,
`workload`, `cost_center`, `zone`, `cloud`, `region`, `lifecycle`, `controller`)
with the `top` N (default 10, max 50) groups kept and the rest folded into
`other`. Series are dense (zero-filled).

`forecast_month_usd = month-to-date spend + (trailing 7 × 24 h spend / 7) ×
remaining days in the month`, with the same filters.

## Rightsizing (`ListRightsizing`)

Per (cluster, namespace, workload, container) over the window (default `7d`,
≤ 90 d) from `container_usage_5m` t-digests and `cluster_events`:

- **CPU** = chosen percentile (`p95` default; `p90`, `p99`, `max`) ×
  (1 + headroom, default 15 %), floored at 10m, rounded up to 5m.
- **Memory** = max(p99, observed max) × (1 + headroom), floored at 32 Mi,
  rounded up to 1 Mi — never below the observed maximum.
- **OOM guard**: any OOM kill in the window keeps memory at or above the current
  request and raises it to 1.25 × max(observed max, current limit).
- **Confidence** by observed history: < 1 day of 5-minute buckets `low`,
  < 5 days `medium`, else `high`.
- **Direction**: `upsize` if any resource must grow by > 10 % (risk first),
  else `downsize` if any can shrink by > 10 %, else `ok` (hidden by default).
- **Cost impact** uses the workload's own billed prices from `workload_cost_1h`
  (`cpu_cost_usd / billed core-hours`, `ram_cost_usd / billed GiB-hours`) × average
  running replicas × 720 h; a container using more than it requests is priced at
  its median usage, which is what it's billed for.
- **CPU throttle risk** = share of 5-minute buckets where p99 usage ≥ 90 % of the
  CPU limit.

Results are cached for 60 s per scope (overview, team spend and efficiency share
them). `ListWasteRecommendations` / `GetWorkload` roll the same engine up per
workload (downsizes with positive savings; signal like `cpu.req=2.0 p95=0.31`).

## Efficiency score (`GetEfficiency`)

For the fleet, each cluster and each namespace:

```
blend = (min(cpuEff, 1) · cpu$ + min(ramEff, 1) · ram$) / (cpu$ + ram$)
score = 100 · blend · (1 − idle$ / (allocated$ + idle$))
```

i.e. the share of every compute dollar — including dollars paid for idle node
capacity — that does measured work. Usage above requests is capped at 1 (it is
risk, not extra efficiency); namespaces carry no idle, so their score is the
blend alone. `recoverable_usd_month` is the sum of positive downsize savings from
the rightsizing engine over the same window (at least one day).

## Team spend, capacity demands, anomalies

- `GetTeamSpend`: allocated compute + network spend per (team, cost center)
  scaled to a month and split by cloud; recoverable from rightsizing (each
  workload credited to the team that carried most of its cost); GPU idle =
  GPU cost × (1 − utilisation) only for workloads whose GPU utilisation is ever
  reported above 0 — without telemetry nothing is claimed idle.
- `ListCapacityDemands`: pods reported `unschedulable` in the last 30 minutes
  that have not appeared in `pod_cost_1s` since, grouped per workload, priced
  with the cheapest node type seen in the cluster (last 6 h of `node_cost_1s`)
  that fits one pod; blocked cost = requested cores × that type's per-core price.
- `ListAnomalies` adds OOM-kill bursts (≥ 3 in an hour, kind `capacity`) and
  error-log spikes (z-score of the last hour vs the trailing baseline from
  `log_volume_1m`, ≥ 50 lines, kind `logs`) to the spend z-scores; their dollar
  figure is the workload's run-rate spend — exposure, not loss.
