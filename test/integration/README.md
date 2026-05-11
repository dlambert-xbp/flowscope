# Integration tests

This directory holds shared helpers used by FlowScope integration tests.
Tests that consume these helpers live next to the package under test
and are gated by a `//go:build integration` tag so they do not run as
part of the default `go test ./...` invocation.

## Running

Docker must be running locally (or a Docker-compatible socket must be
reachable; testcontainers-go honours `DOCKER_HOST`).

```bash
# Whole repo
go test -race -tags=integration ./...

# Just the alert engine
go test -race -tags=integration ./internal/alerteng/...
```

The first run pulls the ClickHouse image (`clickhouse/clickhouse-server`,
pinned in `clickhouse.go`); subsequent runs reuse the local image cache.

## What lives here

- `clickhouse.go` — `StartClickHouse` boots a ClickHouse container,
  applies every migration in `internal/store/migrations/`, and returns
  a connected `driver.Conn` plus a `Cleanup()` for `t.Cleanup`. No
  build tag — the helper is reusable from any package.
- `ClickHouseHandle.Truncate` clears every table FlowScope writes to,
  including the SNMP inventory tables (`device_snmp_interfaces`) and
  operator-tunable settings tables (`alert_rule_settings`). Append new
  tables here when migrations add them so per-test isolation stays
  honest.

## Fixture seeding

Per-package `_integration_test.go` files own their fixture-row helpers
to keep the cross-package surface small. The alerteng suite, for
example, declares:

- `insertFlows` (in `internal/alerteng/evaluator_integration_test.go`)
  — writes rows into `flows` via `PrepareBatch`, matching the 17-column
  schema from `000001_init.sql` + `000007_asn.sql`.
- `insertIfaces` and `insertCounterSamples` (in
  `internal/alerteng/evaluator_extra_integration_test.go`) — write the
  `device_snmp_interfaces` and `iface_counter_samples` rows the four
  P1 rules read. Match the column counts in `000003_snmp.sql` and
  `000001_init.sql` exactly.

When you add a rule that reads a new column, update the corresponding
seed helper in the same PR — drift between the helper signature and
the SQL is the most common cause of `clickhouse: bind error: too few
values` failures.

## Conventions

- **Build tag.** Test files that drive containers carry
  `//go:build integration` as the first line. The package directive
  goes on its own line below the tag plus a blank line.
- **Hermetic data.** Each test calls `handle.Truncate(ctx, t)` to
  reset the fixture tables before seeding. Tests run serially (no
  `t.Parallel()`) so this stays simple; per-test containers are an
  option if a future suite needs parallelism.
- **No silent skips.** If Docker is missing the tests fail loudly with
  a clear message. Skipping would let CI regressions hide.

## Why a build tag?

The default `go test ./...` runs in seconds and gates every PR. Pulling
a 250 MB ClickHouse image and waiting 10–20 seconds for it to boot
turns that workflow into something developers avoid. The tag keeps
unit tests fast and lets integration tests run on a separate CI lane
(see `go test -race -tags=integration ./...` in `CLAUDE.md`).
