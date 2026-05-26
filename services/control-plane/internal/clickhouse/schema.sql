-- KubeHero time-series store — ClickHouse.
-- Target: billions of pod-second samples; 1s resolution; 90-day hot window
-- then tier to object storage via the s3() disk.

-- ─── Raw pod-second samples ──────────────────────────────────────────────
-- One row per pod per second. The collector emits these via Connect-RPC
-- and the control-plane batches them into this table.
CREATE TABLE IF NOT EXISTS pod_cost_1s (
    ts                Int64  CODEC(DoubleDelta, ZSTD(1)), -- unix ms
    org_id            LowCardinality(String),
    cluster_id        LowCardinality(String),
    node              LowCardinality(String),
    namespace         LowCardinality(String),
    pod               String,
    team              LowCardinality(String),
    cost_center       LowCardinality(String),
    nodepool          LowCardinality(String),
    cloud             LowCardinality(String),
    region            LowCardinality(String),
    sku               LowCardinality(String),
    lifecycle         LowCardinality(String), -- on-demand · spot · savings-plan · committed
    gpu_kind          LowCardinality(String),
    cpu_millicores    UInt32,
    mem_bytes         UInt64,
    gpu_util_pct      Float32,
    cost_usd_sec      Float64 CODEC(Gorilla, ZSTD(1)),
    recoverable_usd_sec Float64 CODEC(Gorilla, ZSTD(1))
) ENGINE = MergeTree()
  PARTITION BY toYYYYMMDD(toDateTime(ts/1000))
  ORDER BY (org_id, cluster_id, namespace, pod, ts)
  TTL toDateTime(ts/1000) + INTERVAL 90 DAY
  SETTINGS index_granularity = 8192;

-- ─── Rollups — recomputed by materialized views, fast to query ────────────
CREATE TABLE IF NOT EXISTS pod_cost_1m (
    ts_minute         DateTime,
    org_id            LowCardinality(String),
    cluster_id        LowCardinality(String),
    namespace         LowCardinality(String),
    pod               String,
    team              LowCardinality(String),
    cost_center       LowCardinality(String),
    nodepool          LowCardinality(String),
    cloud             LowCardinality(String),
    region            LowCardinality(String),
    gpu_kind          LowCardinality(String),
    cost_usd          AggregateFunction(sum, Float64),
    recoverable_usd   AggregateFunction(sum, Float64),
    cpu_millicores    AggregateFunction(avg, UInt32),
    mem_bytes         AggregateFunction(avg, UInt64),
    gpu_util_pct      AggregateFunction(avg, Float32)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_minute)
  ORDER BY (org_id, cluster_id, team, namespace, pod, ts_minute)
  TTL ts_minute + INTERVAL 365 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS pod_cost_1m_mv
TO pod_cost_1m AS
SELECT
    toStartOfMinute(toDateTime(ts/1000))   AS ts_minute,
    org_id, cluster_id, namespace, pod,
    team, cost_center, nodepool, cloud, region, gpu_kind,
    sumState(cost_usd_sec)                 AS cost_usd,
    sumState(recoverable_usd_sec)          AS recoverable_usd,
    avgState(cpu_millicores)               AS cpu_millicores,
    avgState(mem_bytes)                    AS mem_bytes,
    avgState(gpu_util_pct)                 AS gpu_util_pct
FROM pod_cost_1s
GROUP BY ts_minute, org_id, cluster_id, namespace, pod,
         team, cost_center, nodepool, cloud, region, gpu_kind;

-- Team-hour rollup — what dashboards hit every second.
CREATE TABLE IF NOT EXISTS team_cost_1h (
    ts_hour      DateTime,
    org_id       LowCardinality(String),
    team         LowCardinality(String),
    cost_usd     AggregateFunction(sum, Float64),
    recoverable_usd AggregateFunction(sum, Float64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_hour)
  ORDER BY (org_id, team, ts_hour)
  TTL ts_hour + INTERVAL 400 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS team_cost_1h_mv
TO team_cost_1h AS
SELECT
    toStartOfHour(toDateTime(ts/1000))     AS ts_hour,
    org_id, team,
    sumState(cost_usd_sec)                 AS cost_usd,
    sumState(recoverable_usd_sec)          AS recoverable_usd
FROM pod_cost_1s
GROUP BY ts_hour, org_id, team;

-- Nodepool-hour rollup.
CREATE TABLE IF NOT EXISTS nodepool_cost_1h (
    ts_hour      DateTime,
    org_id       LowCardinality(String),
    cluster_id   LowCardinality(String),
    nodepool     LowCardinality(String),
    cloud        LowCardinality(String),
    region       LowCardinality(String),
    cost_usd     AggregateFunction(sum, Float64)
) ENGINE = AggregatingMergeTree()
  PARTITION BY toYYYYMM(ts_hour)
  ORDER BY (org_id, cluster_id, nodepool, ts_hour);

CREATE MATERIALIZED VIEW IF NOT EXISTS nodepool_cost_1h_mv
TO nodepool_cost_1h AS
SELECT
    toStartOfHour(toDateTime(ts/1000))     AS ts_hour,
    org_id, cluster_id, nodepool, cloud, region,
    sumState(cost_usd_sec)                 AS cost_usd
FROM pod_cost_1s
GROUP BY ts_hour, org_id, cluster_id, nodepool, cloud, region;

-- ─── Node inventory (current state) ──────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_inventory (
    ts                Int64,
    org_id            LowCardinality(String),
    cluster_id        LowCardinality(String),
    node              String,
    cloud             LowCardinality(String),
    region            LowCardinality(String),
    nodepool          LowCardinality(String),
    sku               LowCardinality(String),
    lifecycle         LowCardinality(String),
    cpu_millicores    UInt32, -- allocatable
    mem_bytes         UInt64, -- allocatable
    gpu_kind          LowCardinality(String),
    gpu_count         UInt8,
    price_per_hour    Float64
) ENGINE = ReplacingMergeTree(ts)
  ORDER BY (org_id, cluster_id, node);

-- ─── Policy events — audit trail mirror for fast timeline queries ────────
CREATE TABLE IF NOT EXISTS policy_events (
    ts           DateTime,
    org_id       LowCardinality(String),
    cluster_id   LowCardinality(String),
    policy_kind  LowCardinality(String),
    policy_name  String,
    kind         LowCardinality(String), -- armed · disarmed · triggered · applied · reverted
    actor        String,
    detail       String,
    savings_usd_mo Float64,
    audit_id     String
) ENGINE = MergeTree()
  PARTITION BY toYYYYMM(ts)
  ORDER BY (org_id, cluster_id, ts);
