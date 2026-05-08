-- 000008_snmp_walk_requests.sql
--
-- Operator-triggered "walk now" queue. The api inserts one row per
-- request; the snmp scheduler reads max(requested_at) per exporter
-- on every dispatch tick and walks any exporter whose latest request
-- post-dates the last completed walk. TTL drops old rows so the table
-- stays small.
--
-- Append-only, no replacement semantics. The scheduler relies on
-- max(requested_at) > lastWalked[exporter] to decide whether to fire,
-- so duplicate inserts from a chatty operator are harmless.

CREATE TABLE IF NOT EXISTS snmp_walk_requests
(
    exporter      IPv6 CODEC(ZSTD(1)),
    requested_at  DateTime64(3, 'UTC'),
    requested_by  LowCardinality(String) DEFAULT 'anonymous'
)
ENGINE = MergeTree
ORDER BY (exporter, requested_at)
TTL toDateTime(requested_at) + INTERVAL 1 DAY
SETTINGS index_granularity = 8192;
