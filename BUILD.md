# Building FlowScope

This is the operator's build manual for the v1 (Go + ClickHouse + React) line of FlowScope. The Python-era stack lives on the [`v1` branch](https://github.com/dlambert-xbp/flowscope/tree/v1).

There are three levels you can build at, in increasing fidelity:

| Level | What you get | Time | Needs |
|---|---|---|---|
| **1 — Native Go** | Binaries + tests, no DB | seconds | Go 1.24+ |
| **2 — Local Docker stack** | ClickHouse + ingest, real end-to-end | ~1 min | Docker Desktop |
| **3 — Azure VM** | Production self-hosted ClickHouse | 3–5 min | Azure CLI, an Azure subscription |

---

## Level 1 — Native Go binaries

Quickest verification that the code compiles and tests pass. No database, no containers.

```bash
go build ./...        # compiles cmd/ingest and cmd/synth
go test ./...         # 24 tests across record, netflow, store
go vet ./...          # static analysis
```

Outputs `ingest.exe` and `synth.exe` (or no extension on Linux/macOS) in the repo root.

You can run `synth` standalone — it just sends UDP packets and doesn't care whether anything receives them:

```bash
go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 5000 --duration 10s
```

`ingest` will start without a ClickHouse DSN and run in ring-only mode:

```bash
go run ./cmd/ingest
```

---

## Level 2 — Full local Docker stack

Brings up ClickHouse + ingest as containers, runs the schema migrations, exposes the listener on `:2055/udp`. This is the everyday dev loop.

```bash
docker compose up --build
```

Wait for two log lines:

```
flowscope-clickhouse  | <Information> Application: Ready for connections
flowscope-ingest      | clickhouse migrations applied
flowscope-ingest      | flowscope ingest started
```

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

## Level 3 — Production: self-hosted ClickHouse on Azure

Provisions a `Standard_D8s_v5` VM with a Premium SSD v2 data disk, Ubuntu 24.04, and cloud-init that installs ClickHouse, mounts the data disk, applies kernel tuning, disables transparent hugepages, and starts the systemd service. See [`deploy/infra/clickhouse-vm.bicep`](deploy/infra/clickhouse-vm.bicep) and [`deploy/infra/cloud-init.yaml`](deploy/infra/cloud-init.yaml).

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

The `ingest` service connects to it via `FLOWSCOPE_CLICKHOUSE_DSN=clickhouse://flowscope:<password>@<vm-ip>:9000/flowscope`.

---

## Viewing the dashboard mock

There is **no React app yet** — `cmd/api` and the React SPA are upcoming slices. What exists today is the design mock at [`mock/dashboard-final.html`](mock/dashboard-final.html), which is a single self-contained HTML file demonstrating the v1.0 design language across the four primary tabs (Overview, Flows, Devices, Alerts).

Three ways to view it:

**Easiest** — double-click the file in Explorer, or:

```powershell
start mock/dashboard-final.html
```

Opens directly in your default browser via `file://`. Works because the mock has no relative asset references that a `file://` origin would block.

**With a local HTTP server** (if you prefer not to use `file://`):

```bash
# Python (any version)
python -m http.server 8000

# Or Go (no extra dependency)
go run ./cmd/synth -- --help    # nothing relevant; placeholder
```

Then browse to <http://localhost:8000/mock/dashboard-final.html>.

**Side-by-side with the alternate mock** — the original cleaner-enterprise variant lives at [`mock/dashboard.html`](mock/dashboard.html) for comparison.

---

## Common issues

- **`docker compose up` hangs at "starting" for 20 seconds.** ClickHouse first boot. The healthcheck blocks ingest until ClickHouse responds to `/ping`. Patience.
- **Port 2055 already in use.** Some other tool is binding the NetFlow port. On Windows: `Get-NetUDPEndpoint -LocalPort 2055`. On Linux/macOS: `lsof -i :2055`. Stop the other process or change `FLOWSCOPE_NETFLOW_ADDR` in `docker-compose.yml`.
- **Race detector unavailable on Windows.** `go test -race` requires cgo + a C compiler. CI on Linux runs it; locally on Windows, install MSYS2/mingw-w64 or use WSL.
- **`docker compose build` fails on Go version mismatch.** The Dockerfile expects `golang:1.24-alpine` matching `go.mod`'s directive. If you bump dependencies that require a newer Go, update both.
- **ClickHouse memory pressure on small dev hosts.** The official image wants ~1 GiB minimum. If Docker Desktop is starved, raise its memory limit in Settings → Resources.

---

## What is *not* yet buildable

- `cmd/api` — the REST/WebSocket service the React app will consume
- `web/` — the React SPA itself (Vite + Tailwind + shadcn)
- `cmd/alert`, `cmd/snmp`, `cmd/gnmi` — additional services from the architecture
- NetFlow v9 / IPFIX / sFlow parsers — only NetFlow v5 is wired today
- Helm chart for AKS

Tracking by slice in the roadmap section of [`VISION.md`](VISION.md).
