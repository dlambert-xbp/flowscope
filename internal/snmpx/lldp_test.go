package snmpx

import (
	"strings"
	"testing"
)

// TestDecodeChassisID locks in the chassis-id subtype matrix
// described in CLAUDE.md / TASKS.md P3 #20. Subtype 4 is a MAC, 5 is
// a network address, 7 is a printable string. Anything weirder than
// those falls back to lowercase hex so we never drop the row.
func TestDecodeChassisID(t *testing.T) {
	cases := []struct {
		name    string
		subtype int
		raw     []byte
		want    string
	}{
		{"mac (subtype 4)", 4, []byte{0xAA, 0xBB, 0xCC, 0x01, 0x02, 0x03}, "aa:bb:cc:01:02:03"},
		{"ipv4 (subtype 5 / AF=1)", 5, []byte{0x01, 0x0A, 0x00, 0x00, 0x01}, "10.0.0.1"},
		{"ipv6 (subtype 5 / AF=2)", 5,
			[]byte{0x02, 0x20, 0x01, 0x0D, 0xB8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01},
			"2001:db8::1"},
		{"local string (subtype 7)", 7, []byte("core-edge-01"), "core-edge-01"},
		{"chassis component (subtype 1)", 1, []byte("Slot 1"), "Slot 1"},
		{"interface name (subtype 6)", 6, []byte("GigabitEthernet0/0/1"), "GigabitEthernet0/0/1"},
		{"mac with wrong length (fallback to hex)", 4, []byte{0xAA, 0xBB, 0xCC}, "aabbcc"},
		{"unknown subtype 99 (hex fallback)", 99, []byte{0xDE, 0xAD, 0xBE, 0xEF}, "deadbeef"},
		{"sanitised embedded null", 7, []byte{'h', 'i', 0x00, '!'}, "hi!"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeChassisID(c.subtype, c.raw)
			if got != c.want {
				t.Errorf("decodeChassisID(%d, %x) = %q; want %q", c.subtype, c.raw, got, c.want)
			}
		})
	}
}

// TestDecodePortID covers the symmetric subtype matrix on the
// lldpRemPortId column.
func TestDecodePortID(t *testing.T) {
	cases := []struct {
		name     string
		subtype  int
		raw      []byte
		portDesc string
		want     string
	}{
		{"mac (subtype 3)", 3, []byte{0x00, 0x1B, 0x44, 0x11, 0x3A, 0xB7}, "", "00:1b:44:11:3a:b7"},
		{"interface name (subtype 5)", 5, []byte("Te1/0/47"), "", "Te1/0/47"},
		{"ifindex stub falls back to portDesc", 0, []byte{0x00, 0x01}, "ifindex 2", "ifindex 2"},
		{"local string (subtype 7)", 7, []byte("Gi0/1"), "", "Gi0/1"},
		{"ipv4 network addr (subtype 4 / AF=1)", 4, []byte{0x01, 0xC0, 0xA8, 0x01, 0x01}, "", "192.168.1.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodePortID(c.subtype, c.raw, c.portDesc)
			if got != c.want {
				t.Errorf("decodePortID(%d, %x, %q) = %q; want %q",
					c.subtype, c.raw, c.portDesc, got, c.want)
			}
		})
	}
}

// TestDecodeLLDPCapabilities exercises the 16-bit system-capabilities
// bitmap from RFC 4363 / IEEE 802.1AB §11.5.6. Order matters because
// the API surfaces this string in the JSON response and React renders
// it directly.
func TestDecodeLLDPCapabilities(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"empty bytes → empty string", []byte{}, ""},
		{"only router bit", []byte{0x08, 0x00}, "router"},
		{"bridge + router (switch+L3)", []byte{0x28, 0x00}, "bridge,router"},
		{"wlan-ap only", []byte{0x10, 0x00}, "wlan-ap"},
		{"all eleven bits", []byte{0xFF, 0xE0}, "other,repeater,bridge,wlan-ap,router,telephone,docsis,station-only,c-vlan-bridge,s-vlan-bridge,two-port-mac-relay"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeLLDPCapabilities(c.raw)
			if got != c.want {
				t.Errorf("decodeLLDPCapabilities(%x) = %q; want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestDecodeCDPCapabilities checks the CDP bitmap → shared label set
// translation. The collapse of CDP's "transparent-bridge" + "switch"
// into a single "bridge" label is intentional — the UI doesn't need
// to distinguish them.
func TestDecodeCDPCapabilities(t *testing.T) {
	cases := []struct {
		name string
		bits uint32
		want string
	}{
		{"router (bit 0)", 0x01, "router"},
		{"transparent-bridge collapses to bridge (bit 1)", 0x02, "bridge"},
		{"switch collapses to bridge (bit 3)", 0x08, "bridge"},
		{"both bridge bits → single label", 0x0A, "bridge"},
		{"wlan-ap (bit 9)", 0x200, "wlan-ap"},
		{"router+host", 0x11, "router,host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeCDPCapabilities(c.bits)
			if got != c.want {
				t.Errorf("decodeCDPCapabilities(0x%x) = %q; want %q", c.bits, got, c.want)
			}
		})
	}
}

// TestSanitize is the regression net for vendor TLVs that carry
// embedded nulls / control chars. We accept ASCII printable + bytes
// ≥ 0x80 (UTF-8 multibyte) and drop everything else.
func TestSanitize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"normal string", "normal string"},
		{"with\x00null", "withnull"},
		{"with\r\nctrl", "withctrl"},
		{"  trim me  ", "trim me"},
		{"", ""},
		{"utf-8 café", "utf-8 café"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := sanitize(c.in)
			if got != c.want {
				t.Errorf("sanitize(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

// TestFormatMgmtAddr / TestFormatCDPAddr verify the management
// address decoders. Anything outside the v4/v6 boundary returns the
// empty string — operators see a missing-address state instead of
// garbage on the topology hover card.
func TestFormatMgmtAddr(t *testing.T) {
	if got := formatMgmtAddr(1, []byte{8, 8, 8, 8}); got != "8.8.8.8" {
		t.Errorf("ipv4: got %q", got)
	}
	if got := formatMgmtAddr(1, []byte{8, 8, 8}); got != "" {
		t.Errorf("ipv4 short length: got %q", got)
	}
	if got := formatMgmtAddr(2, make([]byte, 16)); got != "::" {
		t.Errorf("ipv6: got %q", got)
	}
	if got := formatMgmtAddr(16, []byte{1, 2}); got != "" {
		t.Errorf("unknown af: got %q", got)
	}
}

func TestFormatCDPAddr(t *testing.T) {
	if got := formatCDPAddr(1, []byte{10, 0, 0, 1}); got != "10.0.0.1" {
		t.Errorf("ipv4: got %q", got)
	}
	if got := formatCDPAddr(2, []byte{10, 0, 0, 1}); got != "" {
		t.Errorf("non-IP type: got %q", got)
	}
}

// TestMockWalkNeighbors locks in the mock-client neighbor shape so
// the dev loop renders something stable. Two LLDP rows + one CDP row
// per device; deterministic chassis IDs keyed off seed.
func TestMockWalkNeighbors(t *testing.T) {
	m := NewMock()
	ifTable := map[uint32]string{2: "Te1/0/1", 3: "Te1/0/2", 4: "Te1/0/3"}
	got, err := m.WalkNeighbors(t.Context(), "10.0.0.42", ifTable)
	if err != nil {
		t.Fatalf("mock walk: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 neighbors, got %d", len(got))
	}
	lldpCount, cdpCount := 0, 0
	for _, n := range got {
		switch n.DiscoveryProto {
		case "lldp":
			lldpCount++
		case "cdp":
			cdpCount++
		}
		if n.LocalPortName == "" {
			t.Errorf("local port name not resolved for ifindex %d", n.LocalIfIndex)
		}
		if n.RemoteChassisID == "" {
			t.Errorf("neighbor missing chassis id: %#v", n)
		}
	}
	if lldpCount != 2 || cdpCount != 1 {
		t.Errorf("want 2 lldp + 1 cdp, got %d/%d", lldpCount, cdpCount)
	}
	// First LLDP row's management addr is always the synthetic peer.
	if !strings.HasPrefix(got[0].RemoteManagementAddr, "10.0.0.") {
		t.Errorf("first neighbor mgmt addr expected 10.0.0.x, got %q", got[0].RemoteManagementAddr)
	}
}
