-- 000013_resource_value_numeric.sql
--
-- Extends device_resource_samples for the ENTITY-SENSOR-MIB metrics
-- that don't fit a 0–100 percent shape: temperature (°C), fan (RPM),
-- voltage (V), current (A), power (W). The V1 column `value_percent`
-- stays put — it still drives sparklines for cpu / memory / storage
-- where 0–100 is the natural scale — and the two new columns carry
-- the raw numeric reading plus its unit.
--
-- ALTER on a ReplicatedMergeTree / MergeTree is online; new rows
-- populate the new columns and existing rows backfill to zero / empty
-- which the read layer interprets as "use the legacy value_percent".
ALTER TABLE device_resource_samples
    ADD COLUMN IF NOT EXISTS value_numeric Float64 AFTER max_bytes,
    ADD COLUMN IF NOT EXISTS unit          LowCardinality(String) AFTER value_numeric;
