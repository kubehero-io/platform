# Alerting

One alerting engine covers every signal KubeHero stores. Rules are evaluated on
the control plane; notifications go out through the same channel grammar as
policy escalations. API: `kubehero.v1.AlertsService` (reads: viewer;
`TestAlertRule`: member; mutations: admin).

## Rule kinds and queries

| Kind | Query | Value |
|---|---|---|
| `logs` | LogQL metric query: `sum by (namespace) (count_over_time({level="error"}[5m]))` | the last point of each series |
| `cost` | `cost{namespace="ml-inference"}`, `cost by (team)` | spend rate in $/hour over the last 15 minutes (`cost{…}[30m]` to change the window) |
| `network` | `network{namespace="edge", egress="true"} by (workload)` | network spend rate in $/hour over 15 minutes, each flow pair counted once |
| `event` | `events{kind="oom_killed", namespace="prod"}[10m] by (workload)` | number of cluster events in the window (default 10m, ≤ 24h) |
| `anomaly` | `anomaly{kind="spend"}` (`spend`, `capacity`, `logs`) | $/month impact (spend) or exposure (OOM bursts, log spikes), one series per anomaly |
| `budget` | a BudgetPolicy name, `*` for all, optional window: `prod-monthly[1h]` | burn-rate multiple: monthly-equivalent spend over the window ÷ the policy's monthly ceiling (1.0 = exactly on budget) |

Grammar for the non-LogQL kinds:

```
metric [ '{' label op "value" (',' …)* '}' ] [ '[' duration ']' ] [ by '(' label, … ')' ]
```

`op` is `=`, `!=`, `=~`, `!~` (regexes are RE2, fully anchored). Labels are
whitelisted per metric:

- `cost`: cluster, namespace, workload, workload_kind, team, cost_center,
  nodepool, zone, region, cloud, node, pod, lifecycle
- `network`: cluster, namespace (source), workload (source), zone, dst_namespace,
  dst_workload, dst_kind, egress (`true`/`false`), cross_zone (`true`/`false`)
- `events`: kind, cluster, namespace, workload, pod, container, node, reason,
  severity
- `anomaly`: kind, cluster, namespace, workload, severity (no range or `by`)

`cluster="…"` matches a cluster by id, slug or name. Budget scope: the policy's
own cluster, narrowed to namespaces named through `kubernetes.io/metadata.name`
in its `namespaceSelector`; a single-namespace budget is read through the
operator's burn-rate provider, anything wider with the same math directly.
`logs` rules need the logs engine; `budget` rules need Postgres (the policy
mirror). A rule whose source is unavailable reports the error (in
`TestAlertRule`, and once in the log) and never changes alert state.

Each rule has `op` (`> >= < <= == !=`) and `threshold`, `pending_for`
(0–24h), `eval_interval` (15s–1h, default 1m), `severity` (`info`, `warn`,
`critical`), up to 20 `labels` and 10 `annotations`. `summary`, `description`
and any annotation may use `{{ $value }}`, `{{ $labels.<name> }}` and
`{{ $value | printf "%.2f" }}` — a fixed substitution, not a template language.

## Lifecycle

Per (rule, series):

```
inactive ── condition true ──▶ pending ── held pending_for ──▶ firing ── condition false ──▶ resolved
    ▲                              │                                                          │
    └──── condition false ─────────┘                          kept 24h, then dropped ◀───────┘
```

- A series missing from an evaluation counts as false.
- An evaluation that fails (store down, parse error) changes nothing — a
  ClickHouse outage never "resolves" every alert.
- Evaluations run on a worker pool; first runs are spread across each rule's
  interval and later ones jittered (±5 %, ≤ 5 s). With Postgres, a session
  advisory lock elects one evaluating replica.

## Notifications

- **firing** — once, when an alert starts firing; repeated every **4 h** while it
  keeps firing. If every delivery fails, the next evaluation retries.
- **resolved** — sent only for alerts whose firing was delivered (and then even
  under a silence created later, so paging incidents close).
- **Alertmanager** channels receive every evaluation of a firing alert with
  `endsAt = now + 4 × interval` (Alertmanager expires alerts that stop being
  re-sent) and `endsAt = resolve time` on resolve.
- **Silences** (label matchers, `alertname` = rule name; ≤ 90 days) suppress
  notifications while active; alert state still advances. Deleting a silence
  expires it.
- Rules without channels are UI-only.
- Every delivery attempt is logged per channel in `alert_notifications`
  (channel redacted), pruned after 30 days.

Set `KUBEHERO_DASHBOARD_URL` (e.g. `https://kubehero.example.com`) to put a
deep link in notifications.

### Channels

| Scheme | Target |
|---|---|
| `slack://hooks.slack.com/services/…` | Slack incoming webhook (Block Kit) |
| `pagerduty://<routing-key>` | PagerDuty Events v2; resolves by dedup key |
| `opsgenie://<api-key>[?team=…&priority=P2&region=eu]` | OpsGenie; closes by alias on resolve |
| `webhook+https://…` / `webhook+http://…` | generic JSON (below); URL userinfo = basic auth |
| `teams://…` / `teams+https://…` | Microsoft Teams Workflows webhook, Adaptive Card |
| `discord://…` / `discord+https://discord.com/api/webhooks/…` | Discord embed, mentions disabled |
| `alertmanager+https://am:9093` / `alertmanager+http://…` | Alertmanager `POST /api/v2/alerts` |

Generic webhook body (version 1):

```json
{"version":"1","status":"firing","title":"[FIRING] High spend — shop at $12/h",
 "body":"…","severity":"critical","source":"kubehero/alert/alrt-…","url":"https://…",
 "labels":{"alertname":"High spend","namespace":"shop","severity":"critical"},
 "annotations":{"summary":"…","description":"…"},"fields":{"value":"12"},
 "startsAt":"2026-09-30T12:00:00Z","endsAt":"2026-09-30T12:04:00Z"}
```

Channel URLs are credentials: the API shows them in full to admins only
(`slack://hooks.slack.com/…` to everyone else), accepts the redacted form back
on update (keeping the stored secret), and no error or log line contains one.
`http` variants are for in-cluster receivers; plaintext is an explicit choice.

## Default rules

Seeded once, on the first boot that finds no rules (deleting them does not bring
them back). They are enabled with **no channels**: alerts appear in the UI and
page nobody until an admin adds channels.

| Rule | Kind | Query | Condition | Pending | Severity |
|---|---|---|---|---|---|
| Error log spike | logs | `sum by (cluster, namespace, workload) (count_over_time({level=~"error\|fatal"}[5m]))` | > 100 | 5m | warn |
| OOM kills | event | `events{kind="oom_killed"}[15m] by (cluster, namespace, workload)` | ≥ 3 | — | warn |
| Spend anomaly over $1k/mo | anomaly | `anomaly{kind="spend"}` | > 1000 | — | warn |
| Budget burn over 1.5x | budget | `*[1h]` | > 1.5 | 15m | critical |
| Internet egress over $5/h | network | `network{egress="true"} by (cluster, namespace, workload)` | > 5 | 15m | warn |

## Storage

With `DATABASE_URL`, Postgres migration `0003_alerts` holds `alert_rules`,
`alert_states`, `alert_silences`, `alert_notifications` and `alert_meta` (the
seed marker). Without Postgres, rules, state and silences live in memory and are
lost on restart (logged at startup). Limits: 1000 rules, 500 series per rule
evaluation, 10 channels per rule.
