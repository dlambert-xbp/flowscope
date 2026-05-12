package snmpx

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Neighbor is one entry on the lldp_neighbors table. The walker
// produces these from either LLDP or CDP; downstream code (the
// scheduler's persistTopology + the topology API) treats them
// uniformly via DiscoveryProto.
//
// LocalPortName is best-effort: the walker fills it from the same
// inventory snapshot when ifTable was successfully walked first.
// It stays empty on devices that respond to LLDP/CDP but not
// IF-MIB (vanishingly rare on real gear, but possible in lab
// captures and certainly possible on the mock).
type Neighbor struct {
	DiscoveryProto       string
	LocalIfIndex         uint32
	LocalPortName        string
	RemoteChassisID      string
	RemoteSysName        string
	RemoteSysDesc        string
	RemotePortID         string
	RemoteCapabilities   string
	RemoteManagementAddr string // empty when the TLV didn't carry one
}

// walkLLDPNeighbors returns the LLDP-MIB lldpRemTable rows. portName
// resolves lldpRemLocalPortNum (which is the ifIndex) to a human
// name from the ifTable snapshot we already walked.
//
// The walker is deliberately forgiving: vendors disagree on which
// TLVs they populate, embedded nulls show up in sys_desc, and some
// devices return SNMP_NOSUCHOBJECT for tables they don't implement.
// We sanitise on the way out and surface partial rows rather than
// dropping the whole walk.
func walkLLDPNeighbors(g *gosnmp.GoSNMP, portName map[uint32]string) ([]Neighbor, error) {
	type entry struct {
		chassisSubtype int
		chassisRaw     []byte
		portSubtype    int
		portRaw        []byte
		portDesc       string
		sysName        string
		sysDesc        string
		capEnabled     []byte
		mgmtAddr       string
	}
	// key = "timeMark.localPort.remIndex"; the per-port walk yields one
	// entry per (timeMark, localPort, remIndex) triple. Multiple
	// neighbors on one port are common in shared-segment lab setups.
	by := map[string]*entry{}
	ensure := func(k string) *entry {
		if e, ok := by[k]; ok {
			return e
		}
		e := &entry{}
		by[k] = e
		return e
	}

	// LLDP indices are 3-part: (timeMark, localPort, remIndex). We
	// preserve the full suffix as the map key so the relative ordering
	// inside the table is stable across columns.
	parseSuffix := func(name, base string) (key string, localPort uint32, ok bool) {
		// gosnmp prepends a leading dot on the OID; tolerate both.
		n := strings.TrimPrefix(name, ".")
		if !strings.HasPrefix(n, base+".") {
			return "", 0, false
		}
		tail := strings.TrimPrefix(n, base+".")
		parts := strings.SplitN(tail, ".", 3)
		if len(parts) < 3 {
			return "", 0, false
		}
		var lp uint32
		if _, err := fmt.Sscanf(parts[1], "%d", &lp); err != nil {
			return "", 0, false
		}
		return tail, lp, true
	}

	// One column at a time. BulkWalk returns "no such object" as an
	// error on devices that don't implement LLDP at all — caller
	// treats that as a soft miss, not a hard failure.
	missCount := 0
	walk := func(oid string, fn func(suffix string, lp uint32, pdu gosnmp.SnmpPDU)) error {
		err := g.BulkWalk(oid, func(pdu gosnmp.SnmpPDU) error {
			suffix, lp, ok := parseSuffix(pdu.Name, oid)
			if !ok {
				return nil
			}
			fn(suffix, lp, pdu)
			return nil
		})
		if err != nil {
			missCount++
			// Soft fail: device probably doesn't speak LLDP.
			return nil
		}
		return nil
	}

	_ = walk(OIDLldpRemChassisIdSubtype, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).chassisSubtype = integerValue(pdu)
	})
	_ = walk(OIDLldpRemChassisID, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).chassisRaw = octetBytes(pdu)
	})
	_ = walk(OIDLldpRemPortIDSubtype, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).portSubtype = integerValue(pdu)
	})
	_ = walk(OIDLldpRemPortID, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).portRaw = octetBytes(pdu)
	})
	_ = walk(OIDLldpRemPortDesc, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).portDesc = sanitize(octetString(pdu))
	})
	_ = walk(OIDLldpRemSysName, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).sysName = sanitize(octetString(pdu))
	})
	_ = walk(OIDLldpRemSysDesc, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).sysDesc = sanitize(octetString(pdu))
	})
	_ = walk(OIDLldpRemSysCapEnabled, func(suffix string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(suffix).capEnabled = octetBytes(pdu)
	})

	// Management addresses are sparse and tabled separately; index
	// the suffix into the same map. The suffix shape is
	// "timeMark.localPort.remIndex.addrSubtype.addrLen.addr...". We
	// match the first three numeric parts back to the rest of the
	// row, then format the trailing address bytes per addrSubtype
	// (1=ipv4, 2=ipv6).
	_ = g.BulkWalk(OIDLldpRemManAddrIfSubtype, func(pdu gosnmp.SnmpPDU) error {
		n := strings.TrimPrefix(pdu.Name, ".")
		if !strings.HasPrefix(n, OIDLldpRemManAddrIfSubtype+".") {
			return nil
		}
		tail := strings.TrimPrefix(n, OIDLldpRemManAddrIfSubtype+".")
		parts := strings.Split(tail, ".")
		if len(parts) < 5 {
			return nil
		}
		// Rebuild the (timeMark, localPort, remIndex) key.
		key := parts[0] + "." + parts[1] + "." + parts[2]
		ent, ok := by[key]
		if !ok {
			return nil
		}
		// parts[3] = addrSubtype, parts[4] = addrLen, parts[5..] = bytes
		var subtype, length int
		if _, err := fmt.Sscanf(parts[3], "%d", &subtype); err != nil {
			return nil
		}
		if _, err := fmt.Sscanf(parts[4], "%d", &length); err != nil {
			return nil
		}
		if len(parts) < 5+length {
			return nil
		}
		raw := make([]byte, length)
		for i := 0; i < length; i++ {
			var b int
			if _, err := fmt.Sscanf(parts[5+i], "%d", &b); err != nil {
				return nil
			}
			raw[i] = byte(b)
		}
		ent.mgmtAddr = formatMgmtAddr(subtype, raw)
		return nil
	})

	out := make([]Neighbor, 0, len(by))
	for suffix, e := range by {
		// Suffix: timeMark.localPort.remIndex
		parts := strings.SplitN(suffix, ".", 3)
		if len(parts) < 3 {
			continue
		}
		var lp uint32
		if _, err := fmt.Sscanf(parts[1], "%d", &lp); err != nil {
			continue
		}
		// A row with no chassis ID at all is meaningless to persist.
		if len(e.chassisRaw) == 0 {
			continue
		}
		n := Neighbor{
			DiscoveryProto:       "lldp",
			LocalIfIndex:         lp,
			LocalPortName:        portName[lp],
			RemoteChassisID:      decodeChassisID(e.chassisSubtype, e.chassisRaw),
			RemoteSysName:        e.sysName,
			RemoteSysDesc:        e.sysDesc,
			RemotePortID:         decodePortID(e.portSubtype, e.portRaw, e.portDesc),
			RemoteCapabilities:   decodeLLDPCapabilities(e.capEnabled),
			RemoteManagementAddr: e.mgmtAddr,
		}
		out = append(out, n)
	}
	return out, nil
}

// walkCDPNeighbors returns the cdpCache table rows. CDP indices are
// 2-part: (cdpCacheIfIndex, cdpCacheDeviceIndex). cdpCacheIfIndex is
// the local ifIndex directly, so we skip the extra map lookup LLDP
// needs.
//
// Many devices don't speak CDP — Arista in default mode, every
// non-Cisco vendor. The walker treats SNMP_NOSUCHOBJECT as a clean
// "this device doesn't have CDP" signal and returns an empty slice.
func walkCDPNeighbors(g *gosnmp.GoSNMP, portName map[uint32]string) ([]Neighbor, error) {
	type entry struct {
		deviceID     string
		devicePort   string
		platform     string
		version      string
		capabilities []byte
		addrType     int
		addrRaw      []byte
	}
	by := map[string]*entry{}
	ensure := func(k string) *entry {
		if e, ok := by[k]; ok {
			return e
		}
		e := &entry{}
		by[k] = e
		return e
	}

	parseSuffix := func(name, base string) (key string, localIf uint32, ok bool) {
		n := strings.TrimPrefix(name, ".")
		if !strings.HasPrefix(n, base+".") {
			return "", 0, false
		}
		tail := strings.TrimPrefix(n, base+".")
		parts := strings.SplitN(tail, ".", 2)
		if len(parts) < 2 {
			return "", 0, false
		}
		var ifIdx uint32
		if _, err := fmt.Sscanf(parts[0], "%d", &ifIdx); err != nil {
			return "", 0, false
		}
		return tail, ifIdx, true
	}

	walk := func(oid string, fn func(suffix string, lp uint32, pdu gosnmp.SnmpPDU)) {
		_ = g.BulkWalk(oid, func(pdu gosnmp.SnmpPDU) error {
			suffix, lp, ok := parseSuffix(pdu.Name, oid)
			if !ok {
				return nil
			}
			fn(suffix, lp, pdu)
			return nil
		})
	}

	walk(OIDCdpCacheDeviceID, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).deviceID = sanitize(octetString(pdu))
	})
	walk(OIDCdpCacheDevicePort, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).devicePort = sanitize(octetString(pdu))
	})
	walk(OIDCdpCachePlatform, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).platform = sanitize(octetString(pdu))
	})
	walk(OIDCdpCacheVersion, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).version = sanitize(octetString(pdu))
	})
	walk(OIDCdpCacheCapabilities, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).capabilities = octetBytes(pdu)
	})
	walk(OIDCdpCacheAddressType, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).addrType = integerValue(pdu)
	})
	walk(OIDCdpCacheAddress, func(s string, lp uint32, pdu gosnmp.SnmpPDU) {
		_ = lp
		ensure(s).addrRaw = octetBytes(pdu)
	})

	out := make([]Neighbor, 0, len(by))
	for suffix, e := range by {
		parts := strings.SplitN(suffix, ".", 2)
		if len(parts) < 2 {
			continue
		}
		var lp uint32
		if _, err := fmt.Sscanf(parts[0], "%d", &lp); err != nil {
			continue
		}
		if e.deviceID == "" {
			continue
		}
		// CDP capabilities is a 4-byte big-endian bitmap. Some
		// devices return fewer bytes; pad on the left.
		var capBits uint32
		for _, b := range e.capabilities {
			capBits = (capBits << 8) | uint32(b)
		}
		// Platform is the closest CDP equivalent of LLDP's sys_desc;
		// version adds OS info when present, so concat keeps both
		// signals visible in the topology hover card.
		desc := e.platform
		if e.version != "" {
			if desc != "" {
				desc = desc + " · " + e.version
			} else {
				desc = e.version
			}
		}
		mgmt := ""
		if len(e.addrRaw) > 0 {
			mgmt = formatCDPAddr(e.addrType, e.addrRaw)
		}
		out = append(out, Neighbor{
			DiscoveryProto:       "cdp",
			LocalIfIndex:         lp,
			LocalPortName:        portName[lp],
			RemoteChassisID:      e.deviceID,
			RemoteSysName:        e.deviceID,
			RemoteSysDesc:        desc,
			RemotePortID:         e.devicePort,
			RemoteCapabilities:   decodeCDPCapabilities(capBits),
			RemoteManagementAddr: mgmt,
		})
	}
	return out, nil
}

// decodeChassisID maps lldpRemChassisIdSubtype + raw bytes into a
// printable canonical string. Subtypes 1/2/3/6/7 are already
// printable strings; subtype 4 is a 6-byte MAC; subtype 5 is a
// (1-byte address family) + N-byte network address.
//
// Unknown subtypes fall back to lowercase-hex so the row still
// carries a stable key — the alternative is dropping the neighbor
// which is worse for an operator trying to debug a topology gap.
func decodeChassisID(subtype int, raw []byte) string {
	switch subtype {
	case 4:
		if len(raw) == 6 {
			return formatMAC(raw)
		}
	case 5:
		if len(raw) >= 2 {
			af := int(raw[0])
			body := raw[1:]
			switch af {
			case 1: // IPv4
				if len(body) == 4 {
					return net.IP(body).String()
				}
			case 2: // IPv6
				if len(body) == 16 {
					return net.IP(body).String()
				}
			}
		}
	case 1, 2, 3, 6, 7:
		return sanitize(string(raw))
	}
	// Subtype 4 with non-MAC length, subtype 5 with weird address — and
	// any subtype we don't recognise — fall through to hex.
	return strings.ToLower(hex.EncodeToString(raw))
}

// decodePortID renders the remote port identifier. Subtypes 1/2/3/5/6/7
// are printable; subtype 4 is a network address; subtype 7 is "local"
// (e.g. "Te1/0/47"). When the row is empty but portDesc is populated
// (some vendors only set the desc), fall back to the desc so the UI
// still shows an interface label.
func decodePortID(subtype int, raw []byte, portDesc string) string {
	// Subtype meanings (RFC 7042 / IEEE 802.1AB §8.5.3.3):
	//   1=interfaceAlias, 2=portComponent, 3=macAddress, 4=networkAddress,
	//   5=interfaceName, 6=agentCircuitId, 7=local
	switch subtype {
	case 3:
		if len(raw) == 6 {
			return formatMAC(raw)
		}
	case 4:
		if len(raw) >= 2 {
			af := int(raw[0])
			body := raw[1:]
			switch af {
			case 1:
				if len(body) == 4 {
					return net.IP(body).String()
				}
			case 2:
				if len(body) == 16 {
					return net.IP(body).String()
				}
			}
		}
	}
	if s := sanitize(string(raw)); s != "" {
		return s
	}
	return portDesc
}

// decodeLLDPCapabilities turns the 2-byte lldpRemSysCapEnabled bitmap
// (RFC 4363, lldpV2 §11.5.6) into a stable lowercase CSV. Bit order
// matches the IETF/IEEE spec.
//
//	bit 0 = other
//	bit 1 = repeater
//	bit 2 = bridge (switch)
//	bit 3 = wlan-ap
//	bit 4 = router
//	bit 5 = telephone
//	bit 6 = docsis (cable device)
//	bit 7 = station-only
//	bit 8 = c-vlan-bridge
//	bit 9 = s-vlan-bridge
//	bit 10 = two-port-mac-relay
func decodeLLDPCapabilities(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var bits uint16
	for _, b := range raw {
		bits = (bits << 8) | uint16(b)
	}
	labels := []struct {
		bit  uint16
		name string
	}{
		{1 << 15, "other"},
		{1 << 14, "repeater"},
		{1 << 13, "bridge"},
		{1 << 12, "wlan-ap"},
		{1 << 11, "router"},
		{1 << 10, "telephone"},
		{1 << 9, "docsis"},
		{1 << 8, "station-only"},
		{1 << 7, "c-vlan-bridge"},
		{1 << 6, "s-vlan-bridge"},
		{1 << 5, "two-port-mac-relay"},
	}
	parts := make([]string, 0, 4)
	for _, l := range labels {
		if bits&l.bit != 0 {
			parts = append(parts, l.name)
		}
	}
	return strings.Join(parts, ",")
}

// decodeCDPCapabilities translates the 32-bit cdpCacheCapabilities
// bitmap into the same shared label set LLDP uses, so the API and
// UI can render either uniformly. Cisco's bitmap is denser:
//
//	bit 0 = router
//	bit 1 = transparent-bridge
//	bit 2 = source-route-bridge
//	bit 3 = switch
//	bit 4 = host
//	bit 5 = igmp-router
//	bit 6 = repeater
//	bit 7 = phone
//	bit 8 = remote-mgmt
//	bit 9 = wireless-ap (CAPWAP)
//	bit 10 = two-port-mac-relay
//	bit 11 = sta-only
//
// We collapse "transparent-bridge" + "switch" → "bridge" so the
// label set lines up with LLDP.
func decodeCDPCapabilities(bits uint32) string {
	labels := []struct {
		bit  uint32
		name string
	}{
		{1 << 0, "router"},
		{1 << 1, "bridge"},
		{1 << 2, "source-route-bridge"},
		{1 << 3, "bridge"},
		{1 << 4, "host"},
		{1 << 5, "igmp"},
		{1 << 6, "repeater"},
		{1 << 7, "telephone"},
		{1 << 8, "remote-mgmt"},
		{1 << 9, "wlan-ap"},
		{1 << 10, "two-port-mac-relay"},
		{1 << 11, "station-only"},
	}
	seen := map[string]bool{}
	parts := make([]string, 0, 4)
	for _, l := range labels {
		if bits&l.bit != 0 && !seen[l.name] {
			parts = append(parts, l.name)
			seen[l.name] = true
		}
	}
	return strings.Join(parts, ",")
}

// formatMAC formats 6 raw bytes as colon-separated lowercase hex.
// This is the canonical chassis-ID form anywhere FlowScope shows
// MACs to humans.
func formatMAC(raw []byte) string {
	if len(raw) != 6 {
		return strings.ToLower(hex.EncodeToString(raw))
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		raw[0], raw[1], raw[2], raw[3], raw[4], raw[5])
}

// formatMgmtAddr renders an LLDP management address: subtype 1=ipv4,
// 2=ipv6. Other subtypes (per IANA address family numbers — appletalk,
// decnet, etc.) return empty string; FlowScope ignores them.
func formatMgmtAddr(subtype int, raw []byte) string {
	switch subtype {
	case 1:
		if len(raw) == 4 {
			return net.IP(raw).String()
		}
	case 2:
		if len(raw) == 16 {
			return net.IP(raw).String()
		}
	}
	return ""
}

// formatCDPAddr renders a CDP management address. Type 1 is IP; we
// then infer v4 vs v6 from the byte length. Anything else is dropped
// — we don't surface DECnet management addresses on a 2026 topology
// dashboard.
func formatCDPAddr(addrType int, raw []byte) string {
	if addrType != 1 {
		return ""
	}
	switch len(raw) {
	case 4, 16:
		return net.IP(raw).String()
	}
	return ""
}

// sanitize strips control characters (incl. embedded nulls) from a
// vendor-supplied string. Some switches return trailing nulls in the
// TLV; others embed `\r\n` from a hostname's banner field. We keep
// printable ASCII + non-ASCII letters / digits, drop everything
// else.
func sanitize(s string) string {
	if s == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 {
			continue
		}
		// Allow printable ASCII (0x20..0x7e) plus any byte ≥ 0x80
		// (so UTF-8 multibyte sequences survive). Drop ASCII
		// control bytes.
		if c < 0x20 || c == 0x7f {
			continue
		}
		out = append(out, c)
	}
	return strings.TrimSpace(string(out))
}

// octetBytes returns the raw byte slice from a gosnmp PDU. Where
// octetString would coerce to UTF-8, this keeps binary MAC / chassis
// bytes intact.
func octetBytes(p gosnmp.SnmpPDU) []byte {
	switch v := p.Value.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

// ErrNoLLDP / ErrNoCDP are returned by the walker when the device
// answered SNMP but didn't return any neighbor rows. They aren't
// errors per se — they just let the caller increment the right
// counter and skip the persist step.
var (
	ErrNoLLDP = errors.New("snmpx: no LLDP table")
	ErrNoCDP  = errors.New("snmpx: no CDP table")
)

// WalkNeighbors fetches LLDP first, falls back to CDP for empty
// returns, and combines the two. Real devices typically have one or
// the other; a few Cisco devices have both. The dedup happens at the
// API layer (bidirectional edges A↔B collapse), not here — the
// walker writes everything it sees and lets the read path sort it
// out.
//
// Returns immediately with ctx.Err() if cancelled, so the scheduler's
// 30s walk timeout still applies cleanly.
func (rc *RealClient) WalkNeighbors(ctx context.Context, target string, ifTable map[uint32]string) ([]Neighbor, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	g, err := buildGoSNMP(target, rc.cfg, ctx)
	if err != nil {
		return nil, err
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s: %w", target, err)
	}
	defer g.Conn.Close()

	lldp, _ := walkLLDPNeighbors(g, ifTable)
	cdp, _ := walkCDPNeighbors(g, ifTable)
	out := append(lldp, cdp...)
	return out, nil
}

// WalkNeighbors is the Client interface extension. The MockClient
// implementation lives in mock.go.
type NeighborWalker interface {
	WalkNeighbors(ctx context.Context, target string, ifTable map[uint32]string) ([]Neighbor, error)
}

// neighborWalkDeadline is used by the scheduler to bound the
// neighbor walk; LLDP/CDP walks are usually <2s on real gear, but
// a slow switch could stall. 20s leaves room without colliding
// with the 30s walk budget.
const neighborWalkDeadline = 20 * time.Second
