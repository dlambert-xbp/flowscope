-- 000002_alerts.sql
--
-- Append-only event ledger for the alert engine. Each row records one
-- state transition (opened, heartbeat, closed, acknowledged) for one
-- (rule, scope, group_key) tuple. Current state is computed via
-- argMax(state, ts) GROUP BY (rule, scope, group_key) — see
-- QueryAlerts in internal/store/queries.go.
--
-- Append-only is deliberate: the engine never updates a row, only
-- writes a new one. This avoids the "did the row land?" race that
-- mutable schemas suffer and matches ClickHouse's strengths.

CREATE TABLE IF NOT EXISTS alert_events
(
    ts          DateTime64(3, 'UTC'),
    rule_id     LowCardinality(String),
    severity    LowCardinality(String),
    state       LowCardinality(String),     -- opened | heartbeat | closed | acknowledged
    scope       String,
    group_key   String,
    title       String,
    body        String,
    runbook     String  DEFAULT '',
    actor       String  DEFAULT 'engine',   -- 'engine' or operator login
    labels      Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (rule_id, scope, group_key, ts)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS index_granularity = 8192;
