# FlowScope — Open Tasks

Living TODO list, ranked by priority. Done items are deleted on commit
per the note at the bottom of CLAUDE.md.

Last updated: 2026-05-15.

> **Session label:** items prefixed with `[Session X]` are scoped to fit
> one focused work session. Pick one when you sit down — clear start,
> end, and PR shape. Detail blocks for each open session live below.

---

## P0 — production blockers, ship-gating

1. [~] **Auth on `/api/*` read endpoints.** PR #15 gated `/api/settings/*`
       and SNMP write endpoints behind `X-Auth-Token`. Reads
       (`/api/summary`, `/api/flows/*`, `/api/devices/*`, `/api/alerts*`,
       `/api/top/*`, `/api/services/*`) remain open behind the proxy.
       Closing those finishes Phase 1 auth.
2. [ ] **Master key resolution via Azure Key Vault.** File-backed refs
       (`FLOWSCOPE_SNMP_KEY_REF=file:/run/secrets/snmp_master`) ship in
       compose today; the `kv:` resolver in `internal/secrets` still
       needs Workload Identity wiring and a values.yaml story. Until
       this lands, prod has no way to consume the SNMP master without
       baking it into a K8s Secret by hand.
3. [ ] **Helm chart shipping gaps.** `deploy/helm/flowscope/templates/`
       has Deployments for `alert/api/ingest` + the `init` Job, but:
   - **No `snmp-deployment.yaml`.** `cmd/snmp` exists (BGP, LLDP,
     scheduler, per-device alerting) and would be silently absent
     from any k8s install today.
   - **No `Service` manifests.** Nothing inside the cluster can reach
     api/ingest/alert by DNS, and ingest UDP needs `type=LoadBalancer`
     to receive flows from exporters.
   - **No HPA / PDB.** CLAUDE.md's working-agreement requires both
     where applicable.

   `helm lint` passes only because the chart never claims these. Until
   they exist, "deploy via Helm" is a misleading bullet on the README.

## P1 — high value, concrete scope

4. [ ] **[Session C] Wire exporter allowlist into ingest.** UI persists
       `exporter_allowlist`; ingest doesn't read it. Easy security win.
5. [ ] **Alert detail modal** — timeline of underlying samples that
       triggered + linked flows. Highest-leverage operator UX next.
6. [ ] **More built-in rules.** Today: `exporter_silent` + `heavy_talker`
       + BGP neighbor state. Roadmap from VISION.md §6:
   - Interface up/down (oper_status transitions, debounce) — first
   - Interface utilization > X% of ifSpeed (SNMP gives ifSpeed, sFlow
     gives bps — feasible now)
   - Errors / discards rate-of-change
   - Device unreachable (gNMI dropped AND SNMP fails)
   - Flow anomaly (top-talker delta vs trailing 7-day baseline)
7. [ ] **[Session D2] Retention TTL.** Init container reads
       `flow_retention_days` / `counter_retention_days` and emits
       `ALTER TABLE … MODIFY TTL` before api accepts traffic. Schema-
       migration risk against non-empty tables — needs care.
8. [ ] **Integration tests via testcontainers-go.** `internal/alerteng`
       has zero tests because they want a live ClickHouse. Today's unit
       tests don't touch a DB.

## P2 — quality of life

9. [ ] ⌘K command palette. Brand bar shows the kbd hint; not wired.
10. [ ] Per-rule alert history view ("how often does `exporter_silent`
        fire for this device this week?").
11. [ ] Grouping key for incident correlation. Engine writes `group_key`
        on each event; api treats every alert as independent.
12. [ ] NetFlow v9 / IPFIX enterprise fields and options templates —
        decoded but skipped today.
13. [ ] Bulk import of credentials. CSV upload or `flowscope snmp import`
        CLI for fleets.
14. [ ] SNMP master key rotation. Today, rotating invalidates every
        blob. Need a `re-encrypt under new master` admin action.
15. [ ] Web e2e via Playwright wired to ephemeral compose. Mentioned in
        CLAUDE.md as a CI gate but not wired.

## P3 — Phase 2/3 bets, multi-week

16. [ ] gNMI ingest (Phase 3 in VISION.md). Dial-in subscriptions,
        OpenConfig models, auto-promotion logic that retires SNMP
        polls when gNMI covers the same OIDs. CLAUDE.md still
        describes `cmd/gnmi/` and `internal/gnmix/` as if they exist
        — sync the doc when this lands (or before).
17. [ ] ClickHouse TTL → cold blob storage. Schema has 7-day delete TTL;
        need `TO VOLUME 'cold'` policy + ADLS / S3 storage policy.
18. [ ] 3-replica ClickHouse + Keeper coordination. Single-node today.
19. [ ] UDP load balancer fronting multiple ingest replicas. Architecture
        document describes the shape; compose only runs one.
20. [ ] SNMP v3 walk validation against a real lab. Crypto / store / API
        / UI all done; needs a real v3-only switch to confirm gosnmp's
        USM stack handshakes correctly.
21. [ ] **[Session E] OIDC login flow** (Phase 2 auth). Entra app
        registration, `/auth/login` + `/auth/callback`, JWT against
        JWKS, signed session cookie. Detail block below.
22. [ ] Golden tests for alert rules. Depends on #8
        (testcontainers-go).

---

## Session detail blocks

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

### [Session D2] Retention

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
operators sign in with Entra ID instead of pasting tokens. This is
its own multi-session effort and overlaps with the Phase 2 work in
VISION.md §7. Outline:

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
