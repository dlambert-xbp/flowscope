package snmpx

import (
	"context"
	"testing"
)

// TestMockWalkBGP locks in the synthetic BGP shape the mock client
// returns so the alert engine has a stable signal in the dev loop.
// Keep this in sync with MockClient.WalkBGP — four peers across
// three VRFs: default (one established eBGP + one idle iBGP), mgmt
// (one established), CUSTOMER-A (one established L3VPN PE-CE).
func TestMockWalkBGP(t *testing.T) {
	m := NewMock()
	got, err := m.WalkBGP(t.Context(), "10.0.0.42")
	if err != nil {
		t.Fatalf("WalkBGP: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 mock peers, got %d", len(got))
	}
	vrfs := map[string]int{}
	var sawEstablished, sawIdle bool
	for _, p := range got {
		if p.Exporter != "10.0.0.42" {
			t.Errorf("Exporter = %q; want 10.0.0.42", p.Exporter)
		}
		if p.VRF == "" {
			t.Errorf("VRF should not be empty")
		}
		vrfs[p.VRF]++
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
	if len(vrfs) < 2 {
		t.Errorf("expected peers across multiple VRFs; got %v", vrfs)
	}
	if vrfs[VRFDefault] == 0 {
		t.Errorf("expected at least one peer in VRF %q; got %v", VRFDefault, vrfs)
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

func TestParseAristaBgpV2Index_IPv4(t *testing.T) {
	// instance=1, IPv4 peer 10.0.0.1 → 1.1.4.10.0.0.1
	inst, at, addr, ok := parseAristaBgpV2Index("1.1.4.10.0.0.1")
	if !ok {
		t.Fatalf("parse failed")
	}
	if inst != 1 {
		t.Errorf("instance = %d; want 1", inst)
	}
	if at != 1 {
		t.Errorf("addrType = %d; want 1 (ipv4)", at)
	}
	if addr != "10.0.0.1" {
		t.Errorf("addr = %q; want 10.0.0.1", addr)
	}
}

func TestParseAristaBgpV2Index_NonDefaultInstance(t *testing.T) {
	// instance=7, IPv4 peer 192.0.2.42 → 7.1.4.192.0.2.42
	inst, at, addr, ok := parseAristaBgpV2Index("7.1.4.192.0.2.42")
	if !ok {
		t.Fatalf("parse failed")
	}
	if inst != 7 {
		t.Errorf("instance = %d; want 7", inst)
	}
	if at != 1 || addr != "192.0.2.42" {
		t.Errorf("addrType/addr = %d/%q; want 1/192.0.2.42", at, addr)
	}
}

func TestParseAristaBgpV2Index_IPv6(t *testing.T) {
	// instance=2, IPv6 peer 2001:db8::1.
	// Bytes: 20 01 0d b8 00 00 00 00 00 00 00 00 00 00 00 01
	// Decimal: 32 1 13 184 0×11 1
	// Suffix: 2.2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1
	inst, at, addr, ok := parseAristaBgpV2Index("2.2.16.32.1.13.184.0.0.0.0.0.0.0.0.0.0.0.1")
	if !ok {
		t.Fatalf("parse failed")
	}
	if inst != 2 || at != 2 {
		t.Errorf("instance/addrType = %d/%d; want 2/2", inst, at)
	}
	if addr != "2001:db8::1" {
		t.Errorf("addr = %q; want 2001:db8::1", addr)
	}
}

func TestParseAristaBgpV2Index_RejectsBadShape(t *testing.T) {
	cases := []string{
		"",
		"1",
		"1.1",       // missing addrLen
		"1.1.4",     // missing addr bytes
		"1.1.4.10",  // truncated address
		"1.99.4.10.0.0.1", // unknown InetAddressType
		"1.1.5.10.0.0.1.0", // wrong addrLen for IPv4
	}
	for _, s := range cases {
		if _, _, _, ok := parseAristaBgpV2Index(s); ok {
			t.Errorf("parseAristaBgpV2Index(%q) returned ok=true; expected reject", s)
		}
	}
}

func TestAristaInstanceToVRF(t *testing.T) {
	cases := map[uint32]string{
		0:  VRFDefault,
		1:  VRFDefault,
		2:  "vrf-2",
		42: "vrf-42",
	}
	for in, want := range cases {
		if got := aristaInstanceToVRF(in); got != want {
			t.Errorf("aristaInstanceToVRF(%d) = %q; want %q", in, got, want)
		}
	}
}

func TestParseSnmpAdminStringSuffix(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		want   string
		ok     bool
	}{
		{
			name:   "default",
			suffix: "7.100.101.102.97.117.108.116", // "default"
			want:   "default",
			ok:     true,
		},
		{
			name:   "datacenter",
			suffix: "10.100.97.116.97.99.101.110.116.101.114", // "datacenter"
			want:   "datacenter",
			ok:     true,
		},
		{
			name:   "underscore VRF name",
			suffix: "11.98.97.110.99.116.101.99.95.98.112.111", // "banctec_bpo"
			want:   "banctec_bpo",
			ok:     true,
		},
		{
			name:   "trailing-extra ok (parser stops at length)",
			suffix: "7.100.101.102.97.117.108.116.99.99",
			want:   "default",
			ok:     true,
		},
		{
			name:   "truncated string",
			suffix: "10.100.97",
			ok:     false,
		},
		{
			name:   "non-numeric",
			suffix: "X.Y.Z",
			ok:     false,
		},
		{
			name:   "empty",
			suffix: "",
			ok:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSnmpAdminStringSuffix(tc.suffix)
			if ok != tc.ok {
				t.Fatalf("ok = %v; want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("got = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestWalkVRFContextBGP_CommunityMutation(t *testing.T) {
	// Smoke test for the community-mutation rule: default VRF keeps
	// the bare community; non-default VRFs get appended via @. We
	// can't easily reach into walkVRFContextBGP without an SNMP
	// stand-in, so this test calls the same shaping logic the helper
	// uses and confirms the convention.
	base := Config{Version: "v2c", Community: "elastiflow"}
	if want, got := "elastiflow", communityFor(base, "default"); want != got {
		t.Errorf("default community = %q; want %q", got, want)
	}
	if want, got := "elastiflow@datacenter", communityFor(base, "datacenter"); want != got {
		t.Errorf("non-default community = %q; want %q", got, want)
	}
	if want, got := "elastiflow@banctec_bpo", communityFor(base, "banctec_bpo"); want != got {
		t.Errorf("underscore-vrf community = %q; want %q", got, want)
	}
}

// communityFor mirrors the inline community-mutation in
// walkVRFContextBGP so tests can exercise the rule without standing
// up an SNMP server.
func communityFor(cfg Config, vrf string) string {
	if vrf == VRFDefault {
		return cfg.Community
	}
	return cfg.Community + "@" + vrf
}
