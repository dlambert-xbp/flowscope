-- 000006_exporter_health.sql
--
-- Per-exporter ingest lossiness signal.
--
-- The ingest service tracks the datagram sequence number on every
-- NetFlow v9 / IPFIX / sFlow packet it receives. Gaps in the seq
-- number indicate lost UDP datagrams between the exporter and us
-- — the only sampling-loss signal we can compute without exporter-
-- side cooperation.
--
-- A per-(exporter, source) tracker accumulates {datagrams, gaps}
-- counts in memory and flushes a row here every 10 seconds. The
-- Overview "Exporter accuracy" panel and /api/health/exporters
-- aggregate over the trailing window for a per-exporter loss %.

CREATE TABLE IF NOT EXISTS exporter_health (
    ts          DateTime64(3, 'UTC'),
    exporter    IPv6,
    source      LowCardinality(String),
    datagrams   UInt64,
    seq_gaps    UInt64,
    last_seq    UInt32
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (exporter, source, ts)
TTL toDateTime(ts) + INTERVAL 7 DAY
SETTINGS index_granularity = 8192;
