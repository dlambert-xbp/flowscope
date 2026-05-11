//go:build integration

// Integration tests for the four rules added in rules_device.go:
//
//   - DeviceCPUHigh
//   - DeviceMemoryHigh
//   - DeviceStorageHigh
//   - DeviceUnreachable
//
// Same conventions as evaluator_extra_integration_test.go: one fresh
// container per test via integration.StartClickHouse, no t.Parallel(),
// Truncate before seeding. Run with:
//
//	go test -race -tags=integration ./internal/alerteng/...
package alerteng

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/settings"
	"github.com/dlambert-xbp/flowscope/test/integration"
)

/* ----------------------- fixture helpers ----------------------- */

// fixtureResource is the columns of device_resource_samples the device
// rules read. Source is optional — the tests that don't care leave it
// blank and the violation copy degrades cleanly.
type fixtureResource struct {
	PolledAt     time.Time
	Exporter     string
	Kind         string // "cpu" | "memory" | "storage" | …
	Component    string
	ValuePercent float32
	ValueBytes   uint64
	MaxBytes     uint64
	Source       string
}

// insertResources seeds device_resource_samples. 000012_device_resources.sql
// declares 8 columns — match exactly so the batch lands cleanly.
func insertResources(ctx context.Context, t *testing.T, conn driver.Conn, rows []fixtureResource) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO device_resource_samples (
        polled_at, exporter, kind, component,
        value_percent, value_bytes, max_bytes, source
    )`)
	if err != nil {
		t.Fatalf("prepare device_resource_samples batch: %v", err)
	}
	for _, r := range rows {
		if err := batch.Append(
			r.PolledAt,
			toIPv6Bytes(r.Exporter),
			r.Kind,
			r.Component,
			r.ValuePercent,
			r.ValueBytes,
			r.MaxBytes,
			r.Source,
		); err != nil {
			t.Fatalf("append resource row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send resource batch: %v", err)
	}
}

// fixtureInventory is the subset of device_inventory columns the
// unreachable rule reads. The full table has 11 columns; we fill the
// rest with sensible zero defaults.
type fixtureInventory struct {
	PolledAt   time.Time
	Exporter   string
	SysName    string
	PollStatus string // "ok" | "partial" | "error"
}

func insertInventory(ctx context.Context, t *testing.T, conn driver.Conn, rows []fixtureInventory) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO device_inventory (
        polled_at, exporter, sys_descr, sys_object_id, sys_uptime_ms,
        sys_name, sys_location, sys_contact, iface_count,
        poll_duration_ms, poll_status
    )`)
	if err != nil {
		t.Fatalf("prepare device_inventory batch: %v", err)
	}
	for _, r := range rows {
		if err := batch.Append(
			r.PolledAt,
			toIPv6Bytes(r.Exporter),
			"",            // sys_descr
			"",            // sys_object_id
			uint64(0),     // sys_uptime_ms
			r.SysName,
			"",            // sys_location
			"",            // sys_contact
			uint32(0),     // iface_count
			uint32(0),     // poll_duration_ms
			r.PollStatus,
		); err != nil {
			t.Fatalf("append inventory row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send inventory batch: %v", err)
	}
}

/* ----------------------- DeviceCPUHigh ----------------------- */

// TestEvaluator_DeviceCPUHigh_FiresAboveThreshold seeds two CPU rows on
// one exporter — an old below-threshold reading and a fresh 96% reading.
// argMax picks the fresh row, the WHERE pct >= 80 filter accepts it, and
// because 96 >= 95 we expect severity=critical.
func TestEvaluator_DeviceCPUHigh_FiresAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		{PolledAt: now.Add(-15 * time.Minute), Exporter: "10.0.0.1", Kind: "cpu", Component: "Processor 1", ValuePercent: 20, Source: "cisco-process"},
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "cpu", Component: "Processor 1", ValuePercent: 96, Source: "cisco-process"},
		// Quiet control: different component below threshold, latest reading.
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "cpu", Component: "Processor 2", ValuePercent: 35, Source: "cisco-process"},
		// Wrong kind — should never reach the CPU rule's scan.
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "memory", Component: "Pool: Processor", ValuePercent: 99, Source: "cisco-mempool"},
	})

	rule := DeviceCPUHigh{ThresholdPct: 80, CriticalBumpPct: 15, LookbackSeconds: 1800}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["exporter"] != "10.0.0.1" {
		t.Errorf("exporter label = %q", v.Labels["exporter"])
	}
	if v.Labels["component"] != "Processor 1" {
		t.Errorf("component label = %q", v.Labels["component"])
	}
	if v.Labels["pct"] != "96" {
		t.Errorf("pct label = %q; want 96", v.Labels["pct"])
	}
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q (96%% >= 95%% crit)", v.Severity, SeverityCritical)
	}
	if v.GroupKey != "cpu_10.0.0.1_Processor 1" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
}

// TestEvaluator_DeviceCPUHigh_QuietBelowThreshold confirms a sub-threshold
// fresh reading produces no violation. Guards the >= comparison from
// regressing to >.
func TestEvaluator_DeviceCPUHigh_QuietBelowThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "cpu", Component: "Processor 1", ValuePercent: 79},
	})

	rule := DeviceCPUHigh{ThresholdPct: 80, CriticalBumpPct: 15, LookbackSeconds: 1800}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations, got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_DeviceCPUHigh_ThresholdOverride drops the threshold to 5%
// via alert_rule_settings. A modest 35% reading now trips it. Verifies
// the threshold_pct parameter flows through LoadRules.
func TestEvaluator_DeviceCPUHigh_ThresholdOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	store := settings.New(h.Conn, nil)
	if err := store.AlertRules.Upsert(ctx, settings.AlertRuleSetting{
		RuleID:  "device_cpu_high",
		Enabled: true,
		Params: map[string]any{
			"threshold_pct":     float64(5),
			"critical_bump_pct": float64(15),
			"lookback_seconds":  float64(1800),
		},
	}, "test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "cpu", Component: "Processor 1", ValuePercent: 35},
	})

	rules, _, err := LoadRules(ctx, store.AlertRules)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var rule DeviceCPUHigh
	var found bool
	for _, r := range rules {
		if x, ok := r.(DeviceCPUHigh); ok {
			rule = x
			found = true
		}
	}
	if !found {
		t.Fatal("DeviceCPUHigh missing from loaded rule set")
	}
	if rule.ThresholdPct != 5 {
		t.Fatalf("loaded ThresholdPct = %d; want 5", rule.ThresholdPct)
	}

	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation with 5%% threshold, got %d: %+v", len(violations), violations)
	}
}

/* ----------------------- DeviceMemoryHigh ----------------------- */

// TestEvaluator_DeviceMemoryHigh_BytesRatioDrivesPercent seeds a memory
// row with value_bytes/max_bytes ratio at 90%. The rule's threshold is
// 85% so it fires; value_percent on the row is zero (devices that report
// absolute bytes sometimes leave the derived percent unset). Severity
// should be warning (90 < 95 crit).
func TestEvaluator_DeviceMemoryHigh_BytesRatioDrivesPercent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		{
			PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1",
			Kind: "memory", Component: "Pool: Processor",
			ValuePercent: 0, // unset — drives the if(bytes_total > 0, ...) branch
			ValueBytes:   900 * 1024 * 1024,
			MaxBytes:     1024 * 1024 * 1024,
			Source:       "cisco-mempool",
		},
	})

	rule := DeviceMemoryHigh{ThresholdPct: 85, CriticalBumpPct: 10, LookbackSeconds: 1800}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["component"] != "Pool: Processor" {
		t.Errorf("component label = %q", v.Labels["component"])
	}
	// 900/1024 ≈ 87.89; toUInt32 truncates → 87.
	if v.Labels["pct"] != "87" {
		t.Errorf("pct label = %q; want 87 (900MiB/1GiB)", v.Labels["pct"])
	}
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning (87%% < 95%% crit)", v.Severity)
	}
	if v.Labels["bytes_used"] == "" || v.Labels["bytes_total"] == "" {
		t.Errorf("expected bytes labels to be set when bytes_total > 0: %v", v.Labels)
	}
}

// TestEvaluator_DeviceMemoryHigh_PercentFallback covers the second branch
// of the if(bytes_total > 0, …, value_percent) expression: when max_bytes
// is zero, the rule trusts the device-reported value_percent. Seed a
// row at 92% with bytes columns zero, threshold 85 → warning.
func TestEvaluator_DeviceMemoryHigh_PercentFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		{
			PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1",
			Kind: "memory", Component: "Physical memory",
			ValuePercent: 92,
			ValueBytes:   0,
			MaxBytes:     0,
			Source:       "hrmib",
		},
	})

	rule := DeviceMemoryHigh{ThresholdPct: 85, CriticalBumpPct: 10, LookbackSeconds: 1800}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	if violations[0].Labels["pct"] != "92" {
		t.Errorf("pct label = %q; want 92", violations[0].Labels["pct"])
	}
	if _, ok := violations[0].Labels["bytes_used"]; ok {
		t.Errorf("bytes_used label should be absent when bytes_total = 0: %v", violations[0].Labels)
	}
}

/* ----------------------- DeviceStorageHigh ----------------------- */

// TestEvaluator_DeviceStorageHigh_PerComponent confirms storage rules fire
// per-component: two filesystems on one exporter, one above threshold,
// one below. Expect exactly one violation tied to the saturated mount.
func TestEvaluator_DeviceStorageHigh_PerComponent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertResources(ctx, t, h.Conn, []fixtureResource{
		// 480MB used / 500MB total = 96% — critical.
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "storage", Component: "bootflash:", ValueBytes: 480 * 1024 * 1024, MaxBytes: 500 * 1024 * 1024, Source: "hrmib"},
		// 100MB / 500MB = 20% — quiet.
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", Kind: "storage", Component: "flash:", ValueBytes: 100 * 1024 * 1024, MaxBytes: 500 * 1024 * 1024, Source: "hrmib"},
	})

	rule := DeviceStorageHigh{ThresholdPct: 85, CriticalBumpPct: 10, LookbackSeconds: 3600}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["component"] != "bootflash:" {
		t.Errorf("component = %q; want bootflash:", v.Labels["component"])
	}
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want critical (96%% >= 95%% crit)", v.Severity)
	}
	if v.GroupKey != "stor_10.0.0.1_bootflash:" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
}

/* ----------------------- DeviceUnreachable ----------------------- */

// TestEvaluator_DeviceUnreachable_FiresOnErrorStatus seeds a recent
// poll with status=error and asserts the rule fires critical with the
// labels the UI consumes.
func TestEvaluator_DeviceUnreachable_FiresOnErrorStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertInventory(ctx, t, h.Conn, []fixtureInventory{
		// Previously healthy.
		{PolledAt: now.Add(-1 * time.Hour), Exporter: "10.0.0.1", SysName: "core-sw-01", PollStatus: "ok"},
		// Latest poll: error, fresh.
		{PolledAt: now.Add(-30 * time.Second), Exporter: "10.0.0.1", SysName: "core-sw-01", PollStatus: "error"},
		// Quiet control: a different exporter polling cleanly.
		{PolledAt: now.Add(-30 * time.Second), Exporter: "10.0.0.2", SysName: "edge-sw-01", PollStatus: "ok"},
	})

	rule := DeviceUnreachable{StaleSeconds: 2700, LookbackHours: 24}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["exporter"] != "10.0.0.1" {
		t.Errorf("exporter label = %q", v.Labels["exporter"])
	}
	if v.Labels["status"] != "error" {
		t.Errorf("status label = %q", v.Labels["status"])
	}
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want critical", v.Severity)
	}
	if v.GroupKey != "unreachable_10.0.0.1" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
}

// TestEvaluator_DeviceUnreachable_FiresOnStaleness seeds a device whose
// latest "ok" poll is older than StaleSeconds. The rule should still
// fire because we haven't seen a fresh walk.
func TestEvaluator_DeviceUnreachable_FiresOnStaleness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertInventory(ctx, t, h.Conn, []fixtureInventory{
		// Latest poll 90 minutes ago, status ok — but it's stale per the
		// 30-minute StaleSeconds the test passes in.
		{PolledAt: now.Add(-90 * time.Minute), Exporter: "10.0.0.1", SysName: "core-sw-01", PollStatus: "ok"},
		// Fresh control.
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.2", SysName: "edge-sw-01", PollStatus: "ok"},
	})

	rule := DeviceUnreachable{StaleSeconds: 1800, LookbackHours: 24}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	if violations[0].Labels["exporter"] != "10.0.0.1" {
		t.Errorf("exporter label = %q", violations[0].Labels["exporter"])
	}
	// status should still be "ok" — the trigger here is staleness, not
	// the poll status itself.
	if violations[0].Labels["status"] != "ok" {
		t.Errorf("status label = %q; want ok (stale path)", violations[0].Labels["status"])
	}
}

// TestEvaluator_DeviceUnreachable_QuietWhenHealthy seeds two devices
// polling cleanly, fresh. No violations expected.
func TestEvaluator_DeviceUnreachable_QuietWhenHealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertInventory(ctx, t, h.Conn, []fixtureInventory{
		{PolledAt: now.Add(-30 * time.Second), Exporter: "10.0.0.1", SysName: "core-sw-01", PollStatus: "ok"},
		{PolledAt: now.Add(-30 * time.Second), Exporter: "10.0.0.2", SysName: "edge-sw-01", PollStatus: "partial"},
	})

	rule := DeviceUnreachable{StaleSeconds: 2700, LookbackHours: 24}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations on healthy poll, got %d: %+v", len(violations), violations)
	}
}
