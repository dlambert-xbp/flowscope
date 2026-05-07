-- 000004_snmp_credentials.sql
--
-- Per-exporter SNMP credential bindings. Encrypted at rest with
-- AES-256-GCM whose key is derived via HKDF-SHA256 from the
-- FLOWSCOPE_SNMP_KEY env var (resolved from Azure Key Vault in
-- production). Per VISION.md §4.2 the master key MUST stay constant
-- across restarts — rotating it invalidates every stored
-- ciphertext.
--
-- ReplacingMergeTree by exporter so PUT-then-PUT for the same
-- target writes a new row that supersedes the old. SELECT FINAL
-- in the api yields the current binding.

CREATE TABLE IF NOT EXISTS snmp_credentials
(
    exporter        IPv6 CODEC(ZSTD(1)),
    version         LowCardinality(String),       -- 'v2c' | 'v3'
    port            UInt16  DEFAULT 161,
    interval_sec    UInt32  DEFAULT 900,          -- per-binding poll cadence

    -- v2c
    community_ct    String,                       -- AES-GCM ciphertext (nonce|tag|ct)

    -- v3 user + auth/priv parameters
    v3_username     String,
    v3_auth_proto   LowCardinality(String) DEFAULT '',  -- '' | MD5 | SHA | SHA-224 | SHA-256 | SHA-384 | SHA-512
    v3_auth_pass_ct String,
    v3_priv_proto   LowCardinality(String) DEFAULT '',  -- '' | DES | AES | AES-192 | AES-256
    v3_priv_pass_ct String,
    v3_context      String DEFAULT '',

    updated_at      DateTime64(3, 'UTC'),
    updated_by      String DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY exporter
SETTINGS index_granularity = 8192;
