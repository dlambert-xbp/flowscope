-- 000012_device_resources.sql
--
-- Timeseries table for the SNMP-derived device-health metrics rendered
-- on the Devices tab (CPU, memory, storage, optionally temperature /
-- fan). Modeled on iface_counter_samples — one row per (exporter,
-- kind, component, polled_at) — so the read pattern is identical:
-- argMax for "latest" tiles, range scan over polled_at for sparklines.
--
-- VISION.md §3.1 keeps SNMP as a fallback enrichment path; the snmp
-- service writes here on the same 15-min cadence as device_inventory.
-- 30 day TTL keeps the table cheap; the inventory table sits at 90.
CREATE TABLE IF NOT EXISTS device_resource_samples
(
    polled_at      DateTime64(3, 'UTC'),
    exporter       IPv6 CODEC(ZSTD(1)),
    -- kind: which metric family the row carries. LowCardinality so
    -- the column compresses to one entry per distinct family.
    --   cpu          — value_percent only
    --   memory       — value_bytes + max_bytes; value_percent derived
    --   storage      — value_bytes + max_bytes
    --   temperature  — value_percent carries degrees-C (overloaded)
    --   fan          — value_percent carries RPM (overloaded)
    kind           LowCardinality(String),
    -- component: vendor-specific identifier for the resource instance.
    -- Examples: "Processor 1", "CPU0", "Pool: Processor", "bootflash:",
    -- "Module 1". Stored as the operator-readable string the device
    -- returned in hrDeviceDescr / ciscoMemoryPoolName / equivalent.
    component      String,
    -- value_percent: utilization 0–100 for cpu/memory; overloaded
    -- numeric for temperature/fan. Zero when not populated.
    value_percent  Float32,
    -- value_bytes: bytes-used for memory / storage. Zero otherwise.
    value_bytes    UInt64,
    -- max_bytes: total capacity for memory / storage. Zero when
    -- unknown (UI falls back to percent in that case).
    max_bytes      UInt64,
    -- source: which MIB / OID set this row came from. Lets the UI
    -- explain provenance ("via HOST-RESOURCES-MIB") and lets a future
    -- vendor extension take precedence over a generic source.
    --   hrmib            — HOST-RESOURCES-MIB
    --   cisco-process    — CISCO-PROCESS-MIB
    --   cisco-mempool    — CISCO-MEMORY-POOL-MIB
    --   cisco-enhmempool — CISCO-ENHANCED-MEMPOOL-MIB
    --   juniper-jnx      — JUNIPER-MIB jnxOperating*
    --   arista-entity    — ENTITY-SENSOR-MIB on Arista
    source         LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(polled_at)
ORDER BY (exporter, kind, component, polled_at)
TTL toDateTime(polled_at) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;
