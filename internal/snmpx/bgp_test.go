package snmpx

import (
	"context"
	"testing"
)

// TestMockWalkBGP locks in the synthetic BGP shape the mock client
// returns so the alert engine has a stable signal in the dev loop.
// Keep this in sync with MockClient.WalkBGP — both peers are part of
// the contract: one Established eBGP and one Idle iBGP per device.
func TestMockWalkBGP(t *testing.T) {
	m := NewMock()
	got, err := m.WalkBGP(t.Context(), "10.0.0.42")
	if err != nil {
		t.Fatalf("WalkBGP: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 mock peers, got %d", len(got))
	}
	var sawEstablished, sawIdle bool
	for _, p := range got {
		if p.Exporter != "10.0.0.42" {
			t.Errorf("Exporter = %q; want 10.0.0.42", p.Exporter)
		}
		if p.Source != "bgp4" {
			t.Errorf("Source = %q; want bgp4", p.Source)
		}
		if p.AFI != "ipv4" || p.SAFI != "unicast" {
			t.Errorf("AFI/SAFI = %q/%q; want ipv4/unicast", p.AFI, p.SAFI)
		}
		switch p.State {
		case "established":
			sawEstablished = true
			if p.EstablishedAt.IsZero() {
				t.Errorf("Established peer should have non-zero EstablishedAt")
			}
		case "idle":
			sawIdle = true
		}
	}
	if !sawEstablished || !sawIdle {
		t.Errorf("expected both established and idle peers; established=%v idle=%v", sawEstablished, sawIdle)
	}
}

// TestMockWalkBGP_Determinism ensures repeat walks against the same
// target return the same peers (so ReplacingMergeTree / argMax stays
// stable in the dev loop and tests that depend on the mock are
// reproducible).
func TestMockWalkBGP_Determinism(t *testing.T) {
	m := NewMock()
	a, _ := m.WalkBGP(context.Background(), "10.0.0.7")
	b, _ := m.WalkBGP(context.Background(), "10.0.0.7")
	if len(a) != len(b) {
		t.Fatalf("walk count drift: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].PeerAddr != b[i].PeerAddr {
			t.Errorf("peer[%d].PeerAddr drifted: %q vs %q", i, a[i].PeerAddr, b[i].PeerAddr)
		}
		if a[i].PeerASN != b[i].PeerASN {
			t.Errorf("peer[%d].PeerASN drifted: %d vs %d", i, a[i].PeerASN, b[i].PeerASN)
		}
	}
}

func TestBgpPeerStateName(t *testing.T) {
	cases := map[int]string{
		1: "idle",
		2: "connect",
		3: "active",
		4: "opensent",
		5: "openconfirm",
		6: "established",
		0: "unknown",
		9: "unknown",
	}
	for v, want := range cases {
		if got := BgpPeerStateName(v); got != want {
			t.Errorf("BgpPeerStateName(%d) = %q; want %q", v, got, want)
		}
	}
}

func TestBgpAdminStatusName(t *testing.T) {
	if BgpAdminStatusName(1) != "stop" {
		t.Errorf("admin 1 should be stop")
	}
	if BgpAdminStatusName(2) != "start" {
		t.Errorf("admin 2 should be start")
	}
	if BgpAdminStatusName(0) != "" {
		t.Errorf("admin 0 should be empty")
	}
}
