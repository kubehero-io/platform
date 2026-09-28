-- KubeHero 0.3 — exact LogQL windows from the log volume rollup, and
-- skip indexes for the selectors Grafana and the UI send most.
--
-- LogQL range vectors cover (t - range, t], left-open and right-closed.
-- log_volume_1m buckets are [minute, minute + 60s), so a window built
-- from whole buckets is [t - range, t) and differs only by the lines
-- stamped exactly on the two boundary instants. Second-precision
-- sources (Loki push clients, many OTLP SDKs) put one line in sixty
-- exactly on a minute boundary, so the difference is real. edge_lines
-- and edge_bytes count the lines stamped exactly at the bucket start,
-- which lets the logs engine answer (t - range, t] from the rollup:
--   sum of lines over buckets [t - range, t - 60s]
--   minus edge_lines of bucket t - range, plus edge_lines of bucket t
--
-- Rules for this file (the migration runner splits on semicolons):
-- no semicolons inside comments or string literals.

ALTER TABLE log_volume_1m ADD COLUMN IF NOT EXISTS edge_lines SimpleAggregateFunction(sum, UInt64) DEFAULT 0;
ALTER TABLE log_volume_1m ADD COLUMN IF NOT EXISTS edge_bytes SimpleAggregateFunction(sum, UInt64) DEFAULT 0;

-- Recreate the rollup view with the edge counters (same name, same
-- target). Logs are new in 0.3, so no rows predate this view.
DROP VIEW IF EXISTS log_volume_1m_mv;

CREATE MATERIALIZED VIEW IF NOT EXISTS log_volume_1m_mv
TO log_volume_1m AS
SELECT
    toStartOfMinute(ts)                                      AS ts_minute,
    org_id, cluster_id, namespace, workload, container, team, level,
    count()                                                  AS lines,
    sum(toUInt64(length(body)))                              AS bytes,
    countIf(ts = toStartOfMinute(ts))                        AS edge_lines,
    sumIf(toUInt64(length(body)), ts = toStartOfMinute(ts))  AS edge_bytes
FROM logs
GROUP BY ts_minute, org_id, cluster_id, namespace, workload, container, team, level;

-- {level="error"} is the most common filter and level is not in the
-- sort key: a set index lets granules without errors be skipped.
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_level level TYPE set(16) GRANULARITY 4;

-- Extra stream labels ({app="checkout"}) live in the labels map:
-- bloom filters over keys and values prune granules for them.
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_label_keys mapKeys(labels) TYPE bloom_filter(0.01) GRANULARITY 1;
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_label_values mapValues(labels) TYPE bloom_filter(0.01) GRANULARITY 1;
