// Command alert runs the FlowScope alert engine. It evaluates the
// built-in rule set on a ticker and writes alert state transitions to
// the alert_events table. The api service reads from that table to
// serve /api/alerts; this binary writes only.
//
// In the same process the webhook dispatcher polls alert_events for
// opened / closed transitions and fans out to enabled
// webhook_endpoints (Slack / Teams / PagerDuty / HTTP). Co-locating
// the dispatcher with the engine keeps the deploy footprint at one
// binary.
//
// Both loops run inside a ClickHouse-backed leader lease
// (internal/leaderlease). Followers stay idle until they win the
// lease; loss of lease cancels the leader's child context so engine
// + dispatcher stop gracefully. Operators can therefore set
// alert.replicas > 1 in the Helm chart for HA without dupe-firing.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/alerteng"
	"github.com/dlambert-xbp/flowscope/internal/audit"
	"github.com/dlambert-xbp/flowscope/internal/leaderlease"
	"github.com/dlambert-xbp/flowscope/internal/notifier"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/settings"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("alert exited with error", "err", err)
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
	tickStr := envOr("FLOWSCOPE_ALERT_TICK", "10s")
	tick, err := time.ParseDuration(tickStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_ALERT_TICK %q: %w", tickStr, err)
	}
	stabilityStr := envOr("FLOWSCOPE_ALERT_STABILITY", "60s")
	stability, err := time.ParseDuration(stabilityStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_ALERT_STABILITY %q: %w", stabilityStr, err)
	}
	metricsAddr := envOr("FLOWSCOPE_METRICS_ADDR", ":9101")

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

	settingsStore := settings.New(conn, nil)
	rules, version, err := alerteng.LoadRules(ctx, settingsStore.AlertRules)
	if err != nil {
		slog.Warn("alert: initial rule load failed, using defaults", "err", err)
		rules = alerteng.DefaultRules()
		version = time.Time{}
	}
	slog.Info("alert engine starting",
		"rules", len(rules),
		"tick", tick.String(),
		"stability", stability.String(),
		"metrics", metricsAddr,
		"settings_version", version,
	)

	engine := alerteng.New(conn, rules, tick).
		WithSettingsSource(settingsStore.AlertRules, version).
		WithStabilityWindow(stability)

	// Webhook dispatcher runs alongside the engine. It is independent
	// — the engine writes events; the dispatcher reads them and fans
	// out. Both share the same ClickHouse connection. When
	// FLOWSCOPE_SNMP_KEY is unset the dispatcher logs a warning and
	// skips endpoints that store secrets, since secret_ct can't be
	// decrypted without the master.
	var crypter *snmpx.Crypter
	if mk := os.Getenv("FLOWSCOPE_SNMP_KEY"); mk != "" {
		c, err := snmpx.NewCrypter(mk)
		if err != nil {
			return fmt.Errorf("snmp crypter: %w", err)
		}
		crypter = c
	} else {
		slog.Warn("FLOWSCOPE_SNMP_KEY not set — webhook dispatcher will skip endpoints with secrets (PagerDuty / authenticated HTTP)")
	}

	dispTickStr := envOr("FLOWSCOPE_NOTIFIER_TICK", "5s")
	dispTick, err := time.ParseDuration(dispTickStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_NOTIFIER_TICK %q: %w", dispTickStr, err)
	}
	auditW := audit.NewClickHouseWriter(conn)
	disp := notifier.New(conn, crypter, auditW).WithTick(dispTick)

	// Leader election (P0 #5). Two replicas would dupe-fire BOTH the
	// engine and the dispatcher; a ClickHouse-backed lease ensures
	// exactly one replica runs the loops at any moment. Tunables
	// expose TTL + renew cadence so operators can balance failover
	// latency against ClickHouse write traffic.
	leaseTTLStr := envOr("FLOWSCOPE_ALERT_LEASE_TTL", "30s")
	leaseTTL, err := time.ParseDuration(leaseTTLStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_ALERT_LEASE_TTL %q: %w", leaseTTLStr, err)
	}
	leaseRenewStr := envOr("FLOWSCOPE_ALERT_LEASE_RENEW", "10s")
	leaseRenew, err := time.ParseDuration(leaseRenewStr)
	if err != nil {
		return fmt.Errorf("invalid FLOWSCOPE_ALERT_LEASE_RENEW %q: %w", leaseRenewStr, err)
	}
	lease := leaderlease.New(leaderlease.FromConn(conn), leaderlease.Config{
		Name:          "alert",
		TTL:           leaseTTL,
		RenewInterval: leaseRenew,
	})
	slog.Info("alert: leader-lease configured",
		"name", lease.Name(),
		"holder", lease.Holder(),
		"ttl", leaseTTL.String(),
		"renew", leaseRenew.String(),
	)

	onBecomeLeader := func(leaderCtx context.Context) error {
		slog.Info("alert: became leader — starting engine + dispatcher")
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := disp.Run(leaderCtx); err != nil {
				slog.Error("notifier: dispatcher exited with error", "err", err)
			}
		}()
		engineErr := engine.Run(leaderCtx)
		wg.Wait()
		// engine.Run / disp.Run return nil on ctx cancellation. Surface
		// only real errors — the periodic "lease lost, cancel child"
		// path is not an error.
		if engineErr != nil && leaderCtx.Err() == nil {
			return engineErr
		}
		return nil
	}

	return lease.Run(ctx, onBecomeLeader)
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
