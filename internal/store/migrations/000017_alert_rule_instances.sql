-- 000017_alert_rule_instances.sql
--
-- Per-device alerting: introduce the (template, instance, scope)
-- model layered on top of the existing globally-scoped Go rules.
--
-- Three changes here:
--
--   1. CREATE TABLE alert_rule_instances. Each row is one operator-
--      created (template + scope + params) binding. The engine
--      iterates instances on its tick and tags every event with the
--      instance_id that produced it. ReplacingMergeTree on updated_at
--      lets the api do upsert-by-id with no application-side version
--      stamp.
--
--   2. ALTER alert_events ADD COLUMN instance_id. Each event row
--      carries the instance that fired it. Default '' so rows written
--      by the pre-instance engine remain readable; the engine
--      bootstrap reattributes those to the seed instance for their
--      rule_id on first tick after deploy.
--
--   3. SEED step. For every existing alert_rule_settings row, insert
--      an alert_rule_instances row with the same rule_id (as
--      template_id), the same params, and is_seed=1. This preserves
--      operator-tuned thresholds across the migration. Built-in rules
--      with no override row get their seed instance lazy-created by
--      the api on first read (handled in Go, not SQL).
--
-- Forward-only migration. Re-applying is a no-op because the
-- migrations tracker only invokes each file once; do not depend on
-- IF NOT EXISTS for the INSERT.

-- alert_rule_instances: operator-created bindings of (template + scope
-- + params). instance_id is a UUID minted by the api on POST; for seed
-- rows it is the deterministic 'seed_' || template_id form so the
-- table never gets two seed rows for the same template even if the
-- migration body ran twice (which it cannot, but defense in depth).
CREATE TABLE IF NOT EXISTS alert_rule_instances
(
    instance_id      String,
    template_id      LowCardinality(String),
    name             String,
    enabled          UInt8   DEFAULT 1,
    severity         LowCardinality(String) DEFAULT '',
    scope_json       String  DEFAULT '{}',
    params_json      String  DEFAULT '{}',
    runbook          String  DEFAULT '',
    channels         String  DEFAULT '[]',
    is_seed          UInt8   DEFAULT 0,
    created_at       DateTime64(3, 'UTC'),
    updated_at       DateTime64(3, 'UTC'),
    updated_by       String  DEFAULT 'anonymous'
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY instance_id
SETTINGS index_granularity = 8192;

-- alert_events grows an instance_id column. Default '' keeps rows
-- written by the pre-instance engine readable; the engine bootstrap
-- reattributes those to the seed instance for their rule_id at
-- restart so the in-memory open map keys to the new shape without
-- emitting close + re-open events.
ALTER TABLE alert_events
    ADD COLUMN IF NOT EXISTS instance_id String DEFAULT '' AFTER rule_id;

-- Seed: copy every alert_rule_settings row into alert_rule_instances
-- with template_id = old.rule_id, scope_json = '{}' (matches all),
-- and the existing params/severity/runbook/channels carried over.
-- is_seed = 1 marks these as auto-created so the UI can render them
-- as 'Default · all devices' rows distinct from operator-created
-- instances.
INSERT INTO alert_rule_instances
    (instance_id, template_id, name, enabled, severity,
     scope_json, params_json, runbook, channels,
     is_seed, created_at, updated_at, updated_by)
SELECT
    concat('seed_', rule_id)               AS instance_id,
    rule_id                                AS template_id,
    concat('Default · ', rule_id)          AS name,
    enabled,
    severity,
    '{}'                                   AS scope_json,
    params_json,
    runbook,
    channels,
    1                                      AS is_seed,
    updated_at                             AS created_at,
    updated_at,
    updated_by
FROM alert_rule_settings FINAL
