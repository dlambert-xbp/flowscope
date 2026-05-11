-- 000011_oidc_sessions.sql
--
-- OIDC sessions / revocation list for the Phase 2 login flow
-- (TASKS.md P3 #23). Cookie-based sessions are stateless by default —
-- the api signs the cookie with HMAC-SHA256 and verifies it on every
-- request without a DB hit. This table is for the explicit-revoke
-- path (POST /auth/logout, "log out everywhere" flows) so an
-- operator can invalidate a still-valid signed cookie before its
-- expiry. Sessions are inserted only when revocation is needed —
-- the happy login path stays DB-free.
--
-- ReplacingMergeTree(revoked) collapses repeated revoke writes on
-- merge; ORDER BY id gives O(log n) point lookup keyed by the
-- session UUID stamped in the cookie payload.
--
-- oidc_config already lives in 000005_settings.sql; we don't touch
-- it here. The single-row table there carries issuer / client_id /
-- client_secret_ct / redirect_uri / scopes / enabled.
CREATE TABLE IF NOT EXISTS oidc_sessions
(
    id              UUID,
    subject         String,
    email           String  DEFAULT '',
    scope           LowCardinality(String),       -- 'read' | 'write' | 'admin'
    created_at      DateTime64(3, 'UTC'),
    expires_at      DateTime64(3, 'UTC'),
    revoked         UInt8   DEFAULT 0
)
ENGINE = ReplacingMergeTree(revoked)
ORDER BY id
SETTINGS index_granularity = 8192;
