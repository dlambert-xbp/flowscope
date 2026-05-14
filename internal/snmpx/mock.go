package snmpx

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

// MockClient implements Client without any network IO. It returns
// deterministic synthetic inventory keyed by exporter address so the
// dev loop has plausible data to render in the Devices tab.
//
// VISION.md §4.2 explicitly calls out the mock client — production
// SNMP failures are easy to chase, but reproducing a specific lab
// without one is tedious, so the mock ships in main.
type MockClient struct{}

// NewMock returns a mock SNMP client ready to use.
func NewMock() *MockClient { return &MockClient{} }

// Walk returns a synthetic inventory shaped so successive snapshots
// of the same exporter look like a stable device.
func (m *MockClient) Walk(_ context.Context, target string) (*Inventory, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(target))
	seed := h.Sum64()

	models := []struct {
		descr  string
		oid    string
		vendor string
	}{
		{"Cisco IOS Software, C9300 Software (cat9k_iosxe), Version 17.9.4", "1.3.6.1.4.1.9.1.2370", "Cisco"},
		{"Cisco NX-OS Software, n9000-EOR Version 10.3(3)", "1.3.6.1.4.1.9.12.3.1.3.1971", "Cisco"},
		{"Arista Networks EOS version 4.31.0F running on Arista DCS-7280SR", "1.3.6.1.4.1.30065.1.3000.7280", "Arista"},
		{"Juniper JUNOS 23.4R1.6 running on MX204", "1.3.6.1.4.1.2636.1.1.1.2.94", "Juniper"},
	}
	model := models[seed%uint64(len(models))]

	hostname := fmt.Sprintf("mock-%s-%02d", lowerName(model.vendor), seed%100)
	location := []string{"DC-East · Rack 14", "DC-East · Rack 22", "DC-West · Rack 7", "Branch-NA · IDF-3"}[seed%4]
	contact := "noc@flowscope.lab"

	// Deterministic uptime: 3–137 days.
	uptimeDays := 3 + (seed % 135)
	uptimeMs := uptimeDays * 24 * 3600 * 1000

	// Generate 8 mock interfaces. Mix up/down to exercise UI states.
	ifaces := make([]Interface, 0, 8)
	for i := uint32(1); i <= 8; i++ {
		oper := "up"
		admin := "up"
		if i == 6 {
			admin = "down"
			oper = "down"
		}
		if i == 7 {
			oper = "down" // failed link, still admin up
		}
		ifaces = append(ifaces, Interface{
			IfIndex:     i + 1, // start at 2 to align with synth-sFlow
			IfDescr:     fmt.Sprintf("Te1/0/%d", i),
			IfAlias:     mockAliasFor(i, model.vendor),
			IfType:      6, // ethernetCsmacd
			IfSpeedBps:  10_000_000_000,
			IfMtu:       9216,
			AdminStatus: admin,
			OperStatus:  oper,
			InErrors:    seed%5 + uint64(i)*3,
			OutErrors:   seed%4 + uint64(i)*2,
			InDiscards:  seed%6 + uint64(i),
			OutDiscards: seed%3 + uint64(i)/2,
		})
	}

	return &Inventory{
		PolledAt:       time.Now().UTC(),
		Exporter:       target,
		SysDescr:       model.descr,
		SysObjectID:    model.oid,
		SysUpTimeMs:    uptimeMs,
		SysName:        hostname,
		SysContact:     contact,
		SysLocation:    location,
		Interfaces:     ifaces,
		Resources:      mockResources(seed, model.vendor),
		PollDurationMs: 12,
		Status:         "ok",
		SNMPVersion:    "v2c",
	}, nil
}

// mockResources synthesises a believable CPU/memory snapshot keyed on
// the per-exporter seed. CPU drifts slowly via time so successive
// walks produce slightly different readings (the Summary sparkline
// in the UI has something to draw); memory is stable so the "X GiB
// of Y GiB" tile reads like a real device. Vendor branches keep
// Cisco-only or Juniper-only behaviours from leaking into mocks for
// the wrong platform.
func mockResources(seed uint64, vendor string) []ResourceSample {
	// Slow drift component so dashboards animate during local dev.
	tick := uint64(time.Now().Unix()) / 30 // step every 30s
	out := make([]ResourceSample, 0, 6)

	// CPU — one or two cores depending on vendor flavour.
	cpuCount := uint32(2)
	if vendor == "Cisco" || vendor == "Juniper" {
		cpuCount = 1
	}
	cpuSource := ResourceSourceHRMIB
	if vendor == "Cisco" {
		cpuSource = ResourceSourceCiscoProcess
	}
	for i := uint32(1); i <= cpuCount; i++ {
		base := float32(15 + (seed+tick+uint64(i)*13)%55)
		out = append(out, ResourceSample{
			Kind:         ResourceKindCPU,
			Component:    fmt.Sprintf("Processor %d", i),
			ValuePercent: base,
			Source:       cpuSource,
		})
	}

	// Memory — one pool per vendor flavour. Total varies by model so
	// the headline tile reads plausibly: 4 GiB on a switch, 16 GiB on
	// a router-class device.
	memTotal := uint64(4) << 30
	memSource := ResourceSourceHRMIB
	memComponent := "Physical Memory"
	if vendor == "Cisco" {
		memTotal = uint64(2) << 30
		memSource = ResourceSourceCiscoMempool
		memComponent = "Pool: Processor"
	}
	if vendor == "Juniper" {
		memTotal = uint64(16) << 30
	}
	memUsed := uint64(float64(memTotal) * (0.35 + float64(seed%30)/100))
	memPct := float32(float64(memUsed) / float64(memTotal) * 100)
	out = append(out, ResourceSample{
		Kind:         ResourceKindMemory,
		Component:    memComponent,
		ValuePercent: memPct,
		ValueBytes:   memUsed,
		MaxBytes:     memTotal,
		Source:       memSource,
	})

	// Storage — bootflash / system disk so the UI has something in the
	// "storage" row. HRMIB-derived; Cisco MIBs don't carry this in V1.
	flashTotal := uint64(1) << 30
	flashUsed := uint64(float64(flashTotal) * (0.45 + float64(seed%20)/100))
	out = append(out, ResourceSample{
		Kind:         ResourceKindStorage,
		Component:    "bootflash:",
		ValuePercent: float32(float64(flashUsed) / float64(flashTotal) * 100),
		ValueBytes:   flashUsed,
		MaxBytes:     flashTotal,
		Source:       ResourceSourceHRMIB,
	})

	// Temperature — two sensors, "Inlet" + "Hotspot", drifting around
	// a believable steady state. Hotspot runs ~15°C above inlet so a
	// real device's relationship reads correctly on the dashboard.
	inlet := 22 + float64((seed+tick*3)%8)
	hotspot := inlet + 12 + float64((seed+tick*5)%6)
	out = append(out, ResourceSample{
		Kind:         ResourceKindTemperature,
		Component:    "Inlet",
		ValueNumeric: inlet,
		Unit:         "C",
		Source:       ResourceSourceEntitySensor,
	})
	out = append(out, ResourceSample{
		Kind:         ResourceKindTemperature,
		Component:    "Hotspot",
		ValueNumeric: hotspot,
		Unit:         "C",
		Source:       ResourceSourceEntitySensor,
	})

	// Fans — two fan trays at slightly different RPM so the dashboard
	// shows useful row variance.
	fanA := 5800 + float64((seed+tick*7)%600)
	fanB := 5800 + float64((seed+tick*11)%600)
	out = append(out, ResourceSample{
		Kind:         ResourceKindFan,
		Component:    "Fan tray 1",
		ValueNumeric: fanA,
		Unit:         "rpm",
		Source:       ResourceSourceEntitySensor,
	})
	out = append(out, ResourceSample{
		Kind:         ResourceKindFan,
		Component:    "Fan tray 2",
		ValueNumeric: fanB,
		Unit:         "rpm",
		Source:       ResourceSourceEntitySensor,
	})

	// Power — two PSUs, PSU2 simulating a fault one device in eight so
	// the operator sees an example of a crit-red tile in dev.
	psu1Status := float64(2) // 2 = on (healthy)
	psu2Status := float64(2)
	psu2Pct := float32(0)
	if seed%8 == 0 {
		psu2Status = 8 // 8 = failed
		psu2Pct = 100
	}
	out = append(out, ResourceSample{
		Kind:         ResourceKindPower,
		Component:    "PSU 1",
		ValueNumeric: psu1Status,
		Unit:         "state",
		Source:       ResourceSourceCiscoFRU,
	})
	out = append(out, ResourceSample{
		Kind:         ResourceKindPower,
		Component:    "PSU 2",
		ValuePercent: psu2Pct,
		ValueNumeric: psu2Status,
		Unit:         "state",
		Source:       ResourceSourceCiscoFRU,
	})

	return out
}

func mockAliasFor(i uint32, vendor string) string {
	switch i {
	case 1:
		return "uplink → core-edge-02"
	case 2:
		return "access · vlan 40"
	case 3:
		return "access · vlan 40"
	case 4:
		return "access · vlan 50 · server farm"
	case 5:
		return "access · vlan 30"
	case 6:
		return ""
	case 7:
		return "backup uplink → core-edge-04"
	default:
		return fmt.Sprintf("%s ethernet", vendor)
	}
}

// WalkNeighbors returns synthetic LLDP/CDP neighbors for the mock
// target. The graph forms a small spine-leaf: every mock device
// reports two neighbors keyed off its seed, producing a deterministic
// adjacency that's stable across reloads so the Devices → Neighbors
// tab has something interesting to render in the dev loop.
//
// Mock neighbors are intentionally a mix of "known" remotes (IPs the
// scheduler would also walk in this dev environment) and "unknown"
// remotes (chassis IDs that don't map back to any walked exporter)
// so the topology UI exercises both code paths.
func (m *MockClient) WalkNeighbors(_ context.Context, target string, ifTable map[uint32]string) ([]Neighbor, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(target))
	seed := h.Sum64()
	// Two upstream neighbors keyed off the seed. The remote chassis
	// MAC is deterministic per (target, port) so repeated walks
	// produce the exact same edges — the ReplacingMergeTree dedupes
	// trivially.
	mkMAC := func(salt uint64) string {
		v := seed ^ salt
		return fmt.Sprintf("aa:bb:%02x:%02x:%02x:%02x",
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	// Pick a deterministic "known peer" address so two mock devices
	// out of every batch link to each other. Mod the seed onto the
	// 10.0.0.0/24 range the dev compose stack typically uses; this
	// is a synthetic value and never resolved against a real network.
	knownPeer := fmt.Sprintf("10.0.0.%d", 10+(seed%8))
	out := []Neighbor{
		{
			DiscoveryProto:       "lldp",
			LocalIfIndex:         2,
			LocalPortName:        ifTable[2],
			RemoteChassisID:      mkMAC(0x55a5),
			RemoteSysName:        fmt.Sprintf("mock-core-%02d", seed%10),
			RemoteSysDesc:        "Cisco IOS Software, C9500 Software (cat9k_iosxe)",
			RemotePortID:         "Te1/0/24",
			RemoteCapabilities:   "bridge,router",
			RemoteManagementAddr: knownPeer,
		},
		{
			DiscoveryProto:     "lldp",
			LocalIfIndex:       3,
			LocalPortName:      ifTable[3],
			RemoteChassisID:    mkMAC(0xa5a5),
			RemoteSysName:      fmt.Sprintf("mock-spine-%02d", (seed/3)%10),
			RemoteSysDesc:      "Arista DCS-7050X3 / EOS 4.31.0F",
			RemotePortID:       "Ethernet1/1",
			RemoteCapabilities: "bridge",
		},
		{
			DiscoveryProto:     "cdp",
			LocalIfIndex:       4,
			LocalPortName:      ifTable[4],
			RemoteChassisID:    fmt.Sprintf("mock-ap-%02d", seed%32),
			RemoteSysName:      fmt.Sprintf("mock-ap-%02d", seed%32),
			RemoteSysDesc:      "Cisco AIR-CAP3702E-A-K9",
			RemotePortID:       "GigabitEthernet0",
			RemoteCapabilities: "wlan-ap",
		},
	}
	return out, nil
}

// WalkBGP returns synthetic BGP peers for the mock target. Three
// VRFs so the Devices BGP panel renders a non-trivial grouped view
// in the dev loop:
//
//   - default       — global routing table, mixed health (the
//                     established eBGP transit + an idle iBGP peer)
//   - mgmt          — out-of-band management instance (one peer)
//   - CUSTOMER-A    — example MPLS L3VPN PE-CE session (one peer)
//
// The Idle iBGP peer in 'default' continues to drive the
// BGPNeighborDown alert in the dev loop; the additional VRFs
// exercise the per-VRF UI grouping and the BGPNeighborDown VRF
// scope filter.
func (m *MockClient) WalkBGP(_ context.Context, target string) ([]BGPPeer, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(target))
	seed := h.Sum64()
	now := time.Now().UTC()
	return []BGPPeer{
		{
			PolledAt:        now,
			Exporter:        target,
			VRF:             VRFDefault,
			PeerAddr:        fmt.Sprintf("192.0.2.%d", 1+seed%200),
			PeerASN:         65000 + uint32(seed%100),
			LocalASN:        64512,
			State:           "established",
			AdminStatus:     "start",
			EstablishedAt:   now.Add(-2 * time.Hour),
			LastChangeAt:    now.Add(-30 * time.Second),
			AFI:             "ipv4",
			SAFI:            "unicast",
			PeerDescription: "Mock transit eBGP",
			Source:          "bgp4",
		},
		{
			PolledAt:        now,
			Exporter:        target,
			VRF:             VRFDefault,
			PeerAddr:        fmt.Sprintf("198.51.100.%d", 1+seed%200),
			PeerASN:         64512,
			LocalASN:        64512,
			State:           "idle",
			AdminStatus:     "start",
			AFI:             "ipv4",
			SAFI:            "unicast",
			PeerDescription: "Mock iBGP (idle)",
			Source:          "bgp4",
		},
		{
			PolledAt:        now,
			Exporter:        target,
			VRF:             "mgmt",
			PeerAddr:        fmt.Sprintf("172.16.0.%d", 1+seed%200),
			PeerASN:         64512,
			LocalASN:        64512,
			State:           "established",
			AdminStatus:     "start",
			EstablishedAt:   now.Add(-6 * time.Hour),
			LastChangeAt:    now.Add(-2 * time.Minute),
			AFI:             "ipv4",
			SAFI:            "unicast",
			PeerDescription: "Mock OOB iBGP (mgmt VRF)",
			Source:          "cbgp",
		},
		{
			PolledAt:        now,
			Exporter:        target,
			VRF:             "CUSTOMER-A",
			PeerAddr:        fmt.Sprintf("10.200.%d.1", 1+seed%200),
			PeerASN:         65100 + uint32(seed%50),
			LocalASN:        64512,
			State:           "established",
			AdminStatus:     "start",
			EstablishedAt:   now.Add(-72 * time.Hour),
			LastChangeAt:    now.Add(-15 * time.Minute),
			AFI:             "ipv4",
			SAFI:            "mpls-vpn",
			PeerDescription: "Mock L3VPN PE-CE (CUSTOMER-A)",
			Source:          "cbgp",
		},
	}, nil
}

func lowerName(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out[i] = c
	}
	return string(out)
}
