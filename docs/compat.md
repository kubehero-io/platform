# Compatibility endpoints: Loki, OpenTelemetry, Pyroscope

KubeHero's control plane speaks the wire protocols existing agents and
Grafana already use, so you can point them at KubeHero without changing
them. Everything lands in the same ClickHouse store the KubeHero
collector writes to, and is queryable with LogQL in the dashboard, the
CLI, the `LogsService` RPCs — or Grafana's Loki datasource.

| Endpoint | Protocol | Role needed |
|---|---|---|
| `POST /loki/api/v1/push` | Loki push (snappy protobuf or JSON, optional gzip) | member |
| `GET/POST /loki/api/v1/query_range`, `/query`, `/labels`, `/label/{name}/values`, `/series`, `/index/volume`, `/index/stats` | Loki query API | viewer |
| `GET /loki/api/v1/status/buildinfo`, `GET /ready` | Loki health/version | none |
| `POST /v1/logs` | OTLP/HTTP logs (protobuf or JSON, optional gzip) | member |
| `POST /ingest` | Pyroscope ingest (folded, lines, pprof, multipart) | member |

All of them are served on the control plane's HTTP port (8080 by
default): in-cluster that is
`http://kubehero-control-plane.kubehero-system.svc:8080`.

## Authentication

The same credentials as the Connect RPCs, checked by the same code:

- `Authorization: Bearer <token>` with a static API key
  (`KUBEHERO_API_KEYS`, e.g. `mykey:member`), a cluster enrollment token
  from `RegisterCluster`, or an OIDC JWT;
- `Authorization: Basic …` with the token as the **password** (username
  ignored) — for Grafana datasources and shippers that only do basic
  auth;
- no header at all only when the control plane allows anonymous access
  (`KUBEHERO_REQUIRE_AUTH` unset and no auth configured — dev only).

Pushing needs the `member` role, querying `viewer`. Enrollment tokens are
`member` and scoped to their cluster (see below).

## Which cluster data belongs to

KubeHero partitions everything by cluster. A push is attributed to, in
order:

1. the stream's own `cluster` label (Loki), `k8s.cluster.name` resource
   attribute (OTLP) or `cluster` tag (Pyroscope);
2. the `X-Scope-OrgID` header (Loki's tenant header — Promtail
   `tenant_id`, Alloy `tenant_id`, Fluent Bit `tenant_id`);
3. the cluster of the enrollment token used to authenticate;
4. `KUBEHERO_COMPAT_DEFAULT_CLUSTER` (default `default`).

An enrollment token can only write its own cluster: naming another one
is refused with 403. On the query API, `X-Scope-OrgID` scopes queries to
one cluster (multi-tenant `a|b` is not supported).

## Loki

### Push

```sh
curl -sS -X POST http://localhost:8080/loki/api/v1/push \
  -H "Authorization: Bearer $KUBEHERO_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Scope-OrgID: eks-use1-prod" \
  --data "{\"streams\":[{\"stream\":{\"namespace\":\"shop\",\"pod\":\"api-1\",\"app\":\"api\",\"level\":\"error\"},
           \"values\":[[\"$(date +%s)000000000\",\"payment gateway timeout\",{\"trace_id\":\"4bf92f3577b34da6a3ce929d0e0e4736\"}]]}]}"
```

Promtail's snappy-compressed protobuf (`application/x-protobuf`) is the
default and decoded natively. Answers: `204` accepted, `400` malformed,
`401/403` auth, `413` over 16 MiB (64 MiB decoded), `429`/`503` back off
and retry (Promtail and Alloy do this automatically).

How Loki labels map onto KubeHero's log columns:

| KubeHero | Loki / Promtail / OTel-style label |
|---|---|
| namespace | `namespace`, `k8s_namespace_name`, `namespace_name` |
| pod | `pod`, `pod_name`, `k8s_pod_name` |
| container | `container`, `container_name`, `k8s_container_name` |
| node | `node`, `node_name`, `k8s_node_name` |
| workload (+ kind) | `workload`, `k8s_deployment_name` (Deployment), `k8s_statefulset_name`, `k8s_daemonset_name`, `k8s_cronjob_name`, `k8s_job_name`, else `app` / `app_kubernetes_io_name` / `service_name` |
| level | `level`, `detected_level`, `severity`, `lvl` (normalised to trace/debug/info/warn/error/fatal) |
| stream, team, cluster | `stream`, `team`, `cluster` |
| trace_id | structured metadata `trace_id` / `traceID` |

Every other label and structured-metadata key is kept as an extra label
(names sanitised to `[a-zA-Z_][a-zA-Z0-9_]*`, e.g. `app.kubernetes.io/name`
→ `app_kubernetes_io_name`; at most 32 per line).

**Promtail**

```yaml
clients:
  - url: http://kubehero-control-plane.kubehero-system.svc:8080/loki/api/v1/push
    bearer_token: ${KUBEHERO_TOKEN}
    tenant_id: eks-use1-prod        # → cluster
```

**Grafana Alloy**

```alloy
loki.write "kubehero" {
  endpoint {
    url          = "http://kubehero-control-plane.kubehero-system.svc:8080/loki/api/v1/push"
    bearer_token = sys.env("KUBEHERO_TOKEN")
    tenant_id    = "eks-use1-prod"
  }
}
```

**Fluent Bit**

```ini
[OUTPUT]
    Name        loki
    Match       kube.*
    Host        kubehero-control-plane.kubehero-system.svc
    Port        8080
    Tenant_ID   eks-use1-prod
    Http_User   fluent-bit
    Http_Passwd ${KUBEHERO_TOKEN}
    Labels      job=fluent-bit
    Label_Keys  $kubernetes['namespace_name'],$kubernetes['pod_name'],$kubernetes['container_name']
```

### Query API (Grafana's Loki datasource)

Add a **Loki** datasource with URL
`http://kubehero-control-plane.kubehero-system.svc:8080`, an HTTP header
`Authorization: Bearer <viewer token>` (or basic auth with the token as
password) and optionally `X-Scope-OrgID: <cluster>`. "Save & test" runs
`vector(1)+vector(1)` and works as with Loki. Explore, dashboards, the
log volume histogram and label autocomplete use:

```sh
H="Authorization: Bearer $KUBEHERO_TOKEN"
curl -sS -H "$H" "http://localhost:8080/loki/api/v1/query_range" \
  --data-urlencode 'query={namespace="shop"} | logfmt | status >= 500' \
  --data-urlencode 'since=1h' --data-urlencode 'limit=50' -G
curl -sS -H "$H" "http://localhost:8080/loki/api/v1/query_range" -G \
  --data-urlencode 'query=sum by (level) (count_over_time({namespace="shop"}[5m]))' \
  --data-urlencode 'step=60'
curl -sS -H "$H" "http://localhost:8080/loki/api/v1/labels?since=6h"
curl -sS -H "$H" "http://localhost:8080/loki/api/v1/label/namespace/values"
curl -sS -H "$H" -G "http://localhost:8080/loki/api/v1/series" --data-urlencode 'match[]={app="api"}'
```

Responses use Loki's exact shapes (`streams` values are
`["<unix ns>", "<line>"]`, `matrix`/`vector` samples are
`[<unix seconds>, "<value>"]`); times accept RFC3339, unix seconds or
unix nanoseconds, steps a duration or seconds — as Loki does.

LogQL support: stream selectors, line filters (`|= != |~ !~`, `or`),
`json`, `logfmt`, `regexp`, `pattern`, `unpack`, label filters (numbers,
durations, bytes; `and`/`or`), `line_format`, `label_format`, `drop`,
`keep`, `decolorize`; `count_over_time`, `rate`, `bytes_over_time`,
`bytes_rate`, `absent_over_time`; `sum avg min max count stddev stdvar
topk bottomk` with `by`/`without`; arithmetic, comparison (`bool`) and
set operators with `on`/`ignoring`; `vector()`.
Not supported: `unwrap` (and the `*_over_time` functions that need it),
`group_left`/`group_right`, the WebSocket `/tail` endpoint (use
`LogsService.TailLogs` or `kubehero logs -f`), Loki 3's
`detected_*`/`patterns` endpoints (use `GetLogPatterns`).
`/index/volume` groups by one label (the first of `targetLabels`, else the
selector's first equality matcher).

## OpenTelemetry (OTLP/HTTP logs)

`POST /v1/logs` accepts `ExportLogsServiceRequest` as protobuf
(`application/x-protobuf`) or OTLP/JSON (`application/json`, hex trace
ids). Resource attributes `k8s.namespace.name`, `k8s.pod.name`,
`k8s.container.name`, `k8s.node.name`, `k8s.deployment.name` /
`k8s.statefulset.name` / `k8s.daemonset.name` / `k8s.job.name` /
`k8s.cronjob.name` (else `service.name`) and `k8s.cluster.name` fill the
columns; `severity_number` (or `severity_text`) the level; `trace_id` is
stored as hex; other resource and record attributes become labels
(`service.version` → `service_version`). Rejected records are reported in
`partial_success`.

```sh
curl -sS -X POST http://localhost:8080/v1/logs \
  -H "Authorization: Bearer $KUBEHERO_TOKEN" -H "Content-Type: application/json" \
  --data "{\"resourceLogs\":[{\"resource\":{\"attributes\":[
     {\"key\":\"k8s.namespace.name\",\"value\":{\"stringValue\":\"shop\"}},
     {\"key\":\"service.name\",\"value\":{\"stringValue\":\"checkout\"}}]},
   \"scopeLogs\":[{\"logRecords\":[{\"timeUnixNano\":\"$(date +%s)000000000\",
     \"severityNumber\":17,\"body\":{\"stringValue\":\"payment failed\"}}]}]}]}"
```

**OpenTelemetry Collector**

```yaml
exporters:
  otlphttp/kubehero:
    logs_endpoint: http://kubehero-control-plane.kubehero-system.svc:8080/v1/logs
    headers:
      Authorization: "Bearer ${env:KUBEHERO_TOKEN}"
      X-Scope-OrgID: eks-use1-prod
service:
  pipelines:
    logs:
      receivers: [filelog]
      processors: [k8sattributes, batch]
      exporters: [otlphttp/kubehero]
```

**Grafana Alloy**

```alloy
otelcol.exporter.otlphttp "kubehero" {
  client {
    endpoint = "http://kubehero-control-plane.kubehero-system.svc:8080"
    headers  = { "Authorization" = "Bearer " + sys.env("KUBEHERO_TOKEN") }
  }
}
```

**Fluent Bit**

```ini
[OUTPUT]
    Name        opentelemetry
    Match       *
    Host        kubehero-control-plane.kubehero-system.svc
    Port        8080
    Logs_uri    /v1/logs
    Header      Authorization Bearer ${KUBEHERO_TOKEN}
```

## Pyroscope

`POST /ingest?name=<app>.<type>{k=v,…}&from=<unix>&until=<unix>&format=…&sampleRate=…`
with `format` `folded` (default: `frame;frame;frame count` per line),
`lines` (one stack per line) or `pprof` (raw, optionally gzipped, or the
SDKs' multipart form with `profile`, `prev_profile` and
`sample_type_config` — cumulative types are stored as `profile −
prev_profile`). Each pprof sample type becomes its own profile type
(`cpu`, `wall`, `alloc_space`, `inuse_objects`, `goroutines`, `mutex`,
`block`, …); folded CPU sample counts are converted to nanoseconds at
`sampleRate`. Tags `namespace`, `pod`, `container`, `node` fill the
source; `cluster` picks the cluster; the rest become labels.

```sh
printf 'main;handler;json.Marshal 30\nmain;handler;db.Query 12\n' |
curl -sS -X POST "http://localhost:8080/ingest?name=checkout.cpu%7Bnamespace%3Dshop%7D&from=$(date +%s)&sampleRate=100&format=folded" \
  -H "Authorization: Bearer $KUBEHERO_TOKEN" --data-binary @-

curl -sS -X POST "http://localhost:8080/ingest?name=checkout.cpu&format=pprof" \
  -H "Authorization: Bearer $KUBEHERO_TOKEN" --data-binary @cpu.pb.gz
```

Pyroscope SDKs: set the server address to the control plane and send
the token (e.g. pyroscope-go `ServerAddress:
"http://kubehero-control-plane.kubehero-system.svc:8080"`, basic auth
with the token as password, `TenantID` = cluster).

## Limits and semantics (all write paths)

- Lines longer than 64 KiB are cut and labelled `truncated="true"`.
- Timestamps more than 10 minutes in the future, or older than the
  table's retention (logs/profiles 14 d, flows 30 d, usage 35 d,
  events 90 d), are dropped and counted.
- Accepted rows are buffered and written in large batches about once a
  second; a full buffer answers 429 (`ResourceExhausted` on the RPCs).
  `KUBEHERO_INGEST_LOG_BUFFER_MB` (default 256) sizes the log buffer.
- `KUBEHERO_LOG_USD_PER_GB` (default 0.50) prices log volume in
  `GetLogVolume.est_cost_usd_month` (bytes per day × 30).
