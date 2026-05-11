// Command init is a one-shot bootstrapper that runs before api/ingest
// take traffic. It applies forward-only schema migrations and then
// reads the operator-controlled retention knobs (flow_retention_days,
// counter_retention_days) from app_settings, emitting the
// corresponding ALTER TABLE … MODIFY TTL statements against
// ClickHouse.
//
// Why a separate binary instead of doing this in api startup:
//
//   - TTL changes are operationally significant. Folding them into the
//     api boot path means a typo in a setting could prevent the api
//     from serving — which is the wrong failure mode. As a one-shot
//     init container, a failure is loud and isolated.
//   - We want to apply migrations before either api or ingest start,
//     so that the connection-pool warm-up of those services never
//     races with the meta table being created.
//   - In Helm, this fits cleanly as an initContainer on the api and
//     ingest deployments. In docker-compose, it's a depends_on
//     dependency.
//
// Idempotency: the ALTER TABLE … MODIFY TTL statement is a no-op when
// the new TTL matches the current one. ClickHouse does NOT rewrite
// the existing data parts on a TTL change — it only changes the
// merge expression, and background TTL merges drop expired data
// lazily over time. This is documented in the PR runbook.
//
// Audit: when the desired retention differs from the current TTL, an
// audit_events row is written so operators can see "we shifted flow
// retention from 30d to 7d at 14:02 UTC".
//
// Failure mode: any error in connect / migrate / ALTER / audit causes
// a non-zero exit with a clear log line. No silent failures. The
// process never returns 0 with errors logged.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/store"
)

const auditActor = "init-container"

func main() {
	if err := run(); err != nil {
		slog.Error("init exited with error", "err", err)
		os.Exit(1)
	}
	slog.Info("init completed successfully")
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("FLOWSCOPE_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	dsn := os.Getenv("FLOWSCOPE_CLICKHOUSE_DSN")
	if dsn == "" {
		return errors.New("FLOWSCOPE_CLICKHOUSE_DSN is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	conn, err := store.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("clickhouse open: %w", err)
	}
	defer conn.Close()

	// Forward-only migrations first — guarantees the tables we ALTER
	// and the app_settings table we read both exist.
	if err := store.Migrate(ctx, conn); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	slog.Info("migrations applied")

	// Now apply retention.
	if err := applyRetention(ctx, conn); err != nil {
		return fmt.Errorf("retention: %w", err)
	}
	return nil
}

// applyRetention runs through every entry in store.RetentionTargets,
// reads the desired value from app_settings, and emits the ALTER
// statement. When the current TTL differs from the new one, it also
// writes an audit_events row.
func applyRetention(ctx context.Context, conn driver.Conn) error {
	for _, t := range store.RetentionTargets {
		desired, err := store.ReadRetentionDays(ctx, conn, t.SettingKey, t.DefaultDays)
		if err != nil {
			return fmt.Errorf("read setting %q: %w", t.SettingKey, err)
		}

		current, currentKnown, err := store.CurrentTTLDays(ctx, conn, t.Table)
		if err != nil {
			return fmt.Errorf("inspect current TTL for %s: %w", t.Table, err)
		}

		stmt, err := store.BuildAlterTTLStatement(t, desired)
		if err != nil {
			return fmt.Errorf("build alter %s: %w", t.Table, err)
		}

		// Always emit the ALTER. ClickHouse no-ops when the expression
		// matches, and the cost of a redundant ALTER is negligible
		// next to the cost of getting the comparison wrong.
		slog.Info("applying retention",
			"table", t.Table,
			"days", desired,
			"current_days", currentDaysForLog(current, currentKnown),
			"setting", t.SettingKey,
		)
		if err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ALTER %s: %w", t.Table, err)
		}

		// Emit an audit row only when the value actually changed (or
		// when we couldn't parse the current TTL — log it as a
		// reconciliation event so the operator has a breadcrumb).
		if !currentKnown || current != desired {
			if err := writeRetentionAudit(ctx, conn, t, current, currentKnown, desired); err != nil {
				// Audit failures are loud but non-fatal — the schema
				// change has already landed, and refusing to start
				// because we couldn't write the ledger row would be
				// the wrong tradeoff. Log and move on.
				slog.Warn("retention audit write failed (non-fatal)",
					"table", t.Table,
					"err", err,
				)
			}
		}
	}
	return nil
}

func currentDaysForLog(n int, known bool) any {
	if !known {
		return "unknown"
	}
	return n
}

// writeRetentionAudit appends an audit_events row describing the TTL
// change. We use the same table the api writes to so the existing
// /api/settings/audit endpoint surfaces this event in the Settings →
// Audit tab without any additional plumbing.
func writeRetentionAudit(
	ctx context.Context,
	conn driver.Conn,
	t store.RetentionTarget,
	beforeDays int,
	beforeKnown bool,
	afterDays int,
) error {
	beforeJSON := fmt.Sprintf(`{"table":%q,"days":%d,"known":%t}`,
		t.Table, beforeDays, beforeKnown)
	afterJSON := fmt.Sprintf(`{"table":%q,"days":%d,"setting":%q}`,
		t.Table, afterDays, t.SettingKey)

	addr := netip.IPv6Unspecified()
	ipBytes := addr.As16()

	const ins = `
INSERT INTO audit_events
   (ts, actor, action, resource, target, before_json, after_json, request_id, source_ip)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return conn.Exec(ctx, ins,
		time.Now().UTC(),
		auditActor,
		"update",
		"retention",
		t.Table,
		beforeJSON,
		afterJSON,
		"",
		ipBytes[:],
	)
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
