-- 000010_leader_lease.sql
--
-- Leader-election substrate for cmd/alert. Today the alert binary runs
-- as a single replica because two instances would dupe-fire the engine
-- AND the webhook dispatcher (migration 000009). This table backs a
-- ClickHouse-based lease so we can scale alert.replicas > 1 without
-- duplicates.
--
-- Single row per lease name (today: 'alert'). ReplacingMergeTree on
-- acquired_at means the newest acquisition collapses prior rows on
-- merge; readers use SELECT FINAL to see the current holder.
--
-- The acquire flow is:
--   1. SELECT FINAL the current row.
--   2. If no row, or holder == self, or expires_at < now() → INSERT a
--      new row with updated expires_at and acquired_at = now().
--   3. SELECT FINAL again to confirm self is the holder. This second
--      read closes the race when two followers attempt to acquire
--      simultaneously — the one whose INSERT lands second wins because
--      ReplacingMergeTree keeps the row with the greater acquired_at.
--
-- Forward-only and idempotent (CLAUDE.md §"Schema migrations are
-- forward-only and idempotent"): IF NOT EXISTS guards a re-run on a
-- ClickHouse instance that somehow already has the table.

CREATE TABLE IF NOT EXISTS leader_lease
(
    name         LowCardinality(String),         -- lease key, e.g. 'alert'
    holder       String,                         -- hostname-pid-rand of current owner
    expires_at   DateTime64(3, 'UTC'),
    acquired_at  DateTime64(3, 'UTC')            -- ReplacingMergeTree version
)
ENGINE = ReplacingMergeTree(acquired_at)
ORDER BY name
SETTINGS index_granularity = 8192;
