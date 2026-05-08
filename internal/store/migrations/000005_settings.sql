-- 000005_settings.sql
--
-- Settings & administration substrate. Eight tables landing together
-- because they're a coherent feature set and the api wires them up
-- in one slice. Forward-only — never edit this file after release.
--
-- Convention: every settings table uses ReplacingMergeTree(updated_at)
-- so PUT-then-PUT for the same key collapses to the newest row on
-- background merge. SELECT FINAL in handlers reads the current state.
-- Removal uses ALTER TABLE … DELETE WHERE, matching the existing
-- snmp_credentials pattern.
--
-- Audit events are append-only — they're an immutable ledger by
-- design.

-- custom_services: operator-defined service-name overrides. Range
-- support: port_lo == port_hi means a single port. Group is optional
-- and read by the alert engine for logical port collections. id is a
-- stable UUID minted on create so PUT can edit a row by identity even
-- when the (proto, port_lo, port_hi) tuple changes.
CREATE TABLE IF NOT EXISTS custom_services
(
    id              UUID,
    proto           LowCardinality(String),       -- 'tcp' | 'udp' | 'sctp' | 'dccp'
    port_lo         UInt16,
    port_hi         UInt16,
    name            String,
    description     String  DEFAULT '',
    grp             String  DEFAULT '',           -- 'grp' not 'group' — group is reserved
    owner           String  DEFAULT '',
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;

-- audit_events: append-only ledger of every settings mutation. The
-- before / after JSON blobs let an operator answer "what did this
-- look like before the change?" without requiring per-table point-
-- in-time queries. TTL'd at 365 days to bound storage.
CREATE TABLE IF NOT EXISTS audit_events
(
    ts              DateTime64(3, 'UTC'),
    actor           String,
    action          LowCardinality(String),       -- 'create' | 'update' | 'delete'
    resource        LowCardinality(String),       -- 'custom_service' | 'api_token' | …
    target          String,                       -- key/id of the affected row
    before_json     String  DEFAULT '',
    after_json      String  DEFAULT '',
    request_id      String  DEFAULT '',
    source_ip       IPv6
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (ts, actor, action)
TTL toDateTime(ts) + INTERVAL 365 DAY
SETTINGS index_granularity = 8192;

-- api_tokens: hashed API tokens with per-token scope. token_hash is
-- bcrypt over the plaintext; token_prefix stores the first 6 chars
-- so the UI can render "fst_••••" without revealing the secret. The
-- plaintext is shown ONCE on creation and never persisted.
--
-- updated_at carries the ReplacingMergeTree version: bumping it (e.g.
-- on revoke or last-used update) collapses prior rows on merge. last_
-- used_at writes are throttled by the api so this table doesn't churn
-- on every request.
CREATE TABLE IF NOT EXISTS api_tokens
(
    id              UUID,
    name            String,
    token_prefix    String,
    token_hash      String,
    scope           LowCardinality(String),       -- 'read' | 'write' | 'admin'
    created_at      DateTime64(3, 'UTC'),
    created_by      String  DEFAULT 'anonymous',
    last_used_at    DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC'),
    expires_at      DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC'),
    revoked_at      DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC'),
    updated_at      DateTime64(3, 'UTC')          -- ReplacingMergeTree version
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;

-- exporter_allowlist: opt-in list of exporters the ingest service
-- accepts flows from. Empty (zero rows) means accept-all (current
-- default). Adding a row switches the service to deny-by-default
-- mode. label is the operator-visible name; notes is free-form.
CREATE TABLE IF NOT EXISTS exporter_allowlist
(
    exporter        IPv6,
    label           String  DEFAULT '',
    enabled         UInt8   DEFAULT 1,
    notes           String  DEFAULT '',
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY exporter
SETTINGS index_granularity = 8192;

-- app_settings: flat key/value store for general settings (display
-- name, default time range, etc.). value_json is whatever the api
-- handler agrees on for that key — no enforcement here, just storage.
CREATE TABLE IF NOT EXISTS app_settings
(
    name            String,
    value_json      String,
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name
SETTINGS index_granularity = 8192;

-- alert_rule_settings: tunables for the Go-coded rules in
-- internal/alerteng. Operators can tweak parameters (silence_seconds,
-- bytes_threshold, …), enable/disable, override severity, set a
-- runbook URL, and pick channels — but they can't define new rules
-- here. Rule definitions are still code; that's a Phase 2 feature.
-- params_json is rule-specific (validated by the rule's own loader).
CREATE TABLE IF NOT EXISTS alert_rule_settings
(
    rule_id         LowCardinality(String),       -- 'exporter_silent', 'heavy_talker', …
    enabled         UInt8   DEFAULT 1,
    severity        LowCardinality(String) DEFAULT '',
    params_json     String  DEFAULT '{}',
    runbook         String  DEFAULT '',
    channels        String  DEFAULT '[]',         -- JSON array of webhook IDs
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY rule_id
SETTINGS index_granularity = 8192;

-- webhook_endpoints: outbound integration targets the alert engine
-- can post to. Generic shape — one row covers Slack incoming-webhooks,
-- Microsoft Teams, PagerDuty Events API, custom HTTP endpoints. The
-- secret_ct field is AES-GCM-sealed under FLOWSCOPE_SNMP_KEY (we
-- reuse the existing master key so we don't introduce a second secret
-- root). On v3-style protocols a header_template_json carries the
-- auth header construction recipe.
CREATE TABLE IF NOT EXISTS webhook_endpoints
(
    id              UUID,
    name            String,
    kind            LowCardinality(String),       -- 'slack' | 'teams' | 'pagerduty' | 'http'
    url             String,
    secret_ct       String  DEFAULT '',           -- optional, sealed
    header_template_json String DEFAULT '{}',
    enabled         UInt8   DEFAULT 1,
    severity_filter String  DEFAULT 'critical,warning,info',
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;

-- oidc_config: single-row OIDC configuration. The login flow is
-- DISABLED in v1 — this table exists so the operator can configure
-- the integration ahead of the Phase 2 rollout. The api reads the
-- 'enabled' flag and refuses to gate routes on it until Phase 2
-- ships. id is a constant 'singleton' string so the table acts as
-- a 1-row record under ReplacingMergeTree.
CREATE TABLE IF NOT EXISTS oidc_config
(
    id              String,                       -- always 'singleton'
    enabled         UInt8   DEFAULT 0,
    issuer          String  DEFAULT '',
    client_id       String  DEFAULT '',
    client_secret_ct String  DEFAULT '',
    redirect_uri    String  DEFAULT '',
    scopes          String  DEFAULT 'openid email profile',
    updated_at      DateTime64(3, 'UTC'),
    updated_by      String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;
