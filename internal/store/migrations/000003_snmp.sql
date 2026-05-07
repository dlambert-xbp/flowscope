-- 000003_snmp.sql
--
-- Tables that hold SNMP-derived inventory.
--
-- VISION.md §3.1 / §4.2: SNMP is the FALLBACK telemetry path, not a
-- workhorse. We do not poll fleet-wide every five minutes. Instead we
-- walk per-device on a configurable cadence (default 15 min) and on
-- operator-triggered demand. The data here is opinionated, not
-- exhaustive.

-- One row per (exporter, polled_at). The most recent row is the
-- current truth; old rows accumulate as a low-volume history. Use
-- argMax / max(polled_at) at query time to pick the latest snapshot.
CREATE TABLE IF NOT EXISTS device_inventory
(
    polled_at      DateTime64(3, 'UTC'),
    exporter       IPv6 CODEC(ZSTD(1)),
    sys_descr      String,
    sys_object_id  LowCardinality(String),
    sys_uptime_ms  UInt64,
    sys_name       String,
    sys_location   String,
    sys_contact    String,
    iface_count    UInt32,
    poll_duration_ms UInt32,
    poll_status    LowCardinality(String)   -- ok | partial | error
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(polled_at)
ORDER BY (exporter, polled_at)
TTL toDateTime(polled_at) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;

-- Per-interface SNMP attributes — a snapshot per (exporter, ifindex,
-- polled_at). Joins to iface_counter_samples (sFlow / gNMI-driven)
-- on (exporter, ifindex) for the Devices → Interfaces sub-tab.
CREATE TABLE IF NOT EXISTS device_snmp_interfaces
(
    polled_at      DateTime64(3, 'UTC'),
    exporter       IPv6 CODEC(ZSTD(1)),
    ifindex        UInt32,
    if_descr       String,                    -- "GigabitEthernet0/0/1"
    if_alias       String,                    -- operator-set description
    if_type        UInt32,
    if_speed_bps   UInt64,                    -- normalized: ifHighSpeed*1e6 if set, else ifSpeed
    if_mtu         UInt32,
    if_admin_status LowCardinality(String),   -- up | down | testing
    if_oper_status  LowCardinality(String),   -- up | down | testing | unknown | dormant | notPresent | lowerLayerDown
    if_in_errors   UInt64,
    if_out_errors  UInt64,
    if_in_discards UInt64,
    if_out_discards UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(polled_at)
ORDER BY (exporter, ifindex, polled_at)
TTL toDateTime(polled_at) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
