//go:build integration

// Integration tests for the alerteng rule evaluators against a real
// ClickHouse instance. Run with:
//
//	go test -race -tags=integration ./internal/alerteng/...
//
// Docker must be available locally; CI provides a Linux runner with
// the Docker socket mounted. See test/integration/README.md.
//
// These tests deliberately do NOT use t.Parallel(): each shares the
// same container and truncates fixture tables between cases. Serial
// execution keeps the data setup small and obvious. Future slices that
// need parallel runs can spin per-test containers — at the cost of one
// pull per test.
package alerteng

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/test/integration"
)

// fixtureFlow is the minimal subset of flow columns the rules read.
// Fields not relevant to the assertions get zero values — that mirrors
// the production NetFlow path where AS / VLAN / ToS may be absent.
type fixtureFlow struct {
	Observed time.Time
	Exporter string // dotted-quad or IPv6 literal
	SrcAddr  string
	DstAddr  string
	Bytes    uint64
	Packets  uint64
}

// insertFlows writes the given fixtures via PrepareBatch. The schema
// defined in 000001_init.sql + 000007_asn.sql expects 17 columns —
// match that exactly so the batch lands in one round trip.
func insertFlows(ctx context.Context, t *testing.T, conn driver.Conn, rows []fixtureFlow) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO flows (
        observed, exporter, src_addr, dst_addr,
        src_port, dst_port, proto, bytes, packets,
        input_ifindex, output_ifindex, vlan_id, tos, tcp_flags, source,
        src_as, dst_as
    )`)
	if err != nil {
		t.Fatalf("prepare batch: %v", err)
	}
	for _, r := range rows {
		if err := batch.Append(
			r.Observed,
			toIPv6Bytes(r.Exporter),
			toIPv6Bytes(r.SrcAddr),
			toIPv6Bytes(r.DstAddr),
			uint16(0), // src_port
			uint16(0), // dst_port
			uint8(6),  // proto = TCP
			r.Bytes,
			r.Packets,
			uint32(0),                          // input_ifindex
			uint32(0),                          // output_ifindex
			uint16(0),                          // vlan_id
			uint8(0),                           // tos
			uint8(0),                           // tcp_flags
			"netflow",                          // source
			uint32(0),                          // src_as
			uint32(0),                          // dst_as
		); err != nil {
			t.Fatalf("append row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send batch: %v", err)
	}
}

// toIPv6Bytes parses a dotted-quad or IPv6 literal into the 16-byte
// big-endian form ClickHouse's IPv6 column expects. Mirrors the helper
// in internal/store/batcher.go but kept local so this test file does
// not reach into store internals.
func toIPv6Bytes(s string) net.IP {
	addr := netip.MustParseAddr(s)
	a := addr.As16()
	return a[:]
}

// TestEvaluator_HeavyTalker_FiresAboveThreshold seeds two src→dst
// pairs: one heavy enough to trip a low threshold, one well under.
// We expect exactly one violation, with the labels and scope shaped
// the way the alerts UI consumes them.
func TestEvaluator_HeavyTalker_FiresAboveThreshold(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertFlows(ctx, t, h.Conn, []fixtureFlow{
		// Heavy pair: 2 GiB across two flow records. Above threshold.
		{Observed: now.Add(-30 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.10", DstAddr: "10.2.0.20", Bytes: 1 << 30, Packets: 1000},
		{Observed: now.Add(-10 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.10", DstAddr: "10.2.0.20", Bytes: 1 << 30, Packets: 1000},
		// Light pair: 1 KiB total. Below any reasonable threshold.
		{Observed: now.Add(-5 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.3.0.30", DstAddr: "10.4.0.40", Bytes: 1024, Packets: 1},
	})

	// Use a 1 GiB threshold so the heavy pair (2 GiB) trips it and
	// the light pair (1 KiB) does not. Window covers everything seeded.
	rule := HeavyTalker{WindowSeconds: 300, BytesThreshold: 1 << 30}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if got, want := v.Labels["src_addr"], "10.1.0.10"; got != want {
		t.Errorf("src_addr label = %q; want %q", got, want)
	}
	if got, want := v.Labels["dst_addr"], "10.2.0.20"; got != want {
		t.Errorf("dst_addr label = %q; want %q", got, want)
	}
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want %q", v.Severity, SeverityWarning)
	}
	if v.Scope == "" || v.GroupKey == "" {
		t.Errorf("scope/group_key must be set: scope=%q group_key=%q", v.Scope, v.GroupKey)
	}
}

// TestEvaluator_HeavyTalker_QuietDataPlane confirms zero false
// positives when no pair crosses the threshold. Important: the rule
// must not flag low-volume conversations.
func TestEvaluator_HeavyTalker_QuietDataPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertFlows(ctx, t, h.Conn, []fixtureFlow{
		{Observed: now.Add(-10 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: 4096, Packets: 4},
		{Observed: now.Add(-5 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.2", DstAddr: "10.2.0.2", Bytes: 8192, Packets: 8},
	})

	rule := HeavyTalker{WindowSeconds: 300, BytesThreshold: 1 << 30}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations, got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_ExporterSilent_FiresWhenSilent puts an exporter in the
// "active in last 10m, quiet in last 60s" window — the canonical
// silent-exporter case. We expect one violation pinned to that IP.
func TestEvaluator_ExporterSilent_FiresWhenSilent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	// Silent exporter: produced flows 5m ago, nothing recent.
	// Active exporter: produced flows both 5m ago and 5s ago.
	insertFlows(ctx, t, h.Conn, []fixtureFlow{
		{Observed: now.Add(-5 * time.Minute), Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: 1024, Packets: 1},
		{Observed: now.Add(-5 * time.Minute), Exporter: "10.0.0.2", SrcAddr: "10.1.0.2", DstAddr: "10.2.0.2", Bytes: 1024, Packets: 1},
		{Observed: now.Add(-5 * time.Second), Exporter: "10.0.0.2", SrcAddr: "10.1.0.2", DstAddr: "10.2.0.2", Bytes: 1024, Packets: 1},
	})

	rule := ExporterSilent{SilentSeconds: 60, ActiveSeconds: 600}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(violations), violations)
	}
	v := violations[0]
	if v.Scope != "10.0.0.1" {
		t.Errorf("scope = %q; want %q", v.Scope, "10.0.0.1")
	}
	if v.Labels["exporter"] != "10.0.0.1" {
		t.Errorf("exporter label = %q; want %q", v.Labels["exporter"], "10.0.0.1")
	}
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q", v.Severity, SeverityCritical)
	}
}

// TestEvaluator_ExporterSilent_AllActive — every exporter in the
// active window has reported recently, so nothing should fire. Guards
// against regressions where the NOT IN subquery flips polarity.
func TestEvaluator_ExporterSilent_AllActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertFlows(ctx, t, h.Conn, []fixtureFlow{
		{Observed: now.Add(-300 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: 1024, Packets: 1},
		{Observed: now.Add(-5 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: 1024, Packets: 1},
		{Observed: now.Add(-5 * time.Second), Exporter: "10.0.0.2", SrcAddr: "10.1.0.2", DstAddr: "10.2.0.2", Bytes: 1024, Packets: 1},
	})

	rule := ExporterSilent{SilentSeconds: 60, ActiveSeconds: 600}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations, got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_ExporterSilent_EmptyDataPlane — no flows at all
// means no exporter is "active", so the rule has nothing to compare
// against and must return zero violations. Edge case the production
// engine hits for a few seconds at fresh startup.
func TestEvaluator_ExporterSilent_EmptyDataPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	rule := ExporterSilent{SilentSeconds: 60, ActiveSeconds: 600}
	violations, err := rule.Evaluate(ctx, h.Conn)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations on empty flows table, got %d: %+v", len(violations), violations)
	}
}

// TestEvaluator_DefaultRules_Compose verifies that DefaultRules() can
// be evaluated end-to-end against a populated ClickHouse without
// errors — guards against schema/SQL drift between the rule package
// and the migrations.
func TestEvaluator_DefaultRules_Compose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	h.Truncate(ctx, t)

	now := time.Now().UTC()
	insertFlows(ctx, t, h.Conn, []fixtureFlow{
		{Observed: now.Add(-10 * time.Second), Exporter: "10.0.0.1", SrcAddr: "10.1.0.1", DstAddr: "10.2.0.1", Bytes: 4096, Packets: 4},
	})

	for _, r := range DefaultRules() {
		if _, err := r.Evaluate(ctx, h.Conn); err != nil {
			t.Errorf("rule %s: Evaluate error: %v", r.ID(), err)
		}
	}
}
