//go:build integration

// Integration tests for the four rules added in rules_extra.go:
//
//   - InterfaceOperStatusChange
//   - InterfaceUtilizationHigh
//   - InterfaceErrorsRate
//   - TopTalkerBaselineAnomaly
//
// These tests live next to evaluator_integration_test.go and share its
// conventions: one fresh container per test via integration.StartClickHouse,
// no t.Parallel(), Truncate before seeding. Run with:
//
//	go test -race -tags=integration ./internal/alerteng/...
//
// Docker must be available locally; CI's Linux runner is the source of
// truth — see the PR body for the Windows-host caveat.
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

// fixtureIface is the minimal subset of device_snmp_interfaces columns
// the four rules read. Columns we don't assert on are left zero — that
// mirrors the production SNMP path where polls can be partial.
type fixtureIface struct {
	PolledAt      time.Time
	Exporter      string // dotted-quad or IPv6 literal
	IfIndex       uint32
	IfDescr       string
	IfSpeedBps    uint64
	IfOperStatus  string // "up" | "down" | "lowerLayerDown" | ...
	IfAdminStatus string
}

// insertIfaces seeds device_snmp_interfaces via PrepareBatch. The
// schema declared in 000003_snmp.sql has 14 columns — match exactly so
// the batch lands cleanly.
func insertIfaces(ctx context.Context, t *testing.T, conn driver.Conn, rows []fixtureIface) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO device_snmp_interfaces (
        polled_at, exporter, ifindex, if_descr, if_alias,
        if_type, if_speed_bps, if_mtu, if_admin_status, if_oper_status,
        if_in_errors, if_out_errors, if_in_discards, if_out_discards
    )`)
	if err != nil {
		t.Fatalf("prepare device_snmp_interfaces batch: %v", err)
	}
	for _, r := range rows {
		adminStatus := r.IfAdminStatus
		if adminStatus == "" {
			adminStatus = "up"
		}
		if err := batch.Append(
			r.PolledAt,
			toIPv6Bytes(r.Exporter),
			r.IfIndex,
			r.IfDescr,
			"",            // if_alias
			uint32(6),     // if_type (ethernetCsmacd)
			r.IfSpeedBps,
			uint32(1500),  // if_mtu
			adminStatus,
			r.IfOperStatus,
			uint64(0),     // if_in_errors
			uint64(0),     // if_out_errors
			uint64(0),     // if_in_discards
			uint64(0),     // if_out_discards
		); err != nil {
			t.Fatalf("append iface row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send iface batch: %v", err)
	}
}

// fixtureCounterSample is the iface_counter_samples columns the rules read.
type fixtureCounterSample struct {
	TS          time.Time
	Exporter    string
	IfIndex     uint32
	InOctets    uint64
	OutOctets   uint64
	InErrors    uint64
	OutErrors   uint64
	InDiscards  uint64
	OutDiscards uint64
}

// insertCounterSamples seeds iface_counter_samples via PrepareBatch.
// 000001_init.sql declares 11 columns; match exactly.
func insertCounterSamples(ctx context.Context, t *testing.T, conn driver.Conn, rows []fixtureCounterSample) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO iface_counter_samples (
        ts, exporter, ifindex,
        in_octets, out_octets, in_packets, out_packets,
        in_errors, out_errors, in_discards, out_discards
    )`)
	if err != nil {
		t.Fatalf("prepare iface_counter_samples batch: %v", err)
	}
	for _, r := range rows {
		if err := batch.Append(
			r.TS,
			toIPv6Bytes(r.Exporter),
			r.IfIndex,
			r.InOctets,
			r.OutOctets,
			uint64(0), // in_packets — rule doesn't read this
			uint64(0), // out_packets
			r.InErrors,
			r.OutErrors,
			r.InDiscards,
			r.OutDiscards,
		); err != nil {
			t.Fatalf("append counter row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send counter batch: %v", err)
	}
}

/* ----------------------- InterfaceOperStatusChange ----------------------- */

// TestEvaluator_InterfaceOperStatusChange_FiresOnTransition seeds two
// snapshots for one (exporter, ifindex): an older one with oper_status=up
// and a recent one with oper_status=down. The rule must emit exactly one
// violation, severity=critical (because "down" is in the critical set),
// scope shaped as "<exporter> · <if_descr>".
func TestEvaluator_InterfaceOperStatusChange_FiresOnTransition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		// 10 minutes ago: up. Latest: down. Latest is within the 60s
		// debounce window so the rule must fire.
		{PolledAt: now.Add(-10 * time.Minute), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "down"},
		// Control: another interface with no transition. Must not fire.
		{PolledAt: now.Add(-10 * time.Minute), Exporter: "10.0.0.1", IfIndex: 8, IfDescr: "Gi0/0/8", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
		{PolledAt: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 8, IfDescr: "Gi0/0/8", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
	})

	rule := InterfaceOperStatusChange{DebounceSeconds: 60, LookbackHours: 24}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if got, want := v.Labels["exporter"], "10.0.0.1"; got != want {
		t.Errorf("exporter label = %q; want %q", got, want)
	}
	if got, want := v.Labels["ifindex"], "7"; got != want {
		t.Errorf("ifindex label = %q; want %q", got, want)
	}
	if got, want := v.Labels["prev_status"], "up"; got != want {
		t.Errorf("prev_status = %q; want %q", got, want)
	}
	if got, want := v.Labels["curr_status"], "down"; got != want {
		t.Errorf("curr_status = %q; want %q", got, want)
	}
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q (down should escalate)", v.Severity, SeverityCritical)
	}
	if v.GroupKey != "operstatus_10.0.0.1_7" {
		t.Errorf("group_key = %q; want operstatus_10.0.0.1_7", v.GroupKey)
	}
}

// TestEvaluator_InterfaceOperStatusChange_QuietOutsideDebounce confirms
// the rule does NOT fire when the latest snapshot is older than the
// debounce window. The transition is "stale" — the engine's stability
// window will have already auto-closed it.
func TestEvaluator_InterfaceOperStatusChange_QuietOutsideDebounce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		// Transition happened 5 minutes ago — older than the 60s debounce
		// window. Rule must stay quiet.
		{PolledAt: now.Add(-1 * time.Hour), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
		{PolledAt: now.Add(-5 * time.Minute), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "down"},
	})

	rule := InterfaceOperStatusChange{DebounceSeconds: 60, LookbackHours: 24}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations (transition outside debounce), got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_InterfaceOperStatusChange_DebounceOverride exercises the
// loader→rule plumbing: an alert_rule_settings row sets debounce_seconds
// to 600 (10 min). A transition 5 minutes old now falls INSIDE the
// override window, so the rule fires. Same data shape that the
// "Quiet" test above uses to confirm the override actually changed
// behavior.
func TestEvaluator_InterfaceOperStatusChange_DebounceOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	store := settings.New(h.Conn, nil)
	if err := store.AlertRules.Upsert(ctx, settings.AlertRuleSetting{
		RuleID:  "interface_oper_status_change",
		Enabled: true,
		Params:  map[string]any{"debounce_seconds": float64(600), "lookback_hours": float64(24)},
	}, "test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Hour), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
		{PolledAt: now.Add(-5 * time.Minute), Exporter: "10.0.0.1", IfIndex: 7, IfDescr: "Gi0/0/7", IfSpeedBps: 1_000_000_000, IfOperStatus: "down"},
	})

	rules, _, err := LoadRules(ctx, store.AlertRules)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var rule InterfaceOperStatusChange
	var found bool
	for _, r := range rules {
		if x, ok := r.(InterfaceOperStatusChange); ok {
			rule = x
			found = true
		}
	}
	if !found {
		t.Fatal("InterfaceOperStatusChange missing from loaded rule set")
	}
	if rule.DebounceSeconds != 600 {
		t.Fatalf("loaded rule debounce_seconds = %d; want 600 (override didn't stick)", rule.DebounceSeconds)
	}

	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation with 600s debounce override, got %d: %+v", len(violations), violations)
	}
}

/* ----------------------- InterfaceUtilizationHigh ----------------------- */

// TestEvaluator_InterfaceUtilizationHigh_FiresAboveThreshold seeds a
// 1Gbps interface with counter samples showing a 950 Mbps load — above
// the 80% default threshold AND above the 95% critical bump. We assert
// the violation labels match and severity is critical.
func TestEvaluator_InterfaceUtilizationHigh_FiresAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	// SNMP inventory: 1 Gbps interface.
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 9, IfDescr: "Gi0/0/9", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
	})

	// Counter samples over a 300s window: 950 Mbps avg.
	// 950 Mbps × 300s = 285_000_000_000 bits = 35_625_000_000 bytes total.
	// Split half in/half out: each direction deltas by ~17_812_500_000 octets.
	// Anchor the oldest sample a couple of seconds inside the window so
	// any drift between Go's `time.Now()` and CH's `now()` can't push
	// it outside the WHERE clause.
	const totalOctetsDelta uint64 = 35_625_000_000 / 2
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: 1_000_000, OutOctets: 1_000_000},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: 1_000_000 + totalOctetsDelta, OutOctets: 1_000_000 + totalOctetsDelta},
	})

	rule := InterfaceUtilizationHigh{ThresholdPct: 80, CriticalBumpPct: 15, WindowSeconds: 300}
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
	if v.Labels["ifindex"] != "9" {
		t.Errorf("ifindex label = %q", v.Labels["ifindex"])
	}
	// pct should round to 95 (warn) or higher. Threshold+bump = 95.
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q (≥95%% should be critical)", v.Severity, SeverityCritical)
	}
	if v.GroupKey != "util_10.0.0.1_9" {
		t.Errorf("group_key = %q; want util_10.0.0.1_9", v.GroupKey)
	}
}

// TestEvaluator_InterfaceUtilizationHigh_QuietBelowThreshold confirms
// no violation when bps is well under the configured percentage. The
// "Just below threshold" check guards against off-by-one in the SQL
// HAVING clause.
func TestEvaluator_InterfaceUtilizationHigh_QuietBelowThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 9, IfDescr: "Gi0/0/9", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
	})

	// 100 Mbps over 300s on a 1Gbps link → 10% utilization. Well below
	// the default 80% threshold.
	const totalOctetsDelta uint64 = 100_000_000 * 300 / 8 / 2
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: 0, OutOctets: 0},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: totalOctetsDelta, OutOctets: totalOctetsDelta},
	})

	rule := InterfaceUtilizationHigh{ThresholdPct: 80, CriticalBumpPct: 15, WindowSeconds: 300}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations (10%% < 80%%), got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_InterfaceUtilizationHigh_ThresholdOverride drops the
// threshold from 80% to 5% via alert_rule_settings. The same low-traffic
// fixture from the "Quiet" test now exceeds the override → one
// violation. Verifies the threshold_pct parameter override flows
// through LoadRules into the running rule.
func TestEvaluator_InterfaceUtilizationHigh_ThresholdOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	store := settings.New(h.Conn, nil)
	if err := store.AlertRules.Upsert(ctx, settings.AlertRuleSetting{
		RuleID:  "interface_utilization_high",
		Enabled: true,
		Params: map[string]any{
			"threshold_pct":     float64(5),
			"critical_bump_pct": float64(15),
			"window_seconds":    float64(300),
		},
	}, "test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 9, IfDescr: "Gi0/0/9", IfSpeedBps: 1_000_000_000, IfOperStatus: "up"},
	})
	// 100 Mbps on 1 Gbps link = 10% utilization, above the 5% override.
	const totalOctetsDelta uint64 = 100_000_000 * 300 / 8 / 2
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: 0, OutOctets: 0},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 9, InOctets: totalOctetsDelta, OutOctets: totalOctetsDelta},
	})

	rules, _, err := LoadRules(ctx, store.AlertRules)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var rule InterfaceUtilizationHigh
	var found bool
	for _, r := range rules {
		if x, ok := r.(InterfaceUtilizationHigh); ok {
			rule = x
			found = true
		}
	}
	if !found {
		t.Fatal("InterfaceUtilizationHigh missing from loaded rule set")
	}
	if rule.ThresholdPct != 5 {
		t.Fatalf("loaded ThresholdPct = %d; want 5 (override didn't stick)", rule.ThresholdPct)
	}

	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation with 5%% threshold override, got %d: %+v", len(violations), violations)
	}
}

/* ----------------------- InterfaceErrorsRate ----------------------- */

// TestEvaluator_InterfaceErrorsRate_FiresAboveThreshold seeds counter
// samples whose combined error+discard delta over the trailing 300s
// produces ~30/min, well above the default 10/min threshold.
func TestEvaluator_InterfaceErrorsRate_FiresAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 11, IfDescr: "Te1/0/11", IfSpeedBps: 10_000_000_000, IfOperStatus: "up"},
	})

	// Want >= 10 per minute over 300s window.
	// per_min = errs * 60 / window. With window=300, errs=150 → per_min=30.
	// Split across the four counter columns.
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 11, InErrors: 0, OutErrors: 0, InDiscards: 0, OutDiscards: 0},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 11, InErrors: 30, OutErrors: 30, InDiscards: 50, OutDiscards: 40},
	})

	rule := InterfaceErrorsRate{WindowSeconds: 300, ErrorsPerMin: 10}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["exporter"] != "10.0.0.1" || v.Labels["ifindex"] != "11" {
		t.Errorf("labels: %v", v.Labels)
	}
	if v.GroupKey != "errs_10.0.0.1_11" {
		t.Errorf("group_key = %q; want errs_10.0.0.1_11", v.GroupKey)
	}
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning", v.Severity)
	}
	if v.Labels["per_min"] != "30" {
		t.Errorf("per_min label = %q; want 30", v.Labels["per_min"])
	}
}

// TestEvaluator_InterfaceErrorsRate_QuietBelowThreshold confirms zero
// violations when the trailing rate is just under the threshold. Guards
// the >= comparison from regressing to >.
func TestEvaluator_InterfaceErrorsRate_QuietBelowThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 11, IfDescr: "Te1/0/11", IfSpeedBps: 10_000_000_000, IfOperStatus: "up"},
	})

	// 5 errors over 300s = 1/min. Threshold 10/min.
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 11},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 11, InErrors: 2, OutErrors: 1, InDiscards: 1, OutDiscards: 1},
	})

	rule := InterfaceErrorsRate{WindowSeconds: 300, ErrorsPerMin: 10}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations, got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_InterfaceErrorsRate_ThresholdOverride drops the
// threshold to 1/min via alert_rule_settings. The "Quiet" fixture
// above (1 err/min) now trips the override → one violation.
func TestEvaluator_InterfaceErrorsRate_ThresholdOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	store := settings.New(h.Conn, nil)
	if err := store.AlertRules.Upsert(ctx, settings.AlertRuleSetting{
		RuleID:  "interface_errors_rate",
		Enabled: true,
		Params:  map[string]any{"window_seconds": float64(300), "errors_per_min": float64(1)},
	}, "test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	now := time.Now().UTC()
	insertIfaces(ctx, t, h.Conn, []fixtureIface{
		{PolledAt: now.Add(-1 * time.Minute), Exporter: "10.0.0.1", IfIndex: 11, IfDescr: "Te1/0/11", IfSpeedBps: 10_000_000_000, IfOperStatus: "up"},
	})
	// 5 errors over 300s = 1/min. Exactly hits the override threshold (>=).
	insertCounterSamples(ctx, t, h.Conn, []fixtureCounterSample{
		{TS: now.Add(-295 * time.Second), Exporter: "10.0.0.1", IfIndex: 11},
		{TS: now.Add(-5 * time.Second), Exporter: "10.0.0.1", IfIndex: 11, InErrors: 2, OutErrors: 1, InDiscards: 1, OutDiscards: 1},
	})

	rules, _, err := LoadRules(ctx, store.AlertRules)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var rule InterfaceErrorsRate
	var found bool
	for _, r := range rules {
		if x, ok := r.(InterfaceErrorsRate); ok {
			rule = x
			found = true
		}
	}
	if !found {
		t.Fatal("InterfaceErrorsRate missing from loaded rule set")
	}
	if rule.ErrorsPerMin != 1 {
		t.Fatalf("loaded ErrorsPerMin = %d; want 1 (override didn't stick)", rule.ErrorsPerMin)
	}

	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation with 1/min override, got %d: %+v", len(violations), violations)
	}
}

/* ----------------------- TopTalkerBaselineAnomaly ----------------------- */

// baselineAnchorPair returns (now, this_hour) for the baseline-anomaly
// fixture. now is `time.Now().UTC()`; this_hour is its start-of-hour
// truncation. Tests anchor the "current hour" flow on `now` (so a small
// drift between the test's wall clock and ClickHouse's `now()` still
// lands the row inside whichever hour CH considers current) and anchor
// the baseline buckets on `this_hour - N*24h` so they fall on the same
// hour-of-day. The rule's SQL uses `toStartOfHour(now())` as its
// pivot.
//
// Edge case: if test execution straddles a wall-clock hour boundary,
// CH's `this_hour` may differ from the test's `this_hour` by one hour.
// Each test seeds its current-hour row at `now()` directly (not at a
// computed offset from `this_hour`), which keeps the current-hour
// bucket inside CH's `this_hour` no matter which side of the boundary
// wins.
func baselineAnchorPair() (time.Time, time.Time) {
	now := time.Now().UTC()
	return now, now.Truncate(time.Hour)
}

// TestEvaluator_TopTalkerBaselineAnomaly_FiresOnAnomaly seeds:
//   - 6 baseline buckets at the same hour-of-day across the prior 7 days,
//     each with 1 GB of traffic for one (src,dst) pair (baseline_avg = 1 GB).
//   - One bucket in the current hour with 5 GB for the same pair
//     (5× the baseline, threshold 3× → fires).
//   - One quiet pair below the min_baseline_bytes floor (must not fire).
func TestEvaluator_TopTalkerBaselineAnomaly_FiresOnAnomaly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now, thisHour := baselineAnchorPair()

	const (
		baselineBytesPerDay uint64 = 1_000_000_000 // 1 GB — equals min_baseline_bytes floor
		currentHourBytes    uint64 = 5_000_000_000 // 5× baseline
		quietBytes          uint64 = 100_000_000   // 100 MB — under min_baseline floor
	)

	rows := []fixtureFlow{
		// Current-hour traffic for the anomalous pair. Seeded at `now`
		// so it co-locates with ClickHouse's `toStartOfHour(now())`.
		{Observed: now, Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: currentHourBytes, Packets: 1000},
	}
	// Six prior-day baseline buckets at the same hour-of-day. Use
	// `thisHour - d*24h + 1m` so each row lands cleanly inside its
	// own hour bucket (not on the boundary).
	for d := 1; d <= 6; d++ {
		rows = append(rows, fixtureFlow{
			Observed: thisHour.Add(-time.Duration(d) * 24 * time.Hour).Add(1 * time.Minute),
			Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1",
			Bytes: baselineBytesPerDay, Packets: 100,
		})
	}
	// Quiet pair: baseline well under the 1 GB floor. Should be filtered
	// out by `baseline_avg >= min_baseline_bytes`.
	rows = append(rows,
		fixtureFlow{Observed: now, Exporter: "10.0.0.1", SrcAddr: "10.3.0.1", DstAddr: "10.4.0.1", Bytes: quietBytes * 10, Packets: 10},
		fixtureFlow{Observed: thisHour.Add(-24 * time.Hour).Add(2 * time.Minute), Exporter: "10.0.0.1", SrcAddr: "10.3.0.1", DstAddr: "10.4.0.1", Bytes: quietBytes, Packets: 10},
	)
	insertFlows(ctx, t, h.Conn, rows)

	rule := TopTalkerBaselineAnomaly{Multiplier: 3.0, MinBaselineBytes: 1_000_000_000}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Labels["src_addr"] != "10.1.0.1" {
		t.Errorf("src_addr label = %q; want 10.1.0.1", v.Labels["src_addr"])
	}
	if v.Labels["dst_addr"] != "10.2.0.1" {
		t.Errorf("dst_addr label = %q; want 10.2.0.1", v.Labels["dst_addr"])
	}
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning", v.Severity)
	}
	if v.GroupKey != "baseline_10.1.0.1_10.2.0.1" {
		t.Errorf("group_key = %q; want baseline_10.1.0.1_10.2.0.1", v.GroupKey)
	}
}

// TestEvaluator_TopTalkerBaselineAnomaly_QuietBelowMultiplier confirms
// no violation when the current hour is only 2× the baseline — below
// the default 3× threshold.
func TestEvaluator_TopTalkerBaselineAnomaly_QuietBelowMultiplier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now, thisHour := baselineAnchorPair()
	const baselineBytesPerDay uint64 = 1_000_000_000
	const currentHourBytes uint64 = 2_000_000_000 // 2× baseline, below 3× threshold

	rows := []fixtureFlow{
		{Observed: now, Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: currentHourBytes, Packets: 100},
	}
	for d := 1; d <= 6; d++ {
		rows = append(rows, fixtureFlow{
			Observed: thisHour.Add(-time.Duration(d) * 24 * time.Hour).Add(1 * time.Minute),
			Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1",
			Bytes: baselineBytesPerDay, Packets: 100,
		})
	}
	insertFlows(ctx, t, h.Conn, rows)

	rule := TopTalkerBaselineAnomaly{Multiplier: 3.0, MinBaselineBytes: 1_000_000_000}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations (2× < 3× threshold), got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_TopTalkerBaselineAnomaly_MultiplierOverride drops the
// multiplier to 1.5× via alert_rule_settings. The "Quiet" fixture above
// (2× baseline) now trips it. Verifies both `multiplier` and
// `min_baseline_bytes` flow through the loader.
func TestEvaluator_TopTalkerBaselineAnomaly_MultiplierOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	store := settings.New(h.Conn, nil)
	if err := store.AlertRules.Upsert(ctx, settings.AlertRuleSetting{
		RuleID:  "top_talker_baseline_anomaly",
		Enabled: true,
		Params: map[string]any{
			"multiplier":         float64(1.5),
			"min_baseline_bytes": float64(1_000_000_000),
		},
	}, "test"); err != nil {
		t.Fatalf("upsert override: %v", err)
	}

	now, thisHour := baselineAnchorPair()
	const baselineBytesPerDay uint64 = 1_000_000_000
	const currentHourBytes uint64 = 2_000_000_000 // 2× baseline → trips 1.5× override

	rows := []fixtureFlow{
		{Observed: now, Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: currentHourBytes, Packets: 100},
	}
	for d := 1; d <= 6; d++ {
		rows = append(rows, fixtureFlow{
			Observed: thisHour.Add(-time.Duration(d) * 24 * time.Hour).Add(1 * time.Minute),
			Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1",
			Bytes: baselineBytesPerDay, Packets: 100,
		})
	}
	insertFlows(ctx, t, h.Conn, rows)

	rules, _, err := LoadRules(ctx, store.AlertRules)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	var rule TopTalkerBaselineAnomaly
	var found bool
	for _, r := range rules {
		if x, ok := r.(TopTalkerBaselineAnomaly); ok {
			rule = x
			found = true
		}
	}
	if !found {
		t.Fatal("TopTalkerBaselineAnomaly missing from loaded rule set")
	}
	if rule.Multiplier != 1.5 {
		t.Fatalf("loaded Multiplier = %v; want 1.5 (override didn't stick)", rule.Multiplier)
	}

	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation with 1.5× override, got %d: %+v", len(violations), violations)
	}
}
