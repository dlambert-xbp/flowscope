// Command snmp is the FlowScope SNMP enrichment service. It walks
// every observed exporter on a configurable cadence (default 60s)
// and persists sysDescr / ifTable / ifXTable / resource samples into
// ClickHouse so the Devices tab can render real hardware metadata
// and live CPU/memory/sensor health.
//
// VISION.md §3.1 / §4.2 — SNMP is the FALLBACK, not a workhorse.
// The 60s default keeps Devices-tab sparklines feeling live; real
// labs can dial the per-credential interval_sec up on noisier gear.
// Operator-triggered walks (via POST /api/devices/{exporter}/snmp/walk)
// also bypass the cadence floor entirely.
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
	"github.com/dlambert-xbp/flowscope/internal/secrets"
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

	// Resolve the SNMP master key through internal/secrets. Source
	// precedence (see internal/secrets.ResolveSNMPMaster):
	//   1. FLOWSCOPE_SNMP_KEY_REF  → env: / file: / kv: dispatch
	//   2. FLOWSCOPE_SNMP_KEY      → legacy plaintext, logs a
	//                                deprecation warning
	// A botched _REF is fatal (no silent fallback) — corrupting the
	// at-rest credential store by loading a different key than the
	// operator intended is exactly the failure mode CLAUDE.md's
	// master-key invariant exists to prevent.
	masterKey, err := secrets.ResolveSNMPMaster(ctx)
	if err != nil {
		return fmt.Errorf("snmp master key: %w", err)
	}
	var creds snmpx.CredentialStore
	if masterKey != "" {
		crypter, err := snmpx.NewCrypter(masterKey)
		if err != nil {
			return fmt.Errorf("snmp crypter: %w", err)
		}
		creds = snmpx.NewClickHouseCredentialStore(conn, crypter)
		slog.Info("snmp: per-exporter credential store enabled",
			"master_fp", secrets.Fingerprint(masterKey),
		)
	} else {
		slog.Warn("FLOWSCOPE_SNMP_KEY_REF / FLOWSCOPE_SNMP_KEY not set — per-exporter credentials disabled, falling back to env-var community / mock")
	}

	var fallback snmpx.Client
	if mockMode {
		fallback = snmpx.NewMock()
		slog.Info("snmp: fallback = mock (FLOWSCOPE_SNMP_MOCK=1)")
	} else {
		fallback = snmpx.NewClient(snmpx.Config{
			Version:   "v2c",
			Community: community,
			Port:      161,
			Timeout:   2 * time.Second,
			Retries:   1,
		})
		slog.Info("snmp: fallback = cluster-wide v2c", "community_set", community != "")
	}

	sched := snmpx.NewScheduler(conn, creds, fallback, interval, workers)
	slog.Info("snmp service starting",
		"interval", interval.String(),
		"workers", workers,
		"creds_enabled", creds != nil,
		"mock_fallback", mockMode,
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
