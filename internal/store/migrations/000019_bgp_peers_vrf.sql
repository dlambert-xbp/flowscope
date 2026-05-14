-- 000019_bgp_peers_vrf.sql
--
-- Per-VRF BGP support. The bgp_peers table is extended with a `vrf`
-- column so the alert engine + Devices view can group peers by VRF
-- (default, mgmt, customer-A, etc.) on devices that run multi-VRF
-- BGP (provider edges, MPLS PEs, multi-tenant data center fabric).
--
-- Backfill semantics: existing rows (written by the BGP4-MIB walker
-- before this migration) get vrf='default' via the column DEFAULT,
-- which is correct because BGP4-MIB only exposes the global routing
-- table.
--
-- The ORDER BY on the table cannot be changed in place (ClickHouse
-- requires a recreate), so the partitioning + ordering stays as-is.
-- Read queries that need per-VRF aggregation add `vrf` to the
-- GROUP BY explicitly.
--
-- Forward-only migration. Re-applying is a no-op on a populated
-- cluster (ADD COLUMN IF NOT EXISTS).

ALTER TABLE bgp_peers
    ADD COLUMN IF NOT EXISTS vrf String DEFAULT 'default' AFTER source
