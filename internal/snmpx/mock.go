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
