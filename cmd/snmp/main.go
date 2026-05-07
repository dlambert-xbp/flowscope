// Command snmp is the FlowScope SNMP enrichment service. It walks
// every observed exporter on a configurable cadence (default 15 min)
// and persists sysDescr / ifTable / ifXTable into ClickHouse so the
// Devices tab can render real hardware metadata.
//
// VISION.md §3.1 / §4.2 — SNMP is the FALLBACK, not a workhorse.
// We do not fleet-poll every five minutes. We walk per-device on
// the configured interval and on operator-triggered demand
// (triggered walks land in a follow-up slice).
//
// Authentication: v2c only in slice 15. v3 with encrypted credential
// storage (AES-256-GCM, HKDF-SHA256, master key from Key Vault per
// VISION.md §4.2) is the next slice.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("snmp exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(os.Getenv("FLOWSCOPE_LOG_LEVEL"))}))
	slog.SetDefault(logger)

	chDSN := envOr("FLOWSCOPE_CLICKHOUSE_DSN", "")
	if chDSN == "" {
		return errors.New("FLOWSCOPE_CLICKHOUSE_DSN is required")
	}
	intervalStr := envOr("FLOWSCOPE_SNMP_INTERVAL", "15m")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_SNMP_INTERVAL %q: %w", intervalStr, err)
	}
	community := envOr("FLOWSCOPE_SNMP_COMMUNITY", "public")
	mockMode := os.Getenv("FLOWSCOPE_SNMP_MOCK") == "1"
	workersStr := envOr("FLOWSCOPE_SNMP_WORKERS", "8")
	workers, err := strconv.Atoi(workersStr)
	if err != nil || workers <= 0 {
		workers = 8
	}
	metricsAddr := envOr("FLOWSCOPE_METRICS_ADDR", ":9102")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	conn, err := store.Open(ctx, chDSN)
	if err != nil {
		return fmt.Errorf("clickhouse open: %w", err)
	}
	defer conn.Close()

	if err := store.Migrate(ctx, conn); err != nil {
		return fmt.Errorf("clickhouse migrate: %w", err)
	}

	go func() {
		if err := obs.ServeMetrics(ctx, metricsAddr); err != nil {
			slog.Error("metrics server exited", "err", err)
		}
	}()

	var client snmpx.Client
	if mockMode {
		client = snmpx.NewMock()
		slog.Info("snmp: using mock client (FLOWSCOPE_SNMP_MOCK=1)")
	} else {
		client = snmpx.NewClient(snmpx.Config{
			Community: community,
			Port:      161,
			Timeout:   2 * time.Second,
			Retries:   1,
		})
	}

	sched := snmpx.NewScheduler(conn, client, interval, workers)
	slog.Info("snmp service starting",
		"interval", interval.String(),
		"workers", workers,
		"mock", mockMode,
		"metrics", metricsAddr,
	)
	return sched.Run(ctx)
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
