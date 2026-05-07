# FlowScope

Self-hostable enterprise network observability — flow, state, and (soon)
streaming telemetry — in one cohesive product. Receivers, not interrogators.

- **Flow ingest** — NetFlow v5, NetFlow v9, IPFIX, sFlow v5
- **State & inventory** — SNMP v2c / v3, on-demand walks (no fleet-wide polling)
- **Alert engine** — rule evaluation with dedup + grouping, per-event ledger in ClickHouse
- **Dashboard** — React 19 SPA, themed, brand bar + tabs (Overview, Flows, Devices, Alerts, Settings)
- **Storage** — ClickHouse warm tier today; cold-tier policy and 3-replica + Keeper coordination on the roadmap

The product vision and the *why* live in [VISION.md](VISION.md). The
developer-facing layout and rules of the road are in [CLAUDE.md](CLAUDE.md).
The build-and-run manual is [BUILD.md](BUILD.md). Open work is in
[TASKS.md](TASKS.md).

> The original Python single-file collector lives on the [`v1` branch](https://github.com/dlambert-xbp/flowscope/tree/v1).
> This README describes the v1 (Go + ClickHouse + React) line that supersedes it.

---

## Quick start

Brings up ClickHouse + ingest + snmp + alert + api + web with one command.
Synthetic traffic in a second terminal so all the views populate immediately.

```bash
docker compose up --build
```

Wait for these log lines:

```
flowscope-clickhouse  | <Information> Application: Ready for connections
flowscope-ingest      | clickhouse migrations applied
flowscope-ingest      | flowscope ingest started
flowscope-api         | flowscope api started addr=:8080
```

Then in a second terminal, drive synthetic NetFlow v9 + sFlow at the
collector:

```bash
go run ./cmd/synth -- --target localhost:2055 --rate 5000 --duration 60s
```

Open the dashboard:

- **Web UI** — <http://localhost/>
- **JSON API** — <http://localhost:8080/api/summary>
- **Prometheus metrics** — `:9100` (ingest), `:9101` (alert), `:9102` (snmp)

To shut down (preserving ClickHouse data): `docker compose down`.
To wipe data too: `docker compose down -v`.

---

## What's running

```
                +---------------+         +-----------------+
  switches ---> |  cmd/ingest   | ----->  |   ClickHouse    |
  (UDP 2055,    |  (Go)         |   ┌->   |  flowscope.*    |
   6343)        +---------------+   │     +-----------------+
                                    │              ^
                +---------------+   │              │
   operator --> |   cmd/snmp    | --┘     ┌--------+--------┐
   (per-       |   (Go)         |         │                 │
    exporter   +---------------+   +-----------+    +--------------+
    creds)                          | cmd/alert |    |   cmd/api    |
                                    |  (Go)     |    |  (Go, :8080) |
                                    +-----------+    +--------------+
                                                            ^
                                                            │  REST + WS
                                                            │
                                                       +----+-----+
                                                       |   web    |
                                                       | (React,  |
                                                       |  :80)    |
                                                       +----------+
```

| Component | Port | Purpose |
|---|---|---|
| `cmd/ingest` | 2055/udp, 6343/udp, 9100/tcp | UDP receivers + ClickHouse batcher; `record.Emit` is the single fan-out point |
| `cmd/api`    | 8080/tcp | JSON REST; reads ClickHouse + serves a fallback live HTML at `/` |
| `cmd/alert`  | 9101/tcp | Rule evaluation tick (default 10s); writes `alert_events` ledger |
| `cmd/snmp`   | 9102/tcp | On-demand walks via gosnmp; encrypted v2c/v3 credential store |
| `web`        | 80/tcp | React 19 + Vite SPA, nginx-served, `/api/*` proxied to api:8080 |

There is no `cmd/gnmi` yet — Phase 3 in [VISION.md](VISION.md).

---

## Configuring exporters

Replace `<collector-ip>` with the IP of the host running FlowScope.

### sFlow on Arista (recommended — hardware-accelerated, real packets)

```
configure
sflow source-interface Management1
sflow destination <collector-ip> 6343
sflow polling-interval 10
sflow sample 10000
sflow run
end
```

`polling-interval 10` makes the switch send absolute interface byte/packet
counters every 10 seconds. FlowScope diffs successive counter samples to
produce authoritative bytes/sec on the Interfaces tab.

### NetFlow v9 / IPFIX on Arista

```
flow tracking hardware
   tracker FLOWSCOPE
      record export on inactive timeout 15
      record export on interval 60
      exporter FLOWSCOPE_EXP
         collector <collector-ip> port 2055
         format ipfix version 10
         template interval 5000
   no shutdown
!
interface Ethernet1
   flow tracker hardware FLOWSCOPE
```

### Cisco IOS / IOS-XE — NetFlow v9

```
flow exporter FLOWSCOPE
 destination <collector-ip>
 transport udp 2055
 template data timeout 60
!
flow monitor FM
 exporter FLOWSCOPE
 record netflow ipv4 original-input
!
interface GigabitEthernet0/1
 ip flow monitor FM input
 ip flow monitor FM output
```

### Linux test source (no switch needed)

```bash
sudo apt install softflowd
sudo softflowd -i eth0 -n <collector-ip>:2055 -v 9
```

### Synthetic traffic (no network needed at all)

`cmd/synth` simulates two switches and a router sending realistic-looking
mixed NetFlow v9 + sFlow (flow + counter samples), so every tab populates:

```bash
go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 5000 --duration 60s
```

---

## REST API

Source of truth: each handler in [cmd/api/handlers.go](cmd/api/handlers.go).
`api/openapi.yaml` is on the roadmap. All endpoints return JSON; most accept
`?exporter=<ip>` to filter to a single exporter.

| Path | Description |
|------|-------------|
| `GET /api/summary?window=300s` | Overview aggregates over a trailing window |
| `GET /api/devices` | All exporters seen, with last-seen and flow counts |
| `GET /api/devices/{exporter}` | Single device + counters |
| `GET /api/devices/{exporter}/inventory` | SNMP-derived sysDescr / ifTable |
| `GET /api/interfaces` | Per-(exporter, ifIndex) ingress/egress totals |
| `GET /api/interfaces/{exporter}/{ifindex}/timeseries?seconds=N` | Bucketed bytes/sec; response carries `"source": "counters" \| "flows"` |
| `GET /api/flows/recent?limit=200` | Most recent flow records |
| `GET /api/top/talkers` / `/top/services` / `/top/protocols` / `/top/conversations` | Top-N panels |
| `GET /api/alerts`, `/api/alerts/summary` | Alert ledger reads |
| `POST /api/alerts/{id}/ack`, `POST /api/alerts/{id}/close` | Alert state transitions |
| `GET /api/snmp/credentials`, `PUT /api/snmp/credentials/{exporter}` | Per-exporter v2c / v3 credential CRUD (encrypted at rest) |
| `POST /api/snmp/credentials/{exporter}/test` | Validate a credential without saving |

WebSocket live-stream (`/api/stream`) is described in [VISION.md](VISION.md) §3
but not yet implemented — the SPA polls today.

---

## Status — honest snapshot (May 2026)

**Working end-to-end on the local docker compose stack:**

- NetFlow v5 + v9 + IPFIX parsing with template cache
- sFlow v5 flow + counter samples; counter-diff timeseries with flow fallback
- ClickHouse warm tier (7-day TTL) with idempotent migrations
- API surface above, plus per-interface timeseries and per-exporter filtering
- React 19 SPA: Overview, Flows (with filter chips), Devices (directory + per-device sub-tabs), Alerts, Settings
- Alert engine with two built-in rules (`exporter_silent`, `heavy_talker`), dedup + grouping, ack/close from UI
- SNMP v2c walks; v3 USM walks (USM stack still needs a real-lab handshake test)
- Per-exporter encrypted credential storage (AES-256-GCM under HKDF-SHA256)
- 49 Go unit tests; race detector runs clean on Linux CI hosts

**Not yet built / known limitations:**

- **No auth** on `/api/*` — `X-Auth-Token` is the Phase-1 plan; today every endpoint is open
- **Master key in env var** — `FLOWSCOPE_SNMP_KEY` is a literal in `docker-compose.yml`. `internal/secrets` (env / file / Key Vault) is not yet implemented
- **No notification fan-out** — engine writes the ledger, but webhook / email / syslog workers are not wired
- **Single-replica everything** — no leader election in `cmd/alert` (two replicas would dupe-fire), no UDP load balancer in front of ingest, single-node ClickHouse, no cold-tier policy
- **No `cmd/gnmi`** — streaming telemetry ingest is Phase 3
- **No Helm chart** — compose is dev-only; production needs the Helm chart described in [VISION.md](VISION.md) §8
- **No CI workflow files** — `golangci-lint`, `go test -race`, `vitest`, `playwright`, `helm lint` are intended gates but `.github/workflows/` is empty
- **OpenAPI spec missing** — `api/openapi.yaml` is the documented source of truth but doesn't exist yet; the TS client is hand-written in [web/src/api.ts](web/src/api.ts)
- **No Playwright e2e** — the contract test in CI doesn't exist
- **No integration tests** against a real ClickHouse — testcontainers-go is on the list

The full open-tasks list with prioritization is in [TASKS.md](TASKS.md).

---

## Repository layout

```
.
├── cmd/                          # One main.go per binary
│   ├── ingest/  api/  alert/  snmp/  synth/
├── internal/                     # Shared Go packages
│   ├── record/    # canonical Flow + Emit fan-out
│   ├── netflow/   # v5 + v9/IPFIX parsers, template cache
│   ├── sflow/     # sFlow v5 (flow + counter samples)
│   ├── store/     # ClickHouse client + batchers + migrations
│   ├── snmpx/     # gosnmp wrapper, encrypted credential store, scheduler
│   ├── alerteng/  # rule engine + built-in rules
│   └── obs/       # Prometheus + structured logging
├── web/                          # React 19 + Vite SPA
├── deploy/
│   ├── clickhouse/               # users.d for the local container
│   └── infra/                    # Bicep + cloud-init for the Azure VM path
├── docker-compose.yml
└── go.mod
```

---

## License

TBD — pick one before first external release.
