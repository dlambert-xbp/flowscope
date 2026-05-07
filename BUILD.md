# Building FlowScope

This is the operator's build manual for the v1 (Go + ClickHouse + React) line of FlowScope. The Python-era stack lives on the [`v1` branch](https://github.com/dlambert-xbp/flowscope/tree/v1).

There are three levels you can build at, in increasing fidelity:

| Level | What you get | Time | Needs |
|---|---|---|---|
| **1 — Native** | Go binaries + tests + React bundle, no DB | seconds | Go 1.25+, Node 20+ |
| **2 — Local Docker stack** | ClickHouse + all four Go services + the SPA, real end-to-end | ~1 min | Docker Desktop |
| **3 — Self-hosted ClickHouse on Azure** | Production data tier on a Premium SSD VM | 3–5 min | Azure CLI, an Azure subscription |

Level 3 is the **data tier only** — there is no Bicep / Helm yet for the Go services or the SPA. To run those in production, you build the container images from this repo and ship them yourself; the Helm chart is in [TASKS.md](TASKS.md).

---

## Level 1 — Native binaries

Quickest verification that everything compiles and tests pass. No database, no containers.

```bash
go build ./...        # compiles cmd/{ingest,api,alert,snmp,synth}
go test ./...         # 50 tests across record, netflow, sflow, store, obs, snmpx
go vet ./...          # static analysis
```

Outputs five binaries (`ingest`, `api`, `alert`, `snmp`, `synth`, `.exe` on Windows) in the repo root.

`cmd/ingest` will start without a ClickHouse DSN and run in ring-only mode — useful for parser testing without a database:

```bash
go run ./cmd/ingest
```

`cmd/api` requires a `FLOWSCOPE_CLICKHOUSE_DSN`; without one it exits immediately. For dev, point it at the compose ClickHouse:

```bash
FLOWSCOPE_CLICKHOUSE_DSN=clickhouse://flowscope:flowscope@localhost:9000/flowscope go run ./cmd/api
```

`cmd/synth` is standalone — it just sends UDP packets and doesn't care whether anything receives them:

```bash
go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 5000 --duration 10s
```

### React app

The SPA lives in `web/` and builds independently:

```bash
cd web
npm install
npm run dev          # Vite dev server on :5173 with HMR
npm run build        # production bundle into web/dist/
npm run lint         # tsc --noEmit
```

`npm run dev` proxies `/api/*` to `http://localhost:8080` (where `cmd/api` runs), so the typical local loop is: compose up ClickHouse → `go run ./cmd/api` → `npm run dev` in another terminal.

---

## Level 2 — Full local Docker stack

Brings up ClickHouse + all four Go services + the nginx-served React SPA. This is the everyday dev loop.

```bash
docker compose up --build
```

Wait for these log lines:

```
flowscope-clickhouse  | <Information> Application: Ready for connections
flowscope-ingest      | clickhouse migrations applied
flowscope-ingest      | flowscope ingest started
flowscope-api         | flowscope api started addr=:8080
flowscope-alert       | flowscope alert started
flowscope-snmp        | flowscope snmp started
```

Exposed ports:

| Port | Service |
|---|---|
| 80/tcp | nginx → React SPA + reverse proxy to `api:8080` |
| 8080/tcp | `cmd/api` direct (REST/JSON) |
| 2055/udp | NetFlow v5/v9/IPFIX |
| 6343/udp | sFlow v5 |
| 9100/tcp | ingest Prometheus `/metrics` |
| 9101/tcp | alert Prometheus `/metrics` |
| 9102/tcp | snmp Prometheus `/metrics` |

In a second terminal, drive synthetic traffic:

```bash
go run ./cmd/synth -- --target localhost:2055 --rate 5000 --duration 30s
```

Verify rows landed:

```bash
docker compose exec clickhouse clickhouse-client \
  --user flowscope --password flowscope \
  --query "SELECT count(), uniq(exporter), formatReadableSize(sum(bytes)) FROM flowscope.flows"
```

Expected output after a 30s run at 5000 fps: `~150000  3  ~XX MiB`.

To shut down (preserving ClickHouse data):

```bash
docker compose down
```

To shut down and wipe all flow data:

```bash
docker compose down -v
```

---

## Level 3 — Self-hosted ClickHouse VM on Azure

Provisions a `Standard_D8s_v5` VM with a Premium SSD v2 data disk, Ubuntu 24.04, and cloud-init that installs ClickHouse, mounts the data disk, applies kernel tuning, disables transparent hugepages, and starts the systemd service. See [`deploy/infra/clickhouse-vm.bicep`](deploy/infra/clickhouse-vm.bicep) and [`deploy/infra/cloud-init.yaml`](deploy/infra/cloud-init.yaml).

This level provisions the **data tier**. The Go services and the SPA still run from container images you build from this repo (compose locally, or hand-rolled VM / AKS deployment until the Helm chart lands).

**Prerequisites:** an Azure subscription, the `az` CLI logged in, an existing VNet + subnet for FlowScope.

```bash
az group create --name flowscope-rg --location eastus2

az deployment group create \
  --resource-group flowscope-rg \
  --template-file deploy/infra/clickhouse-vm.bicep \
  --parameters \
    vmName=flowscope-clickhouse-01 \
    subnetId=<your-subnet-resource-id> \
    adminPublicKey="$(cat ~/.ssh/id_ed25519.pub)" \
    clickhousePassword=<strong-secret-from-keyvault>
```

Cloud-init takes 3–5 minutes after deployment returns. SSH in to verify:

```bash
ssh flowscope@<vm-private-ip>
sudo systemctl status clickhouse-server
clickhouse-client --user flowscope --password '<password>' --query 'SELECT version()'
```

Point the Go services at it via `FLOWSCOPE_CLICKHOUSE_DSN=clickhouse://flowscope:<password>@<vm-ip>:9000/flowscope`.

---

## Viewing the dashboard

After `docker compose up`, the React SPA is served from <http://localhost/> via nginx, with `/api/*` proxied to `cmd/api`.

The two static design mocks in [`mock/`](mock/) are historical — they were the v1.0 design-language reference before the SPA was built. They still open standalone in any browser and are useful for comparing visual choices, but the SPA is the canonical thing now:

- [`mock/dashboard-final.html`](mock/dashboard-final.html) — the v1.0 reference
- [`mock/dashboard.html`](mock/dashboard.html) — earlier cleaner-enterprise variant

---

## Common issues

- **`docker compose up` hangs at "starting" for 20 seconds.** ClickHouse first boot. The healthcheck blocks the Go services until ClickHouse responds to `/ping`. Patience.
- **Port 2055 already in use.** Some other tool is binding the NetFlow port. On Windows: `Get-NetUDPEndpoint -LocalPort 2055`. On Linux/macOS: `lsof -i :2055`. Stop the other process or change `FLOWSCOPE_NETFLOW_ADDR` in [`docker-compose.yml`](docker-compose.yml).
- **Race detector unavailable on Windows.** `go test -race` requires cgo + a C compiler. CI on Linux runs it; locally on Windows, install MSYS2 / mingw-w64 or use WSL.
- **`docker compose build` fails on Go version mismatch.** The Dockerfiles expect `golang:1.25-alpine` matching `go.mod`'s directive. If you bump dependencies that require a newer Go, update both.
- **ClickHouse memory pressure on small dev hosts.** The official image wants ~1 GiB minimum. If Docker Desktop is starved, raise its memory limit in Settings → Resources.
- **`cmd/api` exits immediately with `FLOWSCOPE_CLICKHOUSE_DSN is required`.** Unlike `cmd/ingest`, the API does not have a no-DB mode. Set the DSN env var or run via compose.

---

## What is *not* yet buildable

- **`cmd/gnmi`** — streaming-telemetry ingest (Phase 3 in [VISION.md](VISION.md))
- **Helm chart for AKS** — [`deploy/helm/flowscope/`](deploy/helm/flowscope/) doesn't exist yet; production deploys must hand-roll Deployments / Services
- **OpenAPI-generated TypeScript client** — [CLAUDE.md](CLAUDE.md) describes `api/openapi.yaml` as the source of truth and `make gen-client` as the regen target, but the spec file and the Makefile target don't exist; the TS client in [web/src/api.ts](web/src/api.ts) is hand-written
- **`internal/secrets`** — Key Vault / file / env loader is described in [CLAUDE.md](CLAUDE.md) but not yet implemented; `FLOWSCOPE_SNMP_KEY` is read directly from the env today
- **GitHub Actions workflow files** — [`.github/workflows/`](.github/workflows/) is empty; `go test -race`, `golangci-lint`, `vitest`, `playwright`, `helm lint`, and container build + sign are intended gates but nothing enforces them

The full open-tasks list is in [TASKS.md](TASKS.md).
