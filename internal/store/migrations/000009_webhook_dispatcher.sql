-- 000009_webhook_dispatcher.sql
--
-- Webhook dispatcher persistence. Two tables:
--
--   webhook_dispatcher_state
--     Single-row cursor that survives restarts. The dispatcher polls
--     alert_events for new opened/closed transitions, advances last_ts
--     to the max(ts) it just processed, and writes the row back. Stored
--     as a ReplacingMergeTree on a constant id so SELECT FINAL always
--     returns the newest cursor.
--
--   webhook_deliveries
--     Per-(event signature, endpoint) ledger. Before POST, the
--     dispatcher writes one row per attempt with status ('queued' →
--     'sent' | 'failed'). At-startup or mid-tick the dispatcher reads
--     this table for events still inside the rescue window and skips
--     any (signature, endpoint) tuple that already has a 'sent' row —
--     so a crash mid-fanout doesn't double-fire a channel.
--
-- Event signatures are sha256(ts|rule_id|scope|group_key|state). The
-- alert_events table has no surrogate id; this composite is unique
-- enough for the dispatcher's purposes (engine never writes two rows
-- with the same (rule, scope, group_key, state) at the exact same
-- millisecond).

CREATE TABLE IF NOT EXISTS webhook_dispatcher_state
(
    id           String,                       -- always 'singleton'
    last_ts      DateTime64(3, 'UTC'),
    last_event   String  DEFAULT '',           -- last event signature processed
    updated_at   DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS webhook_deliveries
(
    ts            DateTime64(3, 'UTC'),
    signature     String,                      -- sha256 hex of the source alert_event
    endpoint_id   UUID,
    attempt       UInt8,
    status        LowCardinality(String),      -- 'queued' | 'sent' | 'failed' | 'test'
    http_status   UInt16  DEFAULT 0,
    error         String  DEFAULT '',
    duration_ms   UInt32  DEFAULT 0,
    event_ts      DateTime64(3, 'UTC'),        -- ts of the originating alert_event
    rule_id       LowCardinality(String) DEFAULT '',
    severity      LowCardinality(String) DEFAULT ''
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(ts)
ORDER BY (signature, endpoint_id, attempt, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY
SETTINGS index_granularity = 8192;
