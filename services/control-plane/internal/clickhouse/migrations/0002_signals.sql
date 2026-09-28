-- KubeHero 0.3 — every signal in one store.
--
-- Adds the tables behind logs, continuous profiling, eBPF network
-- flows, container usage history (rightsizing), cluster events, node
-- cost (idle), and pod metadata (label allocation). Also fixes the
-- spend rollups: the collector samples every few seconds, so a row's
-- dollars are cost_usd_sec × interval_sec, not cost_usd_sec alone.
--
-- Rules for this file (the migration runner splits on semicolons):
-- no semicolons inside comments or string literals.

-- ─── pod_cost_1s: allocation-grade columns ───────────────────────────────
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS interval_sec Float32 DEFAULT 5;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS workload LowCardinality(String) DEFAULT '';
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS workload_kind LowCardinality(String) DEFAULT '';
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS zone LowCardinality(String) DEFAULT '';
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS cpu_usage_millicores UInt32 DEFAULT 0;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS mem_usage_bytes UInt64 DEFAULT 0;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS cpu_cost_usd_sec Float64 DEFAULT 0;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS ram_cost_usd_sec Float64 DEFAULT 0;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS gpu_cost_usd_sec Float64 DEFAULT 0;
ALTER TABLE pod_cost_1s ADD COLUMN IF NOT EXISTS gpu_count UInt8 DEFAULT 0;

-- ─── Spend rollups, rebuilt on rate × interval ───────────────────────────
-- Pre-0.3 builds created rollup views that summed per-second rates
-- straight into "cost". Drop them if a dev database still has them.
DROP VIEW IF EXISTS pod_cost_1m_mv;
DROP VIEW IF EXISTS team_cost_1h_mv;
DROP VIEW IF EXISTS nodepool_cost_1h_mv;

-- workload_cost_1h is what allocation, chargeback, timeseries and
-- burn-rate queries read for any window longer than a few minutes.
CREATE TABLE IF NOT EXISTS workload_cost_1h (
    ts_hour                   DateTime,
    org_id                    LowCardinality(String),
    cluster_id                LowCardinality(String),
    namespace                 LowCardinality(String),
    workload                  LowCardinality(String),
    workload_kind             LowCardinality(String),
    team                      LowCardinality(String),
    cost_center               LowCardinality(String),
    nodepool                  LowCardinality(String),
    cloud                     LowCardinality(String),
    region                    LowCardinality(String),
    zone                      LowCardinality(String),
    lifecycle                 LowCardinality(String),
    gpu_kind                  LowCardinality(String),
    cost_usd                  SimpleAggregateFunction(sum, Float64),
    cpu_cost_usd              SimpleAggregateFunction(sum, Float64),
    ram_cost_usd              SimpleAggregateFunction(sum, Float64),
    gpu_cost_usd              SimpleAggregateFunction(sum, Float64),
    recoverable_usd           SimpleAggregateFunction(sum, Float64),
    cpu_request_core_seconds  SimpleAggregateFunction(sum, Float64),
    cpu_usage_core_seconds    SimpleAggregateFunction(sum, Float64),
    ram_request_byte_seconds  SimpleAggregateFunction(sum, Float64),
    ram_usage_byte_seconds    SimpleAggregateFunction(sum, Float64),
    gpu_seconds               SimpleAggregateFunction(sum, Float64),
    pod_seconds               SimpleAggregateFunction(sum, Float64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_hour)
  ORDER BY (org_id, cluster_id, namespace, workload, workload_kind, team, cost_center,
            nodepool, cloud, region, zone, lifecycle, gpu_kind, ts_hour)
  TTL ts_hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS workload_cost_1h_mv
TO workload_cost_1h AS
SELECT
    toStartOfHour(toDateTime(intDiv(ts, 1000)))            AS ts_hour,
    org_id, cluster_id, namespace, workload, workload_kind, team, cost_center,
    nodepool, cloud, region, zone, lifecycle, gpu_kind,
    sum(cost_usd_sec * interval_sec)                        AS cost_usd,
    sum(cpu_cost_usd_sec * interval_sec)                    AS cpu_cost_usd,
    sum(ram_cost_usd_sec * interval_sec)                    AS ram_cost_usd,
    sum(gpu_cost_usd_sec * interval_sec)                    AS gpu_cost_usd,
    sum(recoverable_usd_sec * interval_sec)                 AS recoverable_usd,
    sum(toFloat64(cpu_millicores) / 1000 * interval_sec)    AS cpu_request_core_seconds,
    sum(toFloat64(cpu_usage_millicores) / 1000 * interval_sec) AS cpu_usage_core_seconds,
    sum(toFloat64(mem_bytes) * interval_sec)                AS ram_request_byte_seconds,
    sum(toFloat64(mem_usage_bytes) * interval_sec)          AS ram_usage_byte_seconds,
    sum(toFloat64(gpu_count) * interval_sec)                AS gpu_seconds,
    sum(toFloat64(interval_sec))                            AS pod_seconds
FROM pod_cost_1s
GROUP BY ts_hour, org_id, cluster_id, namespace, workload, workload_kind, team, cost_center,
         nodepool, cloud, region, zone, lifecycle, gpu_kind;

-- ─── Node cost — the denominator for idle ────────────────────────────────
CREATE TABLE IF NOT EXISTS node_cost_1s (
    ts                          Int64 CODEC(DoubleDelta, ZSTD(1)), -- unix ms
    org_id                      LowCardinality(String),
    cluster_id                  LowCardinality(String),
    node                        String,
    nodepool                    LowCardinality(String),
    cloud                       LowCardinality(String),
    region                      LowCardinality(String),
    zone                        LowCardinality(String),
    sku                         LowCardinality(String),
    lifecycle                   LowCardinality(String),
    gpu_kind                    LowCardinality(String),
    gpu_count                   UInt8,
    interval_sec                Float32 DEFAULT 5,
    price_per_hour              Float64,
    price_source                LowCardinality(String),
    cpu_allocatable_millicores  UInt32,
    mem_allocatable_bytes       UInt64,
    cpu_requested_millicores    UInt32,
    mem_requested_bytes         UInt64,
    cpu_used_millicores         UInt32,
    mem_used_bytes              UInt64,
    cost_usd_sec                Float64 CODEC(Gorilla, ZSTD(1)),
    idle_usd_sec                Float64 CODEC(Gorilla, ZSTD(1))
) ENGINE = MergeTree()
  PARTITION BY toYYYYMMDD(toDateTime(intDiv(ts, 1000)))
  ORDER BY (org_id, cluster_id, node, ts)
  TTL toDateTime(intDiv(ts, 1000)) + INTERVAL 90 DAY;

CREATE TABLE IF NOT EXISTS node_cost_1h (
    ts_hour                       DateTime,
    org_id                        LowCardinality(String),
    cluster_id                    LowCardinality(String),
    node                          String,
    nodepool                      LowCardinality(String),
    cloud                         LowCardinality(String),
    region                        LowCardinality(String),
    zone                          LowCardinality(String),
    sku                           LowCardinality(String),
    lifecycle                     LowCardinality(String),
    cost_usd                      SimpleAggregateFunction(sum, Float64),
    idle_usd                      SimpleAggregateFunction(sum, Float64),
    node_seconds                  SimpleAggregateFunction(sum, Float64),
    cpu_allocatable_core_seconds  SimpleAggregateFunction(sum, Float64),
    ram_allocatable_byte_seconds  SimpleAggregateFunction(sum, Float64),
    cpu_used_core_seconds         SimpleAggregateFunction(sum, Float64),
    ram_used_byte_seconds         SimpleAggregateFunction(sum, Float64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_hour)
  ORDER BY (org_id, cluster_id, node, nodepool, cloud, region, zone, sku, lifecycle, ts_hour)
  TTL ts_hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS node_cost_1h_mv
TO node_cost_1h AS
SELECT
    toStartOfHour(toDateTime(intDiv(ts, 1000)))                  AS ts_hour,
    org_id, cluster_id, node, nodepool, cloud, region, zone, sku, lifecycle,
    sum(cost_usd_sec * interval_sec)                              AS cost_usd,
    sum(idle_usd_sec * interval_sec)                              AS idle_usd,
    sum(toFloat64(interval_sec))                                  AS node_seconds,
    sum(toFloat64(cpu_allocatable_millicores) / 1000 * interval_sec) AS cpu_allocatable_core_seconds,
    sum(toFloat64(mem_allocatable_bytes) * interval_sec)          AS ram_allocatable_byte_seconds,
    sum(toFloat64(cpu_used_millicores) / 1000 * interval_sec)     AS cpu_used_core_seconds,
    sum(toFloat64(mem_used_bytes) * interval_sec)                 AS ram_used_byte_seconds
FROM node_cost_1s
GROUP BY ts_hour, org_id, cluster_id, node, nodepool, cloud, region, zone, sku, lifecycle;

-- ─── Pod metadata — labels for label:<key> allocation ───────────────────
CREATE TABLE IF NOT EXISTS pod_metadata (
    org_id         LowCardinality(String),
    cluster_id     LowCardinality(String),
    namespace      LowCardinality(String),
    pod            String,
    workload       LowCardinality(String),
    workload_kind  LowCardinality(String),
    node           LowCardinality(String),
    team           LowCardinality(String),
    cost_center    LowCardinality(String),
    labels         Map(LowCardinality(String), String),
    updated_at     DateTime
) ENGINE = ReplacingMergeTree(updated_at)
  ORDER BY (org_id, cluster_id, namespace, pod)
  TTL updated_at + INTERVAL 90 DAY;

-- ─── Logs ────────────────────────────────────────────────────────────────
-- One row per line. ORDER BY leads with the stream identity so a
-- LogQL selector prunes granules; the day partition prunes time.
CREATE TABLE IF NOT EXISTS logs (
    ts             DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    org_id         LowCardinality(String),
    cluster_id     LowCardinality(String),
    namespace      LowCardinality(String),
    workload       LowCardinality(String),
    workload_kind  LowCardinality(String),
    pod            String CODEC(ZSTD(1)),
    container      LowCardinality(String),
    node           LowCardinality(String),
    team           LowCardinality(String),
    stream         LowCardinality(String),
    level          LowCardinality(String),
    trace_id       String CODEC(ZSTD(1)),
    labels         Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    body           String CODEC(ZSTD(3)),
    INDEX idx_body_ngram body TYPE ngrambf_v1(3, 65536, 3, 0) GRANULARITY 1,
    INDEX idx_body_token lower(body) TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 1,
    INDEX idx_trace trace_id TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_pod pod TYPE bloom_filter(0.01) GRANULARITY 1
) ENGINE = MergeTree()
  PARTITION BY toDate(ts)
  ORDER BY (org_id, cluster_id, namespace, workload, container, pod, ts)
  TTL toDateTime(ts) + INTERVAL 14 DAY
  SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;

-- Volume per minute: log-volume histograms, log cost attribution and
-- the logs alert fast path never touch raw rows.
CREATE TABLE IF NOT EXISTS log_volume_1m (
    ts_minute   DateTime,
    org_id      LowCardinality(String),
    cluster_id  LowCardinality(String),
    namespace   LowCardinality(String),
    workload    LowCardinality(String),
    container   LowCardinality(String),
    team        LowCardinality(String),
    level       LowCardinality(String),
    lines       SimpleAggregateFunction(sum, UInt64),
    bytes       SimpleAggregateFunction(sum, UInt64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_minute)
  ORDER BY (org_id, cluster_id, namespace, workload, container, team, level, ts_minute)
  TTL ts_minute + INTERVAL 90 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS log_volume_1m_mv
TO log_volume_1m AS
SELECT
    toStartOfMinute(ts)       AS ts_minute,
    org_id, cluster_id, namespace, workload, container, team, level,
    count()                   AS lines,
    sum(toUInt64(length(body))) AS bytes
FROM logs
GROUP BY ts_minute, org_id, cluster_id, namespace, workload, container, team, level;

-- ─── Profiles ────────────────────────────────────────────────────────────
-- Stacks are content-addressed: profile_samples stores only the hash.
CREATE TABLE IF NOT EXISTS profile_stacks (
    stack_hash  UInt64,
    frames      Array(String) CODEC(ZSTD(3)),
    last_seen   DateTime
) ENGINE = ReplacingMergeTree(last_seen)
  ORDER BY stack_hash
  TTL last_seen + INTERVAL 30 DAY;

CREATE TABLE IF NOT EXISTS profile_samples (
    ts           DateTime CODEC(Delta, ZSTD(1)),
    org_id       LowCardinality(String),
    cluster_id   LowCardinality(String),
    service      LowCardinality(String),
    namespace    LowCardinality(String),
    workload     LowCardinality(String),
    pod          String CODEC(ZSTD(1)),
    container    LowCardinality(String),
    node         LowCardinality(String),
    type         LowCardinality(String),
    unit         LowCardinality(String),
    origin       LowCardinality(String),
    labels       Map(LowCardinality(String), String),
    stack_hash   UInt64,
    value        Int64,
    duration_ns  Int64
) ENGINE = MergeTree()
  PARTITION BY toDate(ts)
  ORDER BY (org_id, cluster_id, service, type, ts, stack_hash)
  TTL ts + INTERVAL 14 DAY
  SETTINGS ttl_only_drop_parts = 1;

-- ─── Network flows (eBPF) ────────────────────────────────────────────────
-- cross_zone / egress / cost_usd are decided at ingest from endpoint
-- zones + kinds and the configured $/GB, so queries only sum.
CREATE TABLE IF NOT EXISTS net_flows (
    ts             DateTime CODEC(Delta, ZSTD(1)),
    org_id         LowCardinality(String),
    cluster_id     LowCardinality(String),
    window_sec     UInt16,
    src_kind       LowCardinality(String),
    src_namespace  LowCardinality(String),
    src_workload   LowCardinality(String),
    src_pod        String,
    src_node       LowCardinality(String),
    src_zone       LowCardinality(String),
    src_ip         String,
    src_name       String,
    dst_kind       LowCardinality(String),
    dst_namespace  LowCardinality(String),
    dst_workload   LowCardinality(String),
    dst_pod        String,
    dst_node       LowCardinality(String),
    dst_zone       LowCardinality(String),
    dst_ip         String,
    dst_service    LowCardinality(String),
    dst_name       String,
    port           UInt16,
    protocol       LowCardinality(String),
    direction      LowCardinality(String),
    bytes          UInt64,
    packets        UInt64,
    retransmits    UInt32,
    cross_zone     UInt8,
    egress         UInt8,
    cost_usd       Float64
) ENGINE = MergeTree()
  PARTITION BY toDate(ts)
  ORDER BY (org_id, cluster_id, src_namespace, src_workload, dst_namespace, dst_workload, ts)
  TTL ts + INTERVAL 30 DAY
  SETTINGS ttl_only_drop_parts = 1;

CREATE TABLE IF NOT EXISTS net_flows_1h (
    ts_hour        DateTime,
    org_id         LowCardinality(String),
    cluster_id     LowCardinality(String),
    src_kind       LowCardinality(String),
    src_namespace  LowCardinality(String),
    src_workload   LowCardinality(String),
    src_zone       LowCardinality(String),
    dst_kind       LowCardinality(String),
    dst_namespace  LowCardinality(String),
    dst_workload   LowCardinality(String),
    dst_service    LowCardinality(String),
    dst_name       String,
    dst_zone       LowCardinality(String),
    port           UInt16,
    protocol       LowCardinality(String),
    direction      LowCardinality(String),
    cross_zone     UInt8,
    egress         UInt8,
    bytes          SimpleAggregateFunction(sum, UInt64),
    packets        SimpleAggregateFunction(sum, UInt64),
    retransmits    SimpleAggregateFunction(sum, UInt64),
    cost_usd       SimpleAggregateFunction(sum, Float64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_hour)
  ORDER BY (org_id, cluster_id, src_namespace, src_workload, src_kind, src_zone,
            dst_namespace, dst_workload, dst_kind, dst_service, dst_name, dst_zone,
            port, protocol, direction, cross_zone, egress, ts_hour)
  TTL ts_hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS net_flows_1h_mv
TO net_flows_1h AS
SELECT
    toStartOfHour(ts) AS ts_hour,
    org_id, cluster_id,
    src_kind, src_namespace, src_workload, src_zone,
    dst_kind, dst_namespace, dst_workload, dst_service, dst_name, dst_zone,
    port, protocol, direction, cross_zone, egress,
    sum(bytes)                    AS bytes,
    sum(packets)                  AS packets,
    sum(toUInt64(retransmits))    AS retransmits,
    sum(cost_usd)                 AS cost_usd
FROM net_flows
GROUP BY ts_hour, org_id, cluster_id,
         src_kind, src_namespace, src_workload, src_zone,
         dst_kind, dst_namespace, dst_workload, dst_service, dst_name, dst_zone,
         port, protocol, direction, cross_zone, egress;

-- ─── Container usage — the rightsizing history ───────────────────────────
CREATE TABLE IF NOT EXISTS container_usage (
    ts                        DateTime CODEC(Delta, ZSTD(1)),
    org_id                    LowCardinality(String),
    cluster_id                LowCardinality(String),
    namespace                 LowCardinality(String),
    workload                  LowCardinality(String),
    workload_kind             LowCardinality(String),
    pod                       String CODEC(ZSTD(1)),
    container                 LowCardinality(String),
    node                      LowCardinality(String),
    team                      LowCardinality(String),
    cpu_usage_cores           Float32,
    mem_working_set_bytes     UInt64,
    cpu_request_cores         Float32,
    mem_request_bytes         UInt64,
    cpu_limit_cores           Float32,
    mem_limit_bytes           UInt64,
    restarts                  UInt32,
    last_termination_reason   LowCardinality(String)
) ENGINE = MergeTree()
  PARTITION BY toDate(ts)
  ORDER BY (org_id, cluster_id, namespace, workload, container, ts)
  TTL ts + INTERVAL 35 DAY
  SETTINGS ttl_only_drop_parts = 1;

-- 5-minute t-digest rollup: percentile rightsizing over 7–90 days
-- without scanning raw samples.
CREATE TABLE IF NOT EXISTS container_usage_5m (
    ts_5m          DateTime,
    org_id         LowCardinality(String),
    cluster_id     LowCardinality(String),
    namespace      LowCardinality(String),
    workload       LowCardinality(String),
    workload_kind  LowCardinality(String),
    container      LowCardinality(String),
    cpu_digest     AggregateFunction(quantilesTDigest(0.5, 0.9, 0.95, 0.99), Float32),
    mem_digest     AggregateFunction(quantilesTDigest(0.5, 0.9, 0.95, 0.99), UInt64),
    cpu_max        SimpleAggregateFunction(max, Float32),
    mem_max        SimpleAggregateFunction(max, UInt64),
    cpu_request    SimpleAggregateFunction(max, Float32),
    mem_request    SimpleAggregateFunction(max, UInt64),
    cpu_limit      SimpleAggregateFunction(max, Float32),
    mem_limit      SimpleAggregateFunction(max, UInt64),
    samples        SimpleAggregateFunction(sum, UInt64),
    pods           AggregateFunction(uniq, String),
    max_restarts   SimpleAggregateFunction(max, UInt32)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_5m)
  ORDER BY (org_id, cluster_id, namespace, workload, workload_kind, container, ts_5m)
  TTL ts_5m + INTERVAL 180 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS container_usage_5m_mv
TO container_usage_5m AS
SELECT
    toStartOfFiveMinutes(ts)                                   AS ts_5m,
    org_id, cluster_id, namespace, workload, workload_kind, container,
    quantilesTDigestState(0.5, 0.9, 0.95, 0.99)(cpu_usage_cores)       AS cpu_digest,
    quantilesTDigestState(0.5, 0.9, 0.95, 0.99)(mem_working_set_bytes) AS mem_digest,
    max(cpu_usage_cores)                                       AS cpu_max,
    max(mem_working_set_bytes)                                 AS mem_max,
    max(cpu_request_cores)                                     AS cpu_request,
    max(mem_request_bytes)                                     AS mem_request,
    max(cpu_limit_cores)                                       AS cpu_limit,
    max(mem_limit_bytes)                                       AS mem_limit,
    toUInt64(count())                                          AS samples,
    uniqState(pod)                                             AS pods,
    max(restarts)                                              AS max_restarts
FROM container_usage
GROUP BY ts_5m, org_id, cluster_id, namespace, workload, workload_kind, container;

-- ─── Cluster events ──────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS cluster_events (
    ts          DateTime64(3),
    org_id      LowCardinality(String),
    cluster_id  LowCardinality(String),
    kind        LowCardinality(String),
    severity    LowCardinality(String),
    namespace   LowCardinality(String),
    workload    LowCardinality(String),
    pod         String,
    container   LowCardinality(String),
    node        LowCardinality(String),
    reason      LowCardinality(String),
    message     String,
    attributes  Map(LowCardinality(String), String),
    count       UInt32
) ENGINE = MergeTree()
  PARTITION BY toYYYYMM(ts)
  ORDER BY (org_id, cluster_id, kind, ts)
  TTL toDateTime(ts) + INTERVAL 90 DAY;
