-- 000016_snmp_version_and_dynamic_default.sql
--
-- Two related additions to the SNMP substrate:
--
-- 1. device_inventory grows an `snmp_version` column so the UI can
--    show the actual version that walked the device — previously the
--    Inventory section hardcoded "snmp · v2c" regardless of whether a
--    v3 credential resolved the walk. Default '' for back-compat with
--    rows written before the column existed; the api/UI treat empty
--    as "unknown".
--
-- 2. snmp_global_defaults grows a `default_for_dynamic` flag. When an
--    exporter shows up in flows with no per-exporter binding, the
--    scheduler walks it using the global whose flag is true. Lets
--    the operator pick whether v2c or v3 is the fleet-wide default for
--    dynamic discovery — currently the resolver hardcoded v2c, which
--    broke v3-only deployments.
--
-- Forward-only migration. Re-applying is a no-op on a populated cluster.

ALTER TABLE device_inventory
    ADD COLUMN IF NOT EXISTS snmp_version LowCardinality(String) DEFAULT '';

ALTER TABLE snmp_global_defaults
    ADD COLUMN IF NOT EXISTS default_for_dynamic UInt8 DEFAULT 0;
