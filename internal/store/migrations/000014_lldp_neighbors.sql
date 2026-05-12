-- 000014_lldp_neighbors.sql
--
-- L2 topology adjacency, sourced from LLDP (IEEE 802.1AB) and CDP
-- (Cisco Discovery Protocol). One row per (local exporter, local port,
-- discovery_proto, remote chassis id, remote port id). The snmp
-- service walks both MIBs every five minutes (topology is stable, so
-- the cadence is light) and inserts a fresh row per (exporter, edge)
-- on every walk.
--
-- ReplacingMergeTree on last_seen lets repeated walks "overwrite" the
-- previous snapshot without an explicit DELETE: the engine collapses
-- duplicates on merge by argMax(last_seen). The read path uses FINAL
-- (or argMax at query time) to see the latest snapshot. TTL drops
-- stale edges after 30 days so a removed cable doesn't haunt the
-- graph forever — the same retention iface_counter_samples uses.
--
-- VISION.md §3.1 — SNMP is fallback-only, but topology *is* one of
-- the canonical SNMP use cases the document explicitly carves out
-- ("lldpRemTable"). 5-min cadence + bounded fan-out keeps the load
-- well under what fleet-wide polling would cost.
CREATE TABLE IF NOT EXISTS lldp_neighbors
(
    -- last_seen drives the ReplacingMergeTree version + the TTL.
    -- DateTime64(3) keeps walk-to-walk sub-second ordering deterministic
    -- when two walks race on the same edge.
    last_seen              DateTime64(3, 'UTC'),
    first_seen             DateTime64(3, 'UTC'),
    -- local_exporter is the device that *reported* the neighbor (the
    -- one we walked). IPv6 column with v4-mapped form so the schema
    -- accepts either family — matches every other exporter column.
    local_exporter         IPv6 CODEC(ZSTD(1)),
    local_ifindex          UInt32,
    -- local_port_name: resolved on the walker side from the same
    -- exporter's ifTable snapshot when available. Empty string when
    -- the lookup misses (interface not yet walked / not in ifTable).
    -- Persisting it denormalised keeps the API read trivial and means
    -- a brief mismatch with device_snmp_interfaces during the next
    -- walk window is invisible to the UI.
    local_port_name        String,
    -- discovery_proto: 'lldp' (preferred, vendor-neutral) or 'cdp'
    -- (Cisco fallback). LowCardinality so a future 'fdp' (Foundry)
    -- or 'eddp' costs nothing.
    discovery_proto        LowCardinality(String),
    -- remote_chassis_id: normalized to a printable string. The
    -- walker decodes lldpRemChassisIdSubtype (4=MAC, 5=netaddr,
    -- 7=local) into a stable canonical form (colon-separated MAC,
    -- IP-string, raw string respectively). For CDP this is the
    -- cdpCacheDeviceId — usually a hostname or formatted MAC.
    remote_chassis_id      String,
    remote_sys_name        String,
    remote_sys_desc        String,
    remote_port_id         String,
    -- remote_capabilities: comma-separated bitfield (e.g.
    -- "bridge,router,wlan-ap"). The walker translates the LLDP
    -- system-capabilities bitmap and the CDP cdpCacheCapabilities
    -- bitmap into the same shared lowercase string set so the API
    -- can render them uniformly.
    remote_capabilities    String,
    -- remote_management_addr: optional. When the LLDP TLV carries
    -- a management address (or CDP equivalent), persist it as IPv6
    -- so the topology layer can join discovered-only remotes back
    -- to known devices by management IP. Nullable so the column
    -- distinguishes "no TLV" from "0.0.0.0".
    remote_management_addr Nullable(IPv6)
)
ENGINE = ReplacingMergeTree(last_seen)
PARTITION BY toYYYYMMDD(last_seen)
ORDER BY (local_exporter, local_ifindex, discovery_proto, remote_chassis_id, remote_port_id)
TTL toDateTime(last_seen) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;
