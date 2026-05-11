// Package integration contains shared helpers for FlowScope integration
// tests. The flagship helper, StartClickHouse, boots a real ClickHouse
// instance in a container, applies every migration in
// internal/store/migrations/, and hands the caller a live driver.Conn.
//
// The helper is intentionally NOT behind a build tag so it can be reused
// from any package's _integration_test.go file. The tests that consume it
// carry the //go:build integration tag, so the testcontainers-go
// dependency only matters at integration-test time.
//
// Docker is required. If Docker is not reachable the helper fails loudly
// with a clear message — silent skips would mask CI regressions.
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/dlambert-xbp/flowscope/internal/store"
)

// DefaultImage pins the ClickHouse image used by integration tests. It
// matches a recent stable release and intentionally avoids `:latest`
// to keep CI runs reproducible. Bump deliberately when you need newer
// SQL features.
const DefaultImage = "clickhouse/clickhouse-server:24.8-alpine"

// ClickHouse is the handle returned by StartClickHouse. Conn is a
// connected, schema-migrated driver.Conn ready for the tests to use.
// Cleanup terminates the container; tests should defer it.
type ClickHouseHandle struct {
	Conn      driver.Conn
	DSN       string
	Container *tcclickhouse.ClickHouseContainer
	Cleanup   func()
}

// StartClickHouse boots a ClickHouse container, opens a driver.Conn
// against it, and applies all FlowScope migrations. The caller is
// responsible for invoking Cleanup() — typically via t.Cleanup so the
// container is removed even when the test fails or panics.
//
// On any failure (no Docker, image pull error, migration error) the
// function calls t.Fatalf with the underlying cause. There is no silent
// skip — CLAUDE.md "no silent failures" applies here too.
func StartClickHouse(t testing.TB, ctx context.Context) *ClickHouseHandle {
	t.Helper()

	startCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(startCtx, DefaultImage)
	if err != nil {
		t.Fatalf("integration: start clickhouse container (image=%s): %v\n"+
			"Hint: Docker must be running locally for integration tests.",
			DefaultImage, err)
	}

	dsn, err := container.ConnectionString(startCtx)
	if err != nil {
		_ = terminate(container)
		t.Fatalf("integration: clickhouse connection string: %v", err)
	}

	conn, err := store.Open(startCtx, dsn)
	if err != nil {
		_ = terminate(container)
		t.Fatalf("integration: open clickhouse %q: %v", dsn, err)
	}

	migCtx, migCancel := context.WithTimeout(ctx, 60*time.Second)
	defer migCancel()
	if err := store.Migrate(migCtx, conn); err != nil {
		_ = conn.Close()
		_ = terminate(container)
		t.Fatalf("integration: apply migrations: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		if err := terminate(container); err != nil {
			// Don't fail the test for a teardown error — log only.
			t.Logf("integration: terminate clickhouse: %v", err)
		}
	}

	return &ClickHouseHandle{
		Conn:      conn,
		DSN:       dsn,
		Container: container,
		Cleanup:   cleanup,
	}
}

// Truncate clears every fixture table so back-to-back tests in the
// same container stay isolated. Each test calls this at start so it
// can assume an empty data plane regardless of order.
//
// The list mirrors the tables FlowScope writes to from migrations
// 1–8. New tables added by future migrations should be appended here.
func (h *ClickHouseHandle) Truncate(ctx context.Context, t testing.TB) {
	t.Helper()
	tables := []string{
		"flows",
		"iface_counter_samples",
		"events",
		"alert_events",
	}
	for _, tbl := range tables {
		// TRUNCATE on MergeTree is supported and synchronous.
		if err := h.Conn.Exec(ctx, fmt.Sprintf("TRUNCATE TABLE IF EXISTS %s", tbl)); err != nil {
			t.Fatalf("integration: truncate %s: %v", tbl, err)
		}
	}
}

// terminate shells out to the container's Terminate. Pulled out so
// the cleanup path stays one-liner clean.
func terminate(c *tcclickhouse.ClickHouseContainer) error {
	if c == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Terminate(ctx)
}

// MustExec is a small ergonomic helper for seeding fixtures in tests.
// It runs the SQL with the supplied args and t.Fatals on error.
func MustExec(ctx context.Context, t testing.TB, conn driver.Conn, query string, args ...any) {
	t.Helper()
	if err := conn.Exec(ctx, query, args...); err != nil {
		t.Fatalf("integration: exec %q: %v", query, err)
	}
}
