-- 000001_init.sql
--
-- Bootstrap schema for FlowScope on ClickHouse.
--
-- Forward-only and idempotent: every CREATE uses IF NOT EXISTS so the
-- migration runner can replay without harm. New schema changes land as
-- new numbered files; never edit a migration after release.

CREATE TABLE IF NOT EXISTS flows
(
    observed        DateTime64(3, 'UTC'),
    exporter        IPv6 CODEC(ZSTD(1)),
    src_addr        IPv6 CODEC(ZSTD(1)),
    dst_addr        IPv6 CODEC(ZSTD(1)),
    src_port        UInt16,
    dst_port        UInt16,
    proto           UInt8,
    bytes           UInt64,
    packets         UInt64,
    input_ifindex   UInt32,
    output_ifindex  UInt32,
    vlan_id         UInt16,
    tos             UInt8,
    tcp_flags       UInt8,
    source          LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(observed)
ORDER BY (toStartOfMinute(observed), exporter, src_addr, dst_addr)
TTL toDateTime(observed) + INTERVAL 7 DAY
SETTINGS index_granularity = 8192;

-- Counter samples are authoritative for interface throughput
-- (VISION.md §3.3). One row per (exporter, ifindex, ts).
CREATE TABLE IF NOT EXISTS iface_counter_samples
(
    ts              DateTime64(3, 'UTC'),
    exporter        IPv6 CODEC(ZSTD(1)),
    ifindex         UInt32,
    in_octets       UInt64,
    out_octets      UInt64,
    in_packets      UInt64,
    out_packets     UInt64,
    in_errors       UInt64,
    out_errors      UInt64,
    in_discards     UInt64,
    out_discards    UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (exporter, ifindex, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;

-- Generic event log used by the alert engine, exporter discovery,
-- and platform self-monitoring. Free-form labels via Map.
CREATE TABLE IF NOT EXISTS events
(
    ts              DateTime64(3, 'UTC'),
    kind            LowCardinality(String),
    severity        LowCardinality(String),
    scope           String,
    title           String,
    body            String,
    labels          Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (kind, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
