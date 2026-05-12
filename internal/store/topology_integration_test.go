//go:build integration

// Integration tests for QueryTopology. Spin up a real ClickHouse,
// seed lldp_neighbors + device_inventory + exporter_health, and
// assert the node/edge dedup behaviour.
//
// Run with:
//
//	go test -tags=integration ./internal/store/...
package store

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/test/integration"
)

// toIPv6BytesT is the test helper — store has toIPv6 already but it
// takes a netip.Addr, and we want raw strings for fixture ergonomics.
func toIPv6BytesT(s string) net.IP {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	a := addr.As16()
	return a[:]
}

type neighborFixture struct {
	LastSeen           time.Time
	LocalExporter      string
	LocalIfIndex       uint32
	LocalPortName      string
	Proto              string
	RemoteChassisID    string
	RemoteSysName      string
	RemoteSysDesc      string
	RemotePortID       string
	RemoteCaps         string
	RemoteManagement   string // optional; empty → NULL
}

func insertNeighbors(ctx context.Context, t *testing.T, conn driver.Conn, rows []neighborFixture) {
	t.Helper()
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO lldp_neighbors (
        last_seen, first_seen, local_exporter, local_ifindex, local_port_name,
        discovery_proto, remote_chassis_id, remote_sys_name, remote_sys_desc,
        remote_port_id, remote_capabilities, remote_management_addr
    )`)
	if err != nil {
		t.Fatalf("prepare lldp_neighbors batch: %v", err)
	}
	for _, r := range rows {
		var mgmt any
		if r.RemoteManagement != "" {
			mgmt = toIPv6BytesT(r.RemoteManagement)
		}
		if err := batch.Append(
			r.LastSeen, r.LastSeen, toIPv6BytesT(r.LocalExporter),
			r.LocalIfIndex, r.LocalPortName,
			r.Proto, r.RemoteChassisID, r.RemoteSysName, r.RemoteSysDesc,
			r.RemotePortID, r.RemoteCaps, mgmt,
		); err != nil {
			t.Fatalf("append neighbor row: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send neighbor batch: %v", err)
	}
}

func insertInventoryT(ctx context.Context, t *testing.T, conn driver.Conn, exporter, sysName, sysDescr string) {
	t.Helper()
	if err := conn.Exec(ctx,
		`INSERT INTO device_inventory
		   (polled_at, exporter, sys_descr, sys_object_id, sys_uptime_ms,
		    sys_name, sys_location, sys_contact, iface_count,
		    poll_duration_ms, poll_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), toIPv6BytesT(exporter), sysDescr, "1.3.6.1.4.1.9.1.2370",
		uint64(0), sysName, "", "", uint32(0), uint32(0), "ok",
	); err != nil {
		t.Fatalf("insert inventory: %v", err)
	}
}

func insertHealthT(ctx context.Context, t *testing.T, conn driver.Conn, exporter string, ts time.Time) {
	t.Helper()
	if err := conn.Exec(ctx,
		`INSERT INTO exporter_health (ts, exporter, source, datagrams, seq_gaps, last_seq)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ts, toIPv6BytesT(exporter), "netflow_v9", uint64(100), uint64(0), uint32(0),
	); err != nil {
		t.Fatalf("insert exporter_health: %v", err)
	}
}

// TestQueryTopology_Empty covers the "no data yet" path that the UI's
// empty state depends on. Tables must be empty and the response well-
// formed (non-nil slices) so JSON encoding never emits null.
func TestQueryTopology_Empty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)
	// Don't bother truncating — empty container is already clean.

	resp, err := QueryTopology(ctx, h.Conn)
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response on empty topology")
	}
	if len(resp.Nodes) != 0 {
		t.Errorf("want 0 nodes, got %d", len(resp.Nodes))
	}
	if len(resp.Edges) != 0 {
		t.Errorf("want 0 edges, got %d", len(resp.Edges))
	}
}

// TestQueryTopology_BidirectionalDedup is the core invariant from
// TASKS.md P3 #20: when both ends of a link have LLDP enabled, the
// API surfaces ONE edge, not two. Seed A→B and B→A, expect a single
// edge with both port labels backfilled.
func TestQueryTopology_BidirectionalDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	now := time.Now().UTC()
	// Both devices walked.
	insertInventoryT(ctx, t, h.Conn, "10.0.0.1", "core-01", "Cisco C9500")
	insertInventoryT(ctx, t, h.Conn, "10.0.0.2", "core-02", "Cisco C9500")
	// Both reachable.
	insertHealthT(ctx, t, h.Conn, "10.0.0.1", now)
	insertHealthT(ctx, t, h.Conn, "10.0.0.2", now)

	insertNeighbors(ctx, t, h.Conn, []neighborFixture{
		{
			LastSeen: now, LocalExporter: "10.0.0.1", LocalIfIndex: 1,
			LocalPortName: "Te1/0/1", Proto: "lldp",
			RemoteChassisID: "aa:bb:cc:00:00:02", RemoteSysName: "core-02",
			RemotePortID: "Te1/0/1", RemoteCaps: "bridge,router",
			RemoteManagement: "10.0.0.2",
		},
		{
			LastSeen: now, LocalExporter: "10.0.0.2", LocalIfIndex: 1,
			LocalPortName: "Te1/0/1", Proto: "lldp",
			RemoteChassisID: "aa:bb:cc:00:00:01", RemoteSysName: "core-01",
			RemotePortID: "Te1/0/1", RemoteCaps: "bridge,router",
			RemoteManagement: "10.0.0.1",
		},
	})

	resp, err := QueryTopology(ctx, h.Conn)
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	// Two nodes (one per device), exactly one edge after dedup.
	if len(resp.Nodes) != 2 {
		t.Errorf("want 2 nodes after dedup, got %d: %+v", len(resp.Nodes), resp.Nodes)
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("want 1 edge after bidirectional dedup, got %d: %+v", len(resp.Edges), resp.Edges)
	}
	e := resp.Edges[0]
	// Lexicographically smaller IP is the source.
	if e.Source != "10.0.0.1" || e.Target != "10.0.0.2" {
		t.Errorf("edge endpoints not canonicalised: %s → %s", e.Source, e.Target)
	}
	if e.SourcePort == "" || e.TargetPort == "" {
		t.Errorf("expected both ports populated after backfill, got src=%q dst=%q",
			e.SourcePort, e.TargetPort)
	}
	if e.DiscoveryProto != "lldp" {
		t.Errorf("discovery_proto = %q, want lldp", e.DiscoveryProto)
	}
}

// TestQueryTopology_DiscoveredOnly covers the case where a walked
// device reports a neighbor we don't actively monitor. The remote
// must show up as a Discovered=true node with a synthetic
// "chassis:..." ID, not collapse into nothing.
func TestQueryTopology_DiscoveredOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	now := time.Now().UTC()
	insertInventoryT(ctx, t, h.Conn, "10.0.0.1", "core-01", "Cisco")
	insertHealthT(ctx, t, h.Conn, "10.0.0.1", now)

	insertNeighbors(ctx, t, h.Conn, []neighborFixture{
		{
			LastSeen: now, LocalExporter: "10.0.0.1", LocalIfIndex: 5,
			LocalPortName: "Te1/0/5", Proto: "cdp",
			RemoteChassisID: "unknown-ap-42", RemoteSysName: "unknown-ap-42",
			RemoteSysDesc: "Cisco AP", RemotePortID: "Gi0/0",
			RemoteCaps: "wlan-ap",
		},
	})

	resp, err := QueryTopology(ctx, h.Conn)
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	var discovered, known int
	for _, n := range resp.Nodes {
		if n.Discovered {
			discovered++
		} else {
			known++
		}
	}
	if known != 1 {
		t.Errorf("want 1 known node (10.0.0.1), got %d", known)
	}
	if discovered != 1 {
		t.Errorf("want 1 discovered-only node, got %d", discovered)
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("want 1 edge, got %d", len(resp.Edges))
	}
	if resp.Edges[0].DiscoveryProto != "cdp" {
		t.Errorf("edge proto = %q, want cdp", resp.Edges[0].DiscoveryProto)
	}
}

// TestQueryTopology_ReachabilityJoin verifies the exporter_health
// join correctly flags a walked-but-silent device as unreachable.
// last_seen > 5 min ago = unreachable.
func TestQueryTopology_ReachabilityJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	now := time.Now().UTC()
	insertInventoryT(ctx, t, h.Conn, "10.0.0.1", "core-01", "Cisco")
	insertInventoryT(ctx, t, h.Conn, "10.0.0.2", "core-02", "Cisco")
	insertHealthT(ctx, t, h.Conn, "10.0.0.1", now)             // fresh
	insertHealthT(ctx, t, h.Conn, "10.0.0.2", now.Add(-30*time.Minute)) // stale

	insertNeighbors(ctx, t, h.Conn, []neighborFixture{
		{
			LastSeen: now, LocalExporter: "10.0.0.1", LocalIfIndex: 1,
			Proto: "lldp", RemoteChassisID: "aa:bb:cc:00:00:02",
			RemoteSysName: "core-02", RemotePortID: "Te1/0/1",
			RemoteManagement: "10.0.0.2",
		},
	})

	resp, err := QueryTopology(ctx, h.Conn)
	if err != nil {
		t.Fatalf("QueryTopology: %v", err)
	}
	for _, n := range resp.Nodes {
		switch n.Address {
		case "10.0.0.1":
			if !n.Reachable {
				t.Errorf("10.0.0.1 should be reachable (fresh health row)")
			}
		case "10.0.0.2":
			if n.Reachable {
				t.Errorf("10.0.0.2 should be unreachable (stale health row)")
			}
		}
	}
}
