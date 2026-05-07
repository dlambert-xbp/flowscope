# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Install dependencies (Python 3.9+: Flask, waitress, APScheduler, pysnmp, cryptography)
pip install -r requirements.txt

# Run the collector + dashboard (binds 2055/udp, 6343/udp, 8080/tcp)
python app.py

# Override ports
python app.py --netflow-port 9995 --sflow-port 6343 --web-port 8080

# Generate synthetic traffic against a running collector (no real switch needed)
python synth_flows.py --target 127.0.0.1 --duration 300 --rate 30

# Container build / run
docker compose up --build
```

There are no tests, no linter config, and no build step — `app.py` and `web/index.html` are run as-is.

Environment variable overrides (also honored inside the Docker image): `FLOWSCOPE_NETFLOW_PORT`, `FLOWSCOPE_SFLOW_PORT`, `FLOWSCOPE_WEB_PORT`, `FLOWSCOPE_WEB_HOST`, `FLOWSCOPE_DB_PATH`, `FLOWSCOPE_AUTH_TOKEN`, `FLOWSCOPE_SNMP_WORKERS` (default 8).

`FLOWSCOPE_AUTH_TOKEN` (optional): when set, every `/api/*` request must include `X-Auth-Token: <value>`. Unset = no auth (current behavior). The static dashboard shell is not gated; the browser prompts for the token on first 401 and caches it in `sessionStorage`. Token comparison is constant-time. Does not provide TLS — terminate TLS at a reverse proxy.

`FLOWSCOPE_SNMP_KEY` (required for v3 SNMP profiles): master key for AES-256-GCM encryption of v3 passphrases at rest. HKDF-SHA256 derives a 32-byte key from this string. **The value MUST stay constant across restarts** — change it and existing v3 profiles cannot decrypt. Generate with `python -c "import secrets; print(secrets.token_urlsafe(32))"`.

`FLOWSCOPE_SNMP_MOCK` (optional, dev-only): set to `1` to use the deterministic mock SNMP client instead of pysnmp-lextudio. Mock returns synthetic ifDescr/ifAlias/ifSpeed and ignores credentials. Default off.

## Architecture

FlowScope is **two single-file programs** (`app.py` backend, `web/index.html` frontend) plus an optional traffic synthesizer (`synth_flows.py`). Keep this shape — the project's stated goal is a self-contained collector with no agents, no external dashboard, and no build pipeline.

### Concurrency model in `app.py`

`main()` starts UDP-listener threads, RDNS workers, an APScheduler `BackgroundScheduler`, and then hands control to **waitress** as the WSGI server:

1. `netflow_listener` — daemon thread, UDP recv loop on `NETFLOW_PORT`. Dispatches by version word: 5 → `parse_netflow_v5`, 9/10 → `parse_netflow_v9` (NetFlow v9 and IPFIX share enough wire format that they use the same parser).
2. `sflow_listener` — daemon thread, UDP recv loop on `SFLOW_PORT` → `parse_sflow_v5`.
3. `rdns_loop` / `endpoint_rdns_loop` — best-effort PTR resolution for exporter and endpoint IPs.
4. APScheduler jobs:
   - `snmp_scheduler_tick` — runs every 1s, walks `devices`, dispatches eligible polls to `snmp_pool` (a `ThreadPoolExecutor`, default 8 workers, override via `FLOWSCOPE_SNMP_WORKERS`). Per-device interval comes from the binding (default 15s); failures bypass the interval so a flapping device retries on the next tick. The scheduler skips devices with an in-flight future to avoid stacking.
   - `db_prune_tick` — every 5 minutes, deletes rows in `flows` and `iface_counter_samples` older than 6h.
5. **waitress.serve** — production WSGI server (replaced `app.run()`). Request handlers read shared state under the locks.

All shared mutable state (`recent_flows`, `interface_stats`, `devices`, `stats`, `interface_issues`) is guarded by **`state_lock`**. The SQLite connection is shared and guarded by **`db_lock`** (WAL mode). Any new state read or written from multiple threads must take the appropriate lock.

### Data flow: parser → `record_flow` → three stores

Every parser ultimately calls `record_flow(rec)`, which is the single fan-out point that writes into:

- `recent_flows` — `deque(maxlen=5000)` ring buffer used by all live API views.
- `interface_stats` — `defaultdict` keyed by `(exporter_ip, ifindex)`. Updated *additively* from each flow's `input_if`/`output_if`.
- `devices` — exporter IP → first/last-seen and flow count.
- SQLite `flows` table — append-only, pruned to ~6h.

**Important asymmetry for sFlow counter samples.** `parse_sflow_counters_sample` writes directly into `interface_stats` and **replaces** (not adds to) `ingress_bytes`/`ingress_packets`/`egress_bytes`/`egress_packets` with the absolute counters reported by the device. These hardware counter values are authoritative; the additive flow-derived numbers are estimates. When changing how interface totals are computed, preserve this "counter samples win" behavior — the README documents it as a feature.

The same parser also persists each sample to the **`iface_counter_samples`** SQLite table (`exporter`, `ifindex`, `ts`, `in_octets`, `out_octets`, `in_pkts`, `out_pkts`). The per-interface timeseries endpoint (`/api/interfaces/<exp>/<idx>/timeseries`) diffs successive samples to compute authoritative bytes/sec rates. When no counter samples exist (NetFlow-only exporters), it falls back to flow-derived bucketing and returns `{"source": "flows"}` instead of `{"source": "counters"}`.

### NetFlow v9 / IPFIX templates

NetFlow v9 / IPFIX data records are unparseable until the matching template flowset arrives. Templates are stored in the module-level `templates` dict keyed by `(exporter, source_id, template_id)` and looked up when data flowsets are decoded. Field IDs FlowScope cares about are listed in `NF9_FIELDS` (bytes, packets, protocol, ports, IPv4/IPv6 addrs, input/output ifIndex). Other fields are skipped by advancing past their declared length — this is intentional and keeps the parser tolerant of unknown fields.

### sFlow v5 parser

`parse_sflow_v5` dispatches by sample format (`tag & 0xFFF`):

- 1 / 3 — flow_sample / flow_sample_expanded → `parse_sflow_flow_sample` → `parse_sflow_raw_header` (decodes Ethernet/VLAN/IPv4/IPv6/TCP/UDP from the sampled raw packet).
- 2 / 4 — counters_sample / counters_sample_expanded → `parse_sflow_counters_sample` (writes absolute interface totals).

The expanded variants use 8-byte (type+index) input/output interface fields instead of the packed 32-bit form; both are handled.

The agent address from the sFlow datagram header overrides the UDP source IP as the canonical exporter ID, except when it's `0.0.0.0`.

### Frontend

`web/index.html` is a single static file (no build, no framework, no bundler) served by Flask from `web/`. It polls the `/api/*` JSON endpoints. All filtering (by exporter, by ifIndex) is implemented as query parameters on those endpoints — the frontend just passes them through.

### REST API surface

Endpoints listed in the README (`/api/summary`, `/api/devices`, `/api/interfaces`, `/api/flows/recent`, `/api/top/talkers`, `/api/top/ports`, `/api/protocols`, `/api/timeseries`). Most accept `?exporter=<ip>` for filtering; flows + timeseries also accept `?ifindex=<n>`. If you add an endpoint, follow the same pattern: read state under `state_lock`, copy out, release the lock, then aggregate.

Per-interface drill-down endpoints (used by the Interfaces tab modal):
- `GET  /api/interfaces/<exporter>/<ifindex>/timeseries?seconds=N` — bucketed ingress/egress rate. Source: counter-sample diffs when available, flow-derived fallback otherwise. Response includes `"source": "counters" | "flows"`.
- `POST /api/interfaces/<exporter>/<ifindex>/flag` — body `{"note": "..."}`, upserts a row in `interface_issues`.
- `DELETE /api/interfaces/<exporter>/<ifindex>/flag` — clears the flag.
The base `/api/interfaces` response carries the extended SNMP fields (`admin_status`, `oper_status`, `in_errors`, `out_errors`, `in_discards`, `out_discards`, `mtu`, `mac`) plus `flagged` / `flag_note` for each row.

## Constraints worth knowing

- **No auth on the web/API.** The README states this explicitly — don't add features that assume the dashboard is private; keep it suitable for a management network behind a reverse proxy.
- **Two-file philosophy.** `app.py` (backend) and `web/index.html` (frontend) stay as-is. Don't split into a package or add a build step. Runtime deps are Flask, waitress, APScheduler, pysnmp, cryptography — keep this set small. SNMP modules (`snmp_client.py`, `snmp_mock.py`, `snmp_crypto.py`) are the established exception; add new top-level modules only when they have a similarly clean isolated concern.
- **In-memory ring is 5000 flows.** Anything that needs more history must query SQLite, not `recent_flows`.
- **Adding new top-level `.py` modules requires updating the Dockerfile.** Each module needs an explicit `COPY <module>.py ./` line; missing one crashes the container with `ModuleNotFoundError`.

## Working agreement

- **99% confidence rule.** Before making any change, you must be ≥99% confident the change is correct and complete. If you are not, stop and either ask clarifying questions, or write down the specific reasons it may not work (unknown protocol field layouts, untested concurrency interactions, ambiguous user intent, missing dependency, untested platform behavior, etc.) so the user can decide whether to proceed. This applies to code edits, config changes, and dependency additions. A confident plan with explicit caveats is preferred over a guess that compiles.
