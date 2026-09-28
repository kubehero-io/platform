-- KubeHero time-series store — ClickHouse.
-- Target: billions of pod-second samples at the collector's scan
-- resolution, 90-day hot window. Spend rollups live in 0002.

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
