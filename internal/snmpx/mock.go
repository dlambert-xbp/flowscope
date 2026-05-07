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
		PollDurationMs: 12,
		Status:         "ok",
	}, nil
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
