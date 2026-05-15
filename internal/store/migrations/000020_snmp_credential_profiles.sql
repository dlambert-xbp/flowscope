-- 000020_snmp_credential_profiles.sql
--
-- Replace the role-keyed snmp_global_defaults (one v2c row, one v3 row)
-- with snmp_profiles — a named, id-keyed library of N reusable
-- credential profiles. Each profile carries a use_for_discovery flag +
-- priority; when a flow-discovered exporter shows up without a
-- per-exporter binding, the scheduler tries discovery-flagged profiles
-- in priority order, 1s-tight timeout on the first attempt. Bulk
-- discovery scans (new in this release) target a single chosen profile.
--
-- Per-exporter bindings (snmp_credentials) drop the binding_kind enum
-- and gain a profile_id reference:
--   - profile_id = '' : custom inline (row owns the secrets)
--   - profile_id != '': reference to snmp_profiles.id (profile owns the
--                       secrets; row may still override port/interval)
--
-- Data migration:
--   - Existing snmp_global_defaults rows become two named profiles with
--     deterministic IDs ("Global v2c (migrated)" / "Global v3 (migrated)")
--     so existing bindings keep working without operator action.
--   - default_for_dynamic=1 on either global becomes use_for_discovery=1
--     on the corresponding migrated profile, priority 1 (v2c) / 2 (v3).
--     This preserves the legacy "v2c wins when both flagged" ordering.
--   - snmp_credentials rows with binding_kind='global_v2c' / 'global_v3'
--     are re-inserted with profile_id set. ReplacingMergeTree collapses
--     the old row.
--   - binding_kind column is dropped after the conversion.
--
-- snmp_global_defaults is left in place (forward-only: don't drop a
-- table in the same migration that converts away from it). A follow-up
-- cleanup migration can DROP TABLE once we've verified no operational
-- dependencies remain.
--
-- Forward-only and idempotent. Re-applying is a no-op modulo a
-- timestamp bump on the migrated rows.

CREATE TABLE IF NOT EXISTS snmp_profiles
(
    id                  String,                          -- UUID, app-generated
    name                String,                          -- human label; uniqueness enforced in app
    version             LowCardinality(String),          -- 'v2c' | 'v3'
    port                UInt16  DEFAULT 161,
    interval_sec        UInt32  DEFAULT 60,

    community_ct        String  DEFAULT '',              -- AES-GCM ciphertext (v2c)

    v3_username         String  DEFAULT '',
    v3_auth_proto       LowCardinality(String) DEFAULT '',
    v3_auth_pass_ct     String  DEFAULT '',
    v3_priv_proto       LowCardinality(String) DEFAULT '',
    v3_priv_pass_ct     String  DEFAULT '',
    v3_context          String  DEFAULT '',

    use_for_discovery   UInt8   DEFAULT 0,
    discovery_priority  UInt16  DEFAULT 0,               -- meaningful only when use_for_discovery=1

    deleted             UInt8   DEFAULT 0,               -- tombstone; Delete() upserts deleted=1

    updated_at          DateTime64(3, 'UTC'),
    updated_by          String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;

ALTER TABLE snmp_credentials
    ADD COLUMN IF NOT EXISTS profile_id String DEFAULT '';

-- Migrate the v2c global into a named profile. The deterministic id
-- means re-running the migration upserts the same row rather than
-- creating duplicates.
INSERT INTO snmp_profiles
   (id, name, version, port, interval_sec,
    community_ct,
    v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    deleted, updated_at, updated_by)
SELECT
    '00000000-0000-0000-0000-0000000002c0',
    'Global v2c (migrated)',
    'v2c',
    port, interval_sec,
    community_ct,
    '', '', '',
    '', '', '',
    ifNull(default_for_dynamic, toUInt8(0)),
    if(default_for_dynamic = 1, toUInt16(1), toUInt16(0)),
    toUInt8(0),
    updated_at, updated_by
FROM snmp_global_defaults FINAL
WHERE role = 'v2c' AND length(community_ct) > 0;

-- Migrate the v3 global. v3 is "configured" when v3_username is set.
INSERT INTO snmp_profiles
   (id, name, version, port, interval_sec,
    community_ct,
    v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    deleted, updated_at, updated_by)
SELECT
    '00000000-0000-0000-0000-0000000003a0',
    'Global v3 (migrated)',
    'v3',
    port, interval_sec,
    '',
    v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    ifNull(default_for_dynamic, toUInt8(0)),
    if(default_for_dynamic = 1, toUInt16(2), toUInt16(0)),
    toUInt8(0),
    updated_at, updated_by
FROM snmp_global_defaults FINAL
WHERE role = 'v3' AND length(v3_username) > 0;

-- Convert global_v2c-kind bindings: re-insert pointing at the migrated
-- v2c profile. The binding_kind value becomes 'custom' as a transitional
-- holder (the column is dropped two statements down). updated_at = now()
-- so ReplacingMergeTree picks this row over the original.
INSERT INTO snmp_credentials
   (exporter, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    updated_at, updated_by, binding_kind, profile_id)
SELECT
    exporter, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    now64(),
    updated_by,
    'custom',
    '00000000-0000-0000-0000-0000000002c0'
FROM snmp_credentials FINAL
WHERE binding_kind = 'global_v2c';

INSERT INTO snmp_credentials
   (exporter, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    updated_at, updated_by, binding_kind, profile_id)
SELECT
    exporter, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    now64(),
    updated_by,
    'custom',
    '00000000-0000-0000-0000-0000000003a0'
FROM snmp_credentials FINAL
WHERE binding_kind = 'global_v3';

ALTER TABLE snmp_credentials DROP COLUMN IF EXISTS binding_kind;
