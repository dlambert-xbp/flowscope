# FlowScope — Enterprise Network Observability, Done Right

> *Written by an engineer who has paid the SolarWinds bill, fought NNMi licensing, lived through ScienceLogic upgrades, and watched Kentik invoices climb a comma every year. This is the tool the industry has owed itself for a decade.*

---

## 1. Vision

A self-hostable, Azure-native appliance that gives an enterprise network team **one pane of glass** for everything that matters:

- **Flow** — NetFlow v5, NetFlow v9, IPFIX, sFlow v5
- **State & inventory** — SNMP v2c/v3 (on-demand, not blanket polling)
- **Streaming telemetry** — gNMI dial-in / dial-out, OpenConfig models
- **Synthetic correlation** — BGP session state, interface up/down, capacity headroom

It must be:

- **Polished** — looks and feels like a product, not a homelab project
- **Stable** — runs for months untouched; survives device floods, malformed packets, slow disks, node failures
- **Pollerless by default** — receivers, not interrogators. SNMP is a fallback, not a workhorse.
- **Self-hostable** — runs in the customer's Azure tenant. No SaaS dependency, no per-flow billing, no agents on devices, no flow data ever leaves the customer's environment.
- **Highly available** — UDP load balancer fronting stateless ingest replicas; storage tier replicated. Single-node failures cause no data loss and no operator action.
- **Operable by a small team** — managed via Bicep/Terraform, upgrades via versioned container images, observability of the observer is built in.

This is a deliberate rejection of the current enterprise norm: six disparate tools, three license servers, a Java console from 2011, and a quarterly invoice that funds someone else's yacht.

---

## 2. Why this exists

Current commercial offerings each fail in at least one of these ways:

| Tool class | Failure mode |
|---|---|
| Legacy NMS (SolarWinds Orion, NNMi, WhatsUp) | Poll-every-5-minutes mindset, Windows-only stacks, plugin sprawl, eight-figure refresh cycles |
| Flow analyzers (Plixer, ManageEngine NetFlow Analyzer) | Per-exporter licensing, slow drilldowns, dated UI |
| SaaS observability (Kentik, ThousandEyes, Auvik) | Requires shipping flow off-prem, recurring cost scales with traffic, opaque retention |
| Open source (LibreNMS, ntopng, Akvorado, Elastiflow) | Each does one slice well; integrating four of them is a part-time job |

FlowScope's job is to deliver **80% of all four categories** in one cohesive product, with the remaining 20% being intentional scope discipline (e.g., we will not become a SIEM, we will not do APM).

---

## 3. Architectural pillars

### 3.1 Pollerless-first

The platform is **receiver-shaped**:

- Flow exporters push to us (UDP 2055 / 6343 / 4739)
- gNMI dial-in subscriptions stream changes once subscribed
- sFlow counter samples replace SNMP for interface throughput on capable hardware

SNMP is reserved for:

- Initial device discovery / inventory (sysDescr, ifTable, lldpRemTable)
- Filling gaps where streaming telemetry is unavailable
- Triggered, on-demand walks (e.g., "user opened device page → fetch live ARP table")

We never run a fleet-wide 5-minute SNMP poll. That model is why every legacy NMS feels slow — they are perpetually one cycle behind reality, and they bury switches under getbulk traffic.

### 3.2 Stack and modular service shape

The product is one cohesive system delivered as a small set of components, each with a single responsibility:

| Component | Language / runtime | Role |
|---|---|---|
| `ingest` | Go 1.23+ | Stateless UDP receivers (NetFlow v5/v9, IPFIX, sFlow v5). Decode → canonical record → write path. |
| `api` | Go 1.23+ | HTTP/JSON REST + WebSocket for live streams. Reads ClickHouse + in-memory hot state. |
| `alert` | Go 1.23+ | Rule evaluation, notification fan-out (webhook/email/syslog), dedup + grouping. |
| `snmp` | Go 1.23+ | On-demand walks via gosnmp (v2c/v3). Encrypted credential store. |
| `gnmi` | Go 1.23+ | Dial-in/dial-out subscriptions via openconfig/gnmic. |
| `web` | TypeScript / React 19 / Vite | Single-page application, design system, all dashboards. |
| `clickhouse` | ClickHouse | Warm + cold storage, queryable as one logical table via TTL tiering. |

The Go backend is one repo, multiple packages. Components run as separate processes only where the operational model demands it (ingest replicas behind a UDP LB; API replicas behind an HTTP LB). Internal interfaces are stable Go interfaces, not network RPC, so refactoring across module boundaries is cheap.

The reasoning is operational: we are no longer pretending one engineer with vim is the maintenance model. The product must be maintainable by a small team with proper CI, typed contracts, and tests.

### 3.3 Storage tiering

Two physical tiers, one logical view:

| Tier | Store | Retention | Purpose |
|---|---|---|---|
| **Hot** | In-process ring (Go channel-fed deque, ~5k entries per ingest replica) | seconds | Sub-second live views before ClickHouse commits land |
| **Warm + Cold** | ClickHouse with TTL policies — `flows`, `iface_counter_samples`, `events` | 7 days local SSD → 90+ days S3/ADLS via TTL move | Historical drilldown, top-N aggregations, forensics, capacity planning |

ClickHouse handles the warm/cold split transparently via partition TTL clauses: hot partitions live on premium SSD, age out to blob storage, all queryable as one table. No external mover, no Parquet rotation logic, no DuckDB swap.

**Key invariant:** sFlow / gNMI counter samples are *authoritative* for interface throughput. Flow-derived totals are estimates. Anywhere the UI shows a rate, it prefers counter-sample diffs and falls back to flow buckets, labeling the source explicitly (`counters` | `gnmi` | `flows`).

### 3.4 Concurrency and HA model

- Each `ingest` replica owns its UDP listeners. A UDP load balancer (Azure Standard LB) fronts replicas with 5-tuple hashing so a given exporter consistently lands on one replica — keeps NetFlow v9 template state local to that replica.
- Within a replica: one goroutine per listener, fan-in via buffered channels into a worker pool of parsers, single canonical write path (`record.Emit`) into the hot ring and the ClickHouse batch writer.
- ClickHouse is run in a 3-node replicated configuration (or managed via ClickHouse Cloud / Altinity on Azure). Ingest replicas write through the cluster; failures of any single node are transparent.
- `api` replicas are stateless; they hold no local state beyond a brief query cache. Scale horizontally behind an HTTP LB.
- `alert` runs as a singleton with leader election (ClickHouse Keeper or a small lease in the same store). Rules evaluate on a single replica at a time to avoid duplicate notifications.

No shared mutable global outside of explicitly synchronized stores. New state crosses goroutines via channels or a `sync.RWMutex`-guarded struct, with a comment justifying it. Race detector is on for every CI run.

---

## 4. Data ingestion in detail

### 4.1 Flow

- **NetFlow v5** — fixed format, simplest path
- **NetFlow v9 / IPFIX** — template-driven; templates cached per `(exporter, source_id, template_id)` per ingest replica. Unknown fields are skipped by declared length, never guessed.
- **sFlow v5** — flow samples decoded from the raw packet header (Ethernet → VLAN → IPv4/IPv6 → TCP/UDP). Counter samples are the throughput backbone.

We extend `goflow2` parsers as a base; every parsed record funnels into a single `record.Emit` fan-out so there is exactly one place to instrument, rate-limit, or extend.

### 4.2 SNMP

- v2c and v3 supported via `gosnmp/gosnmp`
- v3 passphrases encrypted at rest with AES-256-GCM, key derived via HKDF-SHA256 from a master key sourced from Azure Key Vault (via Managed Identity) at startup
- Per-device polling cadence stored in the binding, not hard-coded
- Failures bypass the interval so flapping devices retry on the next scheduler tick
- In-flight walks are de-duplicated so a slow device cannot stack work
- Mock client behind a build tag for dev without a lab

### 4.3 gNMI

- Dial-in subscriptions to `/interfaces/interface/state/counters`, `/network-instances/.../bgp/.../state`, `/components/component/state`
- TLS with cert-based auth; credentials live in the same encrypted store as SNMP v3
- Falls back to SNMP automatically if a device does not advertise gNMI
- Dial-out collector for vendors that prefer push-from-device

### 4.4 BGP state

Two acquisition paths, in preference order:

1. gNMI subscription to OpenConfig BGP neighbor state (instant, push)
2. SNMP `bgpPeerTable` walk on demand (compatibility)

Up/down transitions feed the alert engine.

---

## 5. UI — the part nobody else gets right

The web UI is a React 19 + TypeScript application built with Vite. It uses Tailwind, shadcn/ui primitives, TanStack Query for data, Recharts for time-series, and react-flow for topology. The API contract is typed end-to-end (OpenAPI spec → generated TS client) so the frontend cannot drift from the backend silently.

All dashboards must:

- Update local UI state on click *before* the network request resolves — never let a 200ms fetch make the dashboard feel broken.
- Use in-app modals — never `window.prompt/confirm/alert`.
- Survive empty data (zero exporters, zero flows) with no console exceptions.
- Meet WCAG 2.2 AA on color contrast, focus order, keyboard navigation.

### 5.1 Overview tab — "Is the system healthy?"

The first thing a NOC sees. Six tiles, refreshed every 2 seconds via WebSocket push:

1. **Flow rate** — flows/sec, packets/sec, bytes/sec (line graph, last 15 min)
2. **Device polling latency** — p50 / p95 / p99 SNMP RTT per device, plus gNMI subscription lag
3. **Exporter health** — count of exporters seen in last 60s vs configured; red tile if any are silent
4. **Receiver health** — UDP socket buffer drops, parser exceptions/min, template cache hit rate, per-replica
5. **Storage** — ClickHouse insert lag, oldest/newest flow timestamp, disk free per node, replication health
6. **Alert summary** — open alerts by severity, MTTA on closed alerts last 24h

This tab is the answer to *"is FlowScope itself working?"* — a question every other vendor forces you to answer with five separate consoles.

### 5.2 Flows tab — "Look at all view"

A grid of cards, every card a top-N panel:

- Top Talkers (src/dst pairs, by bytes and by packets)
- Top Protocols (TCP / UDP / ICMP / GRE / ESP / etc.)
- Top Services (well-known port + heuristic mapping; e.g., 443 → HTTPS, 22 → SSH)
- Top BGP Connections (when BGP context joined to flows via FIB lookup)
- Top ASNs (when GeoIP / IRR enrichment enabled)
- Top Conversations (5-tuple)

Every row in every card is **clickable**:

- Click a talker → drilldown view filtered to that src/dst, with the rest of the cards re-aggregated within the filter
- Click a service → same, scoped to that port/protocol
- Filter chips at the top of the tab show the active filter set; click an `x` to remove

The drilldown UX is the differentiator. Most tools make you start over with a query builder. We just refilter in place — the URL holds the filter state, so deep-linking and sharing a view both work.

### 5.3 Devices tab — "Tell me everything about this box"

Tree of exporters on the left, device detail on the right. Device detail has sub-tabs:

- **Summary** — sysDescr, sysObjectID, uptime, location, contact, software version (gNMI / SNMP), serial, last-seen, first-seen
- **Interfaces** — table with admin/oper status, MTU, MAC, in/out errors, in/out discards, ingress/egress rate. Click a row → modal with per-interface counter-sample timeseries + flow breakdown for that interface
- **Flows** — recent flows touching this exporter, with the same filter-as-you-click model from the Flows tab
- **Neighbors** — LLDP / CDP topology view, links rendered via react-flow
- **BGP** — neighbor table, session state, prefixes received/advertised, last state change
- **Health** — CPU, memory, temperature, fan/PSU state when available via gNMI / entitySensor MIB
- **Live** — on-demand panels: ARP table, MAC table, route table. SNMP/gNMI fired only when the user opens the panel.

Everything cross-links. From a flow row → the source device. From an interface → the flows traversing it. From a BGP neighbor → the device on the other end if it is also under FlowScope.

### 5.4 Alerts tab

- Open / acknowledged / closed views
- Per-alert timeline with the underlying samples that triggered it
- Acknowledgement notes, assignee
- Webhook delivery log per alert (for proving Pagerduty / Slack actually fired)
- Runbook field rendered as markdown

---

## 6. Alerting

A small, opinionated rule engine. **No DSL, no YAML inheritance hierarchy**, no Grafana-style alert manager labyrinth. Rules are JSON objects with five fields: `metric`, `scope`, `condition`, `for`, `severity`.

Built-in rule types:

- **Interface up/down** — oper_status transitions, with debounce (`for: 30s`)
- **Interface utilization** — > X% of ifSpeed for Y duration
- **Errors / discards** — rate-of-change thresholds, not absolute (counters reset on reboot)
- **Exporter silence** — no flow received from exporter for N seconds
- **BGP neighbor state change** — Established → anything else
- **Device unreachable** — gNMI subscription dropped *and* SNMP ping fails
- **Flow anomaly** — top-talker delta vs. trailing 7-day baseline (Phase 3)

Notification channels:

- Webhook (Slack, Teams, PagerDuty, Opsgenie via their respective inbound formats)
- Email (SMTP relay only, no built-in MTA)
- Syslog (so the existing SIEM can ingest)

**Alert hygiene is a feature.** Every alert has:

- A *runbook* field (markdown, rendered in the alert detail)
- An auto-close condition (so flapping interfaces do not leave 4,000 stale alerts)
- A grouping key so 200 BGP neighbors going down with one upstream are *one* incident

The `alert` service runs as a singleton via leader election. Rule evaluation is idempotent so a fail-over does not double-fire notifications.

---

## 7. Authentication & multi-tenancy

- **Phase 1**: API token via `X-Auth-Token` header, constant-time comparison; static SPA shell ungated, browser prompts on first 401 and caches in `sessionStorage`. Backend reads token from Key Vault.
- **Phase 2**: OIDC (Entra ID first) for SSO. Layered alongside the token — token remains for machine clients (CI, scripted exports).
- **Phase 3**: Read-only / admin / per-exporter-group RBAC, claims-driven from OIDC.
- TLS is **always** terminated at a reverse proxy (Azure Application Gateway, nginx, or Caddy in front of the cluster). FlowScope does not ship its own TLS stack — operating one well is a full-time job.

---

## 8. Deployment in Azure

### 8.1 Reference architecture

```
                ┌──────────────────────────────────┐
   Exporters ──▶│  Azure Standard LB (UDP, 5-tuple)│
                └──────┬─────────────────┬─────────┘
                       ▼                 ▼
                ┌──────────────┐   ┌──────────────┐
                │ ingest       │   │ ingest       │   stateless Go
                │ (replica 1)  │   │ (replica 2)  │   2+ replicas
                └──────┬───────┘   └──────┬───────┘
                       └─────┬───────────┘
                             ▼
                ┌──────────────────────────────────┐
                │ ClickHouse cluster (3 replicas)  │   replicated, TTL-tiered
                │ + ClickHouse Keeper              │   warm SSD → cold blob
                └──────┬───────────────────────────┘
                       ▼
                ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
                │ api          │  │ alert        │  │ snmp / gnmi  │
                │ (replica N)  │  │ (singleton)  │  │ (replica N)  │
                └──────┬───────┘  └──────────────┘  └──────────────┘
                       ▼
                ┌──────────────────────────────────┐
                │ Azure App Gateway (TLS, OIDC)    │
                └──────┬───────────────────────────┘
                       ▼
                ┌──────────────────────────────────┐
                │ React SPA (static, served by api │
                │  or hosted on Azure Static Web)  │
                └──────────────────────────────────┘
```

### 8.2 Hosting

- **AKS** is the canonical deployment target. Helm chart published per release; values file is the customer's only configuration surface.
- **Azure Container Apps** is supported for smaller deployments (≤25k fps) where AKS is overkill.
- ClickHouse: managed (ClickHouse Cloud, Altinity) is the default recommendation. Self-hosted on Azure VMs / AKS StatefulSet is supported and documented.
- Cold blob storage: Azure Blob (or ADLS Gen2) lifecycle-managed by ClickHouse TTL policies.
- Identity: Workload Identity for pod-to-Key-Vault, no static secrets in env vars or files.

### 8.3 Sizing reference

| Scale | Ingest replicas | ClickHouse | api replicas |
|---|---|---|---|
| ≤ 10k fps | 2 × 2 vCPU / 4 GiB | 3 × 4 vCPU / 16 GiB / 1 TB SSD | 2 × 1 vCPU |
| ≤ 50k fps | 2 × 4 vCPU / 8 GiB | 3 × 8 vCPU / 32 GiB / 2 TB SSD | 3 × 2 vCPU |
| ≤ 200k fps | 4 × 8 vCPU / 16 GiB | 3 × 16 vCPU / 64 GiB / 4 TB SSD | 4 × 4 vCPU |

Cold tier sized to retention target; ClickHouse compression typically yields 10:1 on flow data.

### 8.4 Upgrades

- Helm upgrade with a new chart version. Deployments use rolling strategies; a single-replica restart never drops more than the kernel UDP buffer of one node.
- ClickHouse schema migrations are forward-only, idempotent, and run by a one-shot init container before the new version's pods receive traffic.
- No external migration tool, no Alembic, no DBA in the loop.

### 8.5 Observability of the observer

- Prometheus `/metrics` on every Go service (receiver counters, parser errors, ClickHouse insert lag, alert engine evaluation time)
- The Overview tab consumes the same metrics for human-facing self-monitoring
- Optional: ship metrics to Azure Monitor via the Azure Monitor agent if the customer is already standardized on it
- Structured JSON logs at `info` by default, `debug` togglable per service via runtime env

---

## 9. What we will deliberately *not* build

Saying no is half of staying polished.

- ❌ Application performance monitoring (APM) — Datadog/New Relic territory
- ❌ Log aggregation / SIEM — Splunk/Sentinel territory
- ❌ Configuration management — Ansible/NSO territory
- ❌ A custom query DSL — ClickHouse SQL is exposed for power users; clickable filters for everyone else
- ❌ A mobile app — the web UI is responsive; a native app is a tax on the maintainer
- ❌ Plugins / marketplace — the entire reason commercial NMS tools rot. We ship features in core or not at all.

Multi-region active/active federation is in scope (Phase 4) — we accepted that "solid product" includes serving MSPs and multi-site enterprises.

---

## 10. Roadmap

### Phase 1 — *Foundations on the new stack*
- Go ingest service: NetFlow v5/v9, IPFIX, sFlow v5 parsers (`goflow2`-derived), single `record.Emit` fan-out
- ClickHouse schema: `flows`, `iface_counter_samples`, `events`; TTL-tiered (7d hot SSD → 90d cold blob)
- Go api service: REST + WebSocket; OpenAPI spec; typed TS client generation
- React SPA: Overview, Flows, Devices, Interfaces tabs at parity with current product
- SNMP v2c/v3 via gosnmp; AES-256-GCM credential store with Key Vault master key
- Token auth, Helm chart for AKS, single-cluster deployment, Prometheus `/metrics`
- CI: `go test -race`, `golangci-lint`, `vitest`, `playwright` e2e, container build + sign

### Phase 2 — *Operations-grade*
- Alert engine (singleton service, leader-elected) with built-in rule types and webhook/email/syslog channels
- BGP neighbor table ingestion (SNMP first, gNMI when available)
- LLDP/CDP topology view, react-flow rendering
- OIDC SSO (Entra ID)
- ClickHouse cold-tier validated at 90+ day retention on real customer scale

### Phase 3 — *Streaming telemetry*
- gNMI dial-in subscriptions (openconfig/gnmic), OpenConfig models
- gNMI dial-out collector for devices that prefer push
- Auto-promotion: when a device advertises gNMI, scheduler stops issuing duplicate SNMP polls for the overlapping OIDs
- Flow anomaly detection (top-talker delta vs trailing 7-day baseline), evaluated by alert engine

### Phase 4 — *Scale and federation*
- RBAC: read-only / admin / per-exporter-group, claims-driven
- Multi-appliance federation — one query, multiple sites, signed HTTPS read fan-out
- IRR / GeoIP / threat-intel enrichment (file-based, no flow-time network calls)

### Phase 5 — *Polish & long-tail*
- Capacity-planning reports (90-day interface trend → "this link will saturate in N weeks")
- ClickHouse → external archival exporter for compliance retention beyond the appliance
- Stable, versioned REST contract with published OpenAPI spec

---

## 11. Operating principles

These are non-negotiable, learned-the-hard-way rules:

1. **99% confidence before changing anything.** If a change is not provably correct and complete, document the doubts and let the operator decide. A confident plan with caveats beats a guess that compiles.
2. **Render on state change.** Click handlers update local React state and trigger render *before* fetches resolve. A dashboard that feels broken for 200ms is a dashboard the user does not trust.
3. **Counter samples win.** When sFlow/gNMI counters and flow-derived rates disagree, counters are truth. The UI labels the source so the operator never wonders.
4. **One fan-out point per data type.** All flows go through `record.Emit`. All SNMP requests go through the worker pool. All alerts evaluate in one place. New features extend these, they do not bypass them.
5. **Logs over metrics over traces** — in that priority order for the maintainer's debugging needs. We instrument what helps a human at 2 AM.
6. **No silent failures.** A dropped UDP packet, a malformed template, a failed SNMP poll — every one increments a counter visible on the Overview tab and on `/metrics`. You cannot fix what you cannot see.
7. **Typed contracts at every boundary.** Go interfaces internally; OpenAPI-generated TS client between api and web. The compiler catches drift, not the customer.
8. **Tests gate merges.** Golden packet captures for every parser version, race-detector on every CI run, Playwright e2e for the critical user journeys. "Stable, runs for months" is enforced by automation, not hope.
9. **Deployable infrastructure-as-code.** Bicep or Terraform module published per release. The customer's only manual step is `helm install` with a values file.

---

## 12. The bottom line

This is a tool built by someone who has had to explain to a CFO why the network monitoring renewal is six figures, while the actual signal — *"is the network up, and where is the traffic going?"* — could fit in a small Azure cluster.

FlowScope's promise is simple:

> **One product. One UI. Live, historical, alerting. No agents. No per-flow license. No quarterly invoice. No yacht.**

If we hold the line on scope, polish, and the architecture above, this is the tool the industry has owed itself since the day NetFlow shipped.
