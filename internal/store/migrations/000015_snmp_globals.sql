-- 000015_snmp_globals.sql
--
-- Global default SNMP credentials. Two role rows — 'v2c' and 'v3' —
-- act as a fallback when a per-exporter binding is set to
-- binding_kind='global_v2c' or 'global_v3'. Same encryption-at-rest
-- pattern as snmp_credentials: secrets are AES-256-GCM-sealed under
-- the FLOWSCOPE_SNMP_KEY master.
--
-- Forward-only migration. Adds the `binding_kind` column to
-- snmp_credentials so an existing v2c/v3 binding can be flipped to
-- "use global" without inlining the secret. Default 'custom'
-- preserves existing rows verbatim.

ALTER TABLE snmp_credentials
    ADD COLUMN IF NOT EXISTS binding_kind LowCardinality(String) DEFAULT 'custom';

CREATE TABLE IF NOT EXISTS snmp_global_defaults
(
    role            LowCardinality(String),       -- 'v2c' | 'v3'
    port            UInt16  DEFAULT 161,
    interval_sec    UInt32  DEFAULT 60,

    -- v2c
    community_ct    String  DEFAULT '',           -- AES-GCM ciphertext (nonce|tag|ct)

    -- v3
    v3_username     String  DEFAULT '',
    v3_auth_proto   LowCardinality(String) DEFAULT '',  -- '' | MD5 | SHA | SHA-224 | SHA-256 | SHA-384 | SHA-512
    v3_auth_pass_ct String  DEFAULT '',
    v3_priv_proto   LowCardinality(String) DEFAULT '',  -- '' | DES | AES | AES-192 | AES-256
    v3_priv_pass_ct String  DEFAULT '',
    v3_context      String  DEFAULT '',

    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY role
SETTINGS index_granularity = 8192;
