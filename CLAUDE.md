# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This document is the developer-facing companion to [VISION.md](VISION.md). VISION.md describes *what* the product is and *why*; this file describes *how* the codebase is laid out and the rules for changing it.

## Stack at a glance

- **Backend services**: Go 1.23+ — `ingest`, `api`, `alert`, `snmp`, `gnmi`. One module, multiple internal packages, multiple binaries built from `cmd/<service>/main.go`.
- **Frontend**: React 19 + TypeScript + Vite. Tailwind + shadcn/ui + TanStack Query + Recharts + react-flow. Lives in `web/`.
- **Storage**: ClickHouse (warm + cold via TTL tiering). In-process Go ring buffer per ingest replica for sub-second hot views.
- **Deployment**: Helm chart targeting AKS or Azure Container Apps. Bicep/Terraform module for the Azure substrate (LB, AKS, ClickHouse, Key Vault).
- **Auth**: API token via `X-Auth-Token` (Phase 1) → OIDC/Entra ID (Phase 2). Master keys read from Azure Key Vault via Workload Identity, never from env vars in prod.

## Repository layout

```
.
├── cmd/                          # Service entrypoints (one main.go per binary)
│   ├── ingest/
│   ├── api/
│   ├── alert/
│   ├── snmp/
│   └── gnmi/
├── internal/                     # Shared Go packages, not importable externally
│   ├── record/                   # Canonical flow record + Emit fan-out
│   ├── netflow/                  # v5/v9/IPFIX parsers, template cache
│   ├── sflow/                    # sFlow v5 parser (flow + counter samples)
│   ├── store/                    # ClickHouse client, schema migrations, batchers
│   ├── snmpx/                    # gosnmp wrapper, encrypted credential store
│   ├── gnmix/                    # openconfig/gnmic wrapper, subscription manager
│   ├── alerteng/                 # Rule evaluation, dedup, grouping, channels
│   ├── secrets/                  # Key Vault / file / env loader, single interface
│   └── obs/                      # Prometheus metrics, structured logging helpers
├── api/openapi.yaml              # Source of truth for the REST contract
├── web/                          # React SPA (Vite project)
│   ├── src/
│   ├── package.json
│   └── vite.config.ts
├── deploy/
│   ├── helm/flowscope/           # Helm chart published per release
│   └── infra/                    # Bicep or Terraform for Azure substrate
├── test/
│   ├── golden/                   # Captured pcaps + expected records, per parser
│   └── e2e/                      # Playwright specs
└── go.mod
```

## Commands

```bash
# Backend
go build ./...                              # Compile all binaries
go test -race ./...                         # Unit tests with race detector (required before merge)
go test -race -tags=integration ./...       # Integration tests (hit a local ClickHouse)
golangci-lint run                           # Lint
go run ./cmd/ingest                         # Run ingest locally
go run ./cmd/api                            # Run api locally

# Frontend
cd web && npm install
cd web && npm run dev                       # Vite dev server with HMR
cd web && npm run build                     # Production bundle
cd web && npm run typecheck
cd web && npm run lint
cd web && npm run test                      # Vitest
cd web && npm run e2e                       # Playwright (requires backend running)

# Local stack via docker compose (ClickHouse + all services)
docker compose up --build

# OpenAPI client regen (after editing api/openapi.yaml)
make gen-client                             # → web/src/api/generated/

# Helm chart smoke test
helm lint deploy/helm/flowscope
helm template deploy/helm/flowscope > /tmp/rendered.yaml

# Synthetic traffic (Go tool, replaces the old synth_flows.py)
go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 50000 --duration 60s
```

CI gates (every PR): `go test -race`, `golangci-lint`, `vitest`, `playwright` against an ephemeral compose stack, `helm lint`, container image build + sign.

## Configuration surface

All services read configuration from environment variables, with Key Vault references resolved at startup via `internal/secrets`.

| Variable | Service | Purpose |
|---|---|---|
| `FLOWSCOPE_NETFLOW_PORT` | ingest | UDP listener (default 2055) |
| `FLOWSCOPE_SFLOW_PORT` | ingest | UDP listener (default 6343) |
| `FLOWSCOPE_IPFIX_PORT` | ingest | UDP listener (default 4739) |
| `FLOWSCOPE_HTTP_ADDR` | api | Listen address (default `:8080`) |
| `FLOWSCOPE_CLICKHOUSE_DSN` | all | ClickHouse cluster DSN |
| `FLOWSCOPE_AUTH_TOKEN_REF` | api | Key Vault reference for the API auth token |
| `FLOWSCOPE_SNMP_KEY_REF` | snmp | Key Vault reference for the SNMP master key |
| `FLOWSCOPE_LOG_LEVEL` | all | `debug` / `info` / `warn` / `error` |
| `FLOWSCOPE_SNMP_WORKERS` | snmp | Worker pool size (default 8) |
| `FLOWSCOPE_SNMP_MOCK` | snmp | `1` to use mock client; build-tag-gated |

`FLOWSCOPE_SNMP_KEY_REF` resolves to a master key the snmp service uses to derive an AES-256-GCM key via HKDF-SHA256 for credential encryption at rest. The resolved value MUST stay constant across restarts — rotating it invalidates every stored v3 credential.

## Architecture

### Concurrency model (Go)

- **`ingest`**: one goroutine per UDP listener, fan-in via buffered channels into a parser worker pool, then into `record.Emit` which writes to (a) the in-process hot ring and (b) the ClickHouse batch writer goroutine. Backpressure is explicit — if the batch writer can't keep up, the parser pool blocks on the channel and dropped-packet counters increment on the listener side.
- **`api`**: `net/http` with `chi` for routing, stateless. WebSocket endpoint streams hot-ring updates and Overview metrics. Reads come from ClickHouse with prepared queries and a small per-request cache.
- **`alert`**: singleton via leader election (using a ClickHouse Keeper lock or equivalent). Single evaluation loop on a ticker; rules are pure functions over query results, idempotent.
- **`snmp`**: `errgroup`-managed worker pool sized via `FLOWSCOPE_SNMP_WORKERS`. Per-device cadence stored in the `devices` table. In-flight walks deduped by `(exporter, oid)` key.
- **`gnmi`**: long-lived gRPC streams, one goroutine per subscription, fan-in to a single writer goroutine that emits records via `record.Emit` (same fan-out point as flow ingest).

Shared mutable state across goroutines uses `sync.RWMutex` or channels — never both, never bare globals. Every shared struct has a comment naming the lock that guards it. The race detector runs on every CI invocation.

### Data flow: parser → `record.Emit` → fan-out

Every parser (NetFlow v5, NetFlow v9/IPFIX, sFlow v5) produces a canonical `record.Flow` and calls `record.Emit(ctx, rec)`. `Emit` is the **single fan-out point** that:

1. Pushes to the in-process hot ring (`internal/record/ring.go`) for sub-second live views.
2. Hands off to the ClickHouse batch writer (`internal/store/batcher.go`), which accumulates and flushes every N records or T milliseconds, whichever comes first.
3. Updates per-exporter / per-interface counter aggregates used by the Overview tab.

**Asymmetry for sFlow / gNMI counter samples.** Counter samples bypass the flow-derived aggregation path and write directly to `iface_counter_samples` in ClickHouse with absolute octets/packets. The per-interface timeseries endpoint diffs successive counter samples to compute authoritative bytes/sec. When no counter samples exist (NetFlow-only exporters), it falls back to flow-bucketed aggregation and labels the response `"source": "flows"`. This `counters > flows` invariant is non-negotiable — see VISION.md §3.3.

### NetFlow v9 / IPFIX templates

Templates are stored per ingest replica in `internal/netflow.TemplateCache`, keyed by `(exporter, source_id, template_id)`. Data flowsets that arrive before their template is seen are dropped and a `nf9_template_miss_total` counter increments. The 5-tuple-hashed UDP load balancer ensures a given exporter consistently lands on the same replica, so template state is reliably colocated with data records from that exporter. Field IDs FlowScope cares about are listed in `internal/netflow/fields.go`; unknown fields are skipped by declared length.

### sFlow v5 parser

`sflow.Parse` dispatches by sample format (`tag & 0xFFF`):

- 1 / 3 — flow_sample / flow_sample_expanded → `parseFlowSample` → `parseRawHeader` (Ethernet / VLAN / IPv4/IPv6 / TCP/UDP)
- 2 / 4 — counters_sample / counters_sample_expanded → `parseCountersSample` (writes absolute interface totals)

The agent address from the sFlow datagram header overrides the UDP source IP as the canonical exporter ID, except when it's `0.0.0.0`.

### Frontend

The SPA polls REST endpoints via TanStack Query and subscribes to a WebSocket for the Overview tab and live flow tail. All filtering (by exporter, by ifIndex, by 5-tuple) is encoded in the URL via React Router search params and passed through to the API as query parameters. The OpenAPI-generated TS client lives in `web/src/api/generated/` and is regenerated by `make gen-client` whenever `api/openapi.yaml` changes — never hand-edit the generated files.

### REST API surface

Source of truth: `api/openapi.yaml`. Adding an endpoint means editing the spec, regenerating the TS client, then implementing the Go handler. Drift between handlers and spec is caught by a contract test in CI.

Key endpoints (full list in the spec):

- `GET /api/summary`, `/api/devices`, `/api/interfaces`, `/api/flows/recent`
- `GET /api/top/talkers`, `/api/top/ports`, `/api/protocols`, `/api/timeseries`
- `GET /api/interfaces/{exporter}/{ifindex}/timeseries?seconds=N` — bucketed ingress/egress; response carries `"source": "counters" | "gnmi" | "flows"`
- `GET /api/alerts`, `POST /api/alerts/{id}/ack`, `POST /api/alerts/{id}/close`
- `WS  /api/stream` — Overview metrics + live flow tail

Most accept `?exporter=<ip>` for filtering; flows + timeseries also accept `?ifindex=<n>`.

## Constraints worth knowing

- **Pollerless-first.** Receivers, not interrogators. Don't add a periodic SNMP poller for anything that streaming telemetry or sFlow counters can provide.
- **Counter samples win.** Anywhere the UI shows a rate, prefer counter-sample diffs and fall back to flow buckets, labeling the source.
- **One fan-out point per data type.** All flows through `record.Emit`. All SNMP through the worker pool. All alerts through the engine. New features extend these, not bypass them.
- **TLS at the edge, not in the binaries.** Azure Application Gateway, nginx, or Caddy terminates TLS. The Go services speak plain HTTP inside the cluster.
- **No flow data leaves the customer's tenant.** No SaaS callbacks, no telemetry-back-to-vendor. The only outbound traffic is what the customer explicitly configures (webhook URLs, SMTP relay, syslog server).
- **Schema migrations are forward-only and idempotent.** They run from a one-shot init container before new pods take traffic. No external migration tool.

## Working agreement

- **99% confidence rule.** Before making any change, you must be ≥99% confident the change is correct and complete. If you are not, stop and either ask clarifying questions, or write down the specific reasons it may not work (unknown protocol field layouts, untested concurrency interactions, ambiguous user intent, missing dependency, untested platform behavior, etc.) so the user can decide whether to proceed. This applies to code edits, schema changes, dependency additions, and infra changes. A confident plan with explicit caveats is preferred over a guess that compiles.
- **Tests gate merges.** A PR that touches a parser must add or update a golden-pcap test. A PR that touches a schema must include the migration and a migration test. A PR that touches a critical user journey must update the Playwright spec. CI enforces this; reviewers enforce it harder.
- **Race detector is non-negotiable.** `go test -race` runs on every CI invocation. Any new shared state must survive the race detector under concurrent load.
- **Typed contracts at every boundary.** Go interfaces internally; OpenAPI-generated TS client between api and web. Don't bypass the generated client to "just hit the endpoint" from React.
- **No silent failures.** Every dropped packet, malformed template, failed walk increments a Prometheus counter and is visible on the Overview tab. If you add a new failure mode, you add the counter in the same PR.
- **Render on state change.** React click handlers update local state synchronously *before* awaiting fetches. A dashboard that feels broken for 200ms is a dashboard the user does not trust.
- **No browser pop-ups.** Use the in-app modal primitives (`Dialog`, `AlertDialog`, `Prompt` from the design system). Never `window.prompt` / `confirm` / `alert`.
- **Adding a new Go binary requires a Helm chart update.** New `cmd/<service>/` needs a Deployment, Service, and (if applicable) HPA + PDB in `deploy/helm/flowscope/templates/`. CI's `helm lint` will catch missing pieces, but reviewers should verify.
