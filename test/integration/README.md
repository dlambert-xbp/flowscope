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
