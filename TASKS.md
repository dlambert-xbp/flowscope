# FlowScope — Open Tasks

Living TODO list. Items are grouped by tier — bugs and papercuts at the top,
phase 2/3 architecture at the bottom. Move things to the bottom of their
section as they're picked up; delete on commit.

Last updated: 2026-05-08.

> **Session label:** items prefixed with `[Session X]` are scoped to fit one
> focused work session. Pick one when you sit down — it has a clear start,
> end, and PR shape. The ordering inside each session is the implementation
> order; nothing inside a session blocks anything else inside it.

---

## Bugs & papercuts (should fix soon)

- [x] `/api/top/*` filter chips show raw keys (`dst_port · 443`) instead of
      human labels (`service · https`). The `label` field on `Filter` is
      plumbed but only set in some triggers. Cosmetic.
- [x] Brand bar tab counts are stubbed (`—`, `—`, `0`). Should read live
      from `/api/summary`, `/api/devices`, `/api/alerts/summary`.
- [x] Per-binding SNMP interval is stored but ignored — scheduler still
      uses cluster-wide `FLOWSCOPE_SNMP_INTERVAL`. Trivial fix in
      `Scheduler.dispatch`.
- [x] No "walk now" button on the Settings → SNMP page. Operator waits up
      to 15 min for results from a new credential.
- [x] Flow-independent SNMP discovery. `Scheduler.discoverExporters` only
      walks devices that have produced flows. Devices in the credentials
      table without flow history are silently ignored. Fix: union
      flow-observed exporters with credential-bound exporters.

## Production blockers

- [~] **Auth on `/api/*`.** VISION.md §7 — `X-Auth-Token` (Phase 1) then
      OIDC SSO (Phase 2). PR #15 gated `/api/settings/*` and SNMP write
      endpoints behind `X-Auth-Token` (shared or per-token from the
      `api_tokens` table) with read/write/admin scopes. Read endpoints
      (`/api/summary`, `/api/flows/*`, `/api/devices/*`, `/api/alerts*`,
      `/api/top/*`, `/api/services/lookup`, `/api/services/library`)
      remain open behind the proxy. Closing those is the remaining
      Phase 1 work.
- [ ] **Alert engine leader election.** Single-replica today. Two
      `cmd/alert` instances would dupe-fire alerts. ClickHouse Keeper
      lock + leader-only writes.
- [ ] **Master key from Key Vault, not env var.** `FLOWSCOPE_SNMP_KEY`
      is a literal in `docker-compose.yml`. Build `internal/secrets` with
      env / file / Key Vault implementations and Workload Identity.
- [~] **Notification channels.** Webhook (Slack / Teams / PagerDuty),
      email (SMTP relay), syslog. PR #15 added the `webhook_endpoints`
      table, secret encryption, and the Settings → Integrations CRUD
      surface. **The dispatcher does not exist yet** — operators can
      configure webhooks but the engine never POSTs to them. See
      [Session B] below. Email and syslog channels are still
      not-yet-modeled.
- [ ] **Race detector in CI.** Local Windows can't run `go test -race`
      (cgo + gcc missing). GitHub Actions Linux runner should run it on
      every push.

## SNMP follow-ups

- [ ] SNMP v3 walk validation against a real lab. Crypto / store / API
      / UI all done; needs a real v3-only switch to confirm gosnmp's
      USM stack handshakes correctly.
- [ ] Bulk import of credentials. CSV upload or `flowscope snmp import`
      CLI for fleets.
- [ ] Master key rotation. Today, rotating invalidates every blob.
      Need a `re-encrypt under new master` admin action.
- [ ] LLDP/CDP topology + Devices → Neighbors sub-tab. SNMP
      `lldpRemTable` walk + `react-flow` graph.
- [ ] BGP via SNMP `bgpPeerTable` + Devices → BGP sub-tab.
- [x] On-demand triggered walks. `POST /api/devices/{e}/snmp/walk`
      that fires an immediate walk regardless of cadence.

## Alert engine follow-ups

- [ ] More built-in rules. Today: `exporter_silent` + `heavy_talker`.
      Roadmap from VISION.md §6:
  - Interface up/down (oper_status transitions, debounce)
  - Interface utilization > X% of ifSpeed (now feasible — SNMP gives us
    ifSpeed, sFlow gives us bps)
  - Errors / discards rate-of-change
  - BGP neighbor state change (Established → anything)
  - Device unreachable (gNMI dropped AND SNMP fails)
  - Flow anomaly (top-talker delta vs trailing 7-day baseline)
- [ ] Alert detail modal — timeline of underlying samples that
      triggered + linked flows.
- [ ] Per-rule history view ("how often does `exporter_silent` fire
      for this device this week?").
- [x] Auto-close after stability window. Currently the engine
      auto-closes the moment the condition clears; production rules
      typically want N-minute confirmation.
- [ ] Grouping key for incident correlation. Engine writes `group_key`
      on each event but the api treats every alert as independent.

## Architecture / Phase 2

- [ ] gNMI ingest (Phase 3 in VISION.md). Dial-in subscriptions,
      OpenConfig models, auto-promotion logic that retires SNMP polls
      when gNMI is available for the same OIDs.
- [ ] NetFlow v9 / IPFIX enterprise fields and options templates.
      Decoded but skipped today.
- [x] Variable-length IPFIX records. Currently records with an
      `0xFFFF` length field abandon the rest of the body — TODO in
      `decodeDataRecords`.

## Cold tier + scale

- [ ] ClickHouse TTL → cold blob storage. Schema has 7-day delete
      TTL; need `TO VOLUME 'cold'` policy + ADLS / S3 storage policy
      configured.
- [ ] 3-replica ClickHouse + Keeper coordination. Single-node today.
- [ ] UDP load balancer fronting multiple ingest replicas. Architecture
      document describes the shape for multi-replica; compose only
      runs one.

## Test footprint

- [ ] Integration tests against a real ClickHouse (testcontainers-go).
      Today: 49 unit tests, none touch a live DB.
- [ ] Golden tests for alert rules. `internal/alerteng` has zero tests
      because they want a live ClickHouse.
- [ ] Web e2e via Playwright. Mentioned in CLAUDE.md as a CI gate but
      not wired.

## UI / UX gaps

- [x] Light theme toggle. Tokens defined for light mode (used by the
      mock); needs a runtime switch.
- [ ] ⌘K command palette. Brand bar shows the kbd hint; not wired.
- [x] j/k keyboard navigation on tables.
- [x] Filter chips with human labels (see Bugs section).
- [x] Deep-linking on Devices. Selecting a device updates `useState`
      not URL — refresh loses selection.

## Docs / ops

- [x] `README.md` is still Python-era. Should describe the Go
      architecture and link to VISION.md / BUILD.md / CLAUDE.md.
- [ ] Helm chart for AKS. Compose works for dev; prod needs Helm per
      VISION.md §8.2.
- [ ] CI workflow files. GitHub Actions for `go test -race`,
      `golangci-lint`, `vitest`, `playwright`, `helm lint`, container
      build + sign.

## Settings page follow-ups (from PR #15)

PR #15 shipped a 9-section Settings shell. Four sections work
end-to-end (Services / SNMP / Auth & tokens / Audit); four others
persist values to ClickHouse but their consumer service does not yet
read the table, so editing in the UI has no operational effect. The
follow-ups below close those gaps. See
[~/.claude/plans/what-of-all-the-wise-cosmos.md] for the full audit.

### [Session A] Wire alert rule tunables into alerteng

Smallest delta, highest immediate operator value. **Goal:** edits to
`alert_rule_settings` (made via Settings → Alert rules) actually
change how `internal/alerteng` evaluates the built-in rules.

- [x] On `cmd/alert` startup, load `alert_rule_settings` rows and
      apply per-rule overrides to `DefaultRules()` in
      `internal/alerteng/rules.go`. Specifically: `enabled`,
      `severity` override, `params` (silence_seconds, active_seconds,
      window_seconds, bytes_threshold).
- [x] Periodic refresh tick (60s) so live edits propagate without a
      restart. Store version timestamp; only re-construct rules when
      the table's `max(updated_at)` has advanced.
- [x] Surface effective values back through `/api/settings/alert-rules`
      so the UI shows what's actually running, not just what was last
      saved.
- [x] Unit test: a rule loaded with custom `params` evaluates with
      those values.

### [Session B] Webhook dispatcher

**Goal:** alerts opened by the engine fan out to enabled webhook
endpoints with severity filtering applied.

- [ ] New goroutine in `cmd/alert` (or new `cmd/notifier` if leader
      election complicates things) that polls `alert_events` for new
      `opened` and `closed` transitions since the last cursor, joins
      against `webhook_endpoints WHERE enabled = 1`, applies
      `severity_filter`, and POSTs.
- [ ] Per-kind formatters: `slack` (Block Kit), `teams` (MessageCard),
      `pagerduty` (Events API v2 dedup_key from `group_key`),
      `http` (raw JSON with `header_template_json` for auth).
- [ ] Decrypt `secret_ct` via the shared `snmpx.Crypter`.
- [ ] Retry with exponential backoff; cap at 3 attempts; log failures
      to `audit_events` so operators can see why a channel is silent.
- [ ] `POST /api/settings/integrations/webhooks/{id}/test` that fires
      a synthetic test alert through the dispatcher path so operators
      can verify a new webhook before relying on it.

### [Session C] Wire exporter allowlist into ingest

**Goal:** rows in `exporter_allowlist` actually gate UDP packet
acceptance in `cmd/ingest`. Empty table = accept-all (current
behavior); non-empty + `enabled = 1` = accept; `enabled = 0` or
absent = drop with a counter increment.

- [ ] Refresh tick in `cmd/ingest` that reads the allowlist every 30s
      into a `map[netip.Addr]bool` guarded by `sync.RWMutex`.
- [ ] At packet ingress (NetFlow + sFlow + IPFIX listeners), gate on
      the source IP. Drops bump a new
      `flowscope_ingest_dropped_unauthorized_total` Prometheus counter
      labeled by exporter so the Overview panel can surface it.
- [ ] Allowlist edits prime the local map synchronously (api → ingest
      RPC? or just rely on the 30s tick and document the lag).
- [ ] Document the deny-by-default cutover in the panel banner so
      operators understand what flipping from zero rows to one rule
      means.

### [Session D] General settings consumption

**Goal:** values saved on Settings → General actually take effect.
Split into two sub-sessions because retention is meaningfully harder
than display values.

**[Session D1] Display values** (theme, default time range, display
name, timezone — easy):

- [x] `GET /api/config/effective` — returns the active values from
      `app_settings` (with defaults baked in for missing keys).
- [x] `App.tsx` hydrates from this on boot before the first paint;
      wire `default_theme` into `theme.tsx` (still allow per-session
      localStorage override), `default_time_range` into the
      `useTimeRange` initial value, `display_name` into the brand
      bar (replaces the literal "FlowScope.").

**[Session D2] Retention** (harder — schema-level):

- [ ] Init container reads `flow_retention_days` and
      `counter_retention_days` and emits
      `ALTER TABLE flows MODIFY TTL toDateTime(observed) + INTERVAL N DAY`
      before the api accepts traffic.
- [ ] Confirm ClickHouse handles a TTL change against a non-empty
      table without rewriting all parts; add a runbook note if it
      stalls.
- [ ] Audit the change so operators can see when retention shifted.

### [Session E] OIDC login flow (Phase 2)

**Goal:** the `oidc_config.enabled` flag actually gates routes;
operators sign in with Entra ID instead of pasting tokens.

This is its own multi-session effort and overlaps with the Phase 2
work in VISION.md §7. Outline only:

- [ ] Entra app registration flow (operator-side runbook).
- [ ] `/auth/login` and `/auth/callback` handlers; PKCE; state cookie.
- [ ] JWT validation against the issuer's JWKS; subject + groups
      extraction.
- [ ] Session cookie (HttpOnly, Secure, SameSite=Lax, signed under a
      new master key — distinct from `FLOWSCOPE_SNMP_KEY`).
- [ ] Middleware extension: `authz.Config` accepts a Session source
      alongside SharedToken / Tokens.
- [ ] Drop the "Phase 2" banner once the flag actually gates.
- [ ] RBAC (Big rocks below) is the natural follow-up.

---

## Big rocks (Phase 4+)

- [~] OIDC SSO with Entra ID — config form persists today (PR #15);
      see [Session E] above for the actual flow.
- [ ] RBAC (read-only / admin / per-exporter-group)
- [ ] Multi-appliance federation (one query, multiple sites)
- [ ] GeoIP / IRR / threat-intel enrichment
- [ ] Capacity-planning reports (90-day interface trend → "this link
      will saturate in N weeks")
