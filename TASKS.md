# FlowScope — Open Tasks

Living TODO list. Items are grouped by tier — bugs and papercuts at the top,
phase 2/3 architecture at the bottom. Move things to the bottom of their
section as they're picked up; delete on commit.

Last updated: 2026-05-07.

---

## Bugs & papercuts (should fix soon)

- [ ] `/api/top/*` filter chips show raw keys (`dst_port · 443`) instead of
      human labels (`service · https`). The `label` field on `Filter` is
      plumbed but only set in some triggers. Cosmetic.
- [ ] Brand bar tab counts are stubbed (`—`, `—`, `0`). Should read live
      from `/api/summary`, `/api/devices`, `/api/alerts/summary`.
- [ ] Per-binding SNMP interval is stored but ignored — scheduler still
      uses cluster-wide `FLOWSCOPE_SNMP_INTERVAL`. Trivial fix in
      `Scheduler.dispatch`.
- [ ] No "walk now" button on the Settings → SNMP page. Operator waits up
      to 15 min for results from a new credential.
- [ ] Flow-independent SNMP discovery. `Scheduler.discoverExporters` only
      walks devices that have produced flows. Devices in the credentials
      table without flow history are silently ignored. Fix: union
      flow-observed exporters with credential-bound exporters.

## Production blockers

- [ ] **Auth on `/api/*`.** VISION.md §7 — `X-Auth-Token` (Phase 1) then
      OIDC SSO (Phase 2). Today every endpoint is open.
- [ ] **Alert engine leader election.** Single-replica today. Two
      `cmd/alert` instances would dupe-fire alerts. ClickHouse Keeper
      lock + leader-only writes.
- [ ] **Master key from Key Vault, not env var.** `FLOWSCOPE_SNMP_KEY`
      is a literal in `docker-compose.yml`. Build `internal/secrets` with
      env / file / Key Vault implementations and Workload Identity.
- [ ] **Notification channels.** Webhook (Slack / Teams / PagerDuty),
      email (SMTP relay), syslog. Engine already writes ledger rows;
      need a fan-out worker that watches `alert_events` for `opened`
      transitions.
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
- [ ] On-demand triggered walks. `POST /api/devices/{e}/snmp/walk`
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
- [ ] Auto-close after stability window. Currently the engine
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
- [ ] Variable-length IPFIX records. Currently records with an
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

- [ ] Light theme toggle. Tokens defined for light mode (used by the
      mock); needs a runtime switch.
- [ ] ⌘K command palette. Brand bar shows the kbd hint; not wired.
- [ ] j/k keyboard navigation on tables.
- [ ] Filter chips with human labels (see Bugs section).
- [ ] Deep-linking on Devices. Selecting a device updates `useState`
      not URL — refresh loses selection.

## Docs / ops

- [ ] `README.md` is still Python-era. Should describe the Go
      architecture and link to VISION.md / BUILD.md / CLAUDE.md.
- [ ] Helm chart for AKS. Compose works for dev; prod needs Helm per
      VISION.md §8.2.
- [ ] CI workflow files. GitHub Actions for `go test -race`,
      `golangci-lint`, `vitest`, `playwright`, `helm lint`, container
      build + sign.

## Big rocks (Phase 4+)

- [ ] OIDC SSO with Entra ID
- [ ] RBAC (read-only / admin / per-exporter-group)
- [ ] Multi-appliance federation (one query, multiple sites)
- [ ] GeoIP / IRR / threat-intel enrichment
- [ ] Capacity-planning reports (90-day interface trend → "this link
      will saturate in N weeks")
