package snmpx

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// BGPPeer is one entry on the bgp_peers table. The walker produces
// these from RFC 4273 bgpPeerTable; cbgpPeer3Table / jnxBgpM2 walks
// produce the same shape with the Source field set accordingly so
// the read path can prefer the richer vendor MIB when present.
//
// Pre-cbgp / pre-jnxbgp BGP4-MIB only exposes IPv4 peers and only
// the global routing table (VRF = "default"); the walker emits
// PeerAddr in v4 dotted form for those. Storage code converts to
// v4-mapped IPv6 for the IPv6 column. cbgpPeer3 / jnxBgpM2 walkers
// populate VRF from the index so multi-VRF devices show peers
// grouped per routing instance.
type BGPPeer struct {
	PolledAt        time.Time
	Exporter        string
	VRF             string // 'default' for global table; vendor MIB walks fill in non-default VRF names
	PeerAddr        string // dotted v4 or v6 string
	PeerASN         uint32
	LocalASN        uint32
	State           string // 'idle' | 'connect' | 'active' | 'opensent' | 'openconfirm' | 'established' | 'unknown'
	AdminStatus     string // 'start' | 'stop' | ''
	EstablishedAt   time.Time // zero when device didn't supply or peer not Established
	LastChangeAt    time.Time // zero when device didn't supply
	AFI             string // 'ipv4' | 'ipv6' | ''
	SAFI            string // 'unicast' | 'multicast' | 'mpls-vpn' | ''
	PeerDescription string
	Source          string // 'bgp4' | 'cbgp' | 'jnxbgp'
}

// VRFDefault is the canonical name for the global / "no VRF" routing
// table. Used both as the BGP4-MIB walker's hardcoded VRF (since
// that MIB has no VRF concept) and as the default column value in
// the bgp_peers schema.
const VRFDefault = "default"

// walkBGPPeers fetches the RFC 4273 bgpPeerTable rows. Returns an
// empty slice (no error) on devices that don't implement BGP4-MIB —
// most CPEs and L2 switches won't.
//
// The walker is deliberately minimal in phase 2: BGP4-MIB only,
// IPv4-only peers, 16-bit ASN. Vendor extensions (cbgpPeer2 for
// IPv6 + 32-bit ASN, jnxBgpM2) are additive in a follow-up — the
// table schema and the BGPPeer struct are already shaped for them.
func walkBGPPeers(g *gosnmp.GoSNMP, exporter string) ([]BGPPeer, error) {
	type entry struct {
		state          int
		adminStatus    int
		remoteAS       int
		localAS        uint32
		fsmEstablished uint32 // seconds since last established (Counter32)
		inUpdateAge    uint32
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

	// Index suffix for bgpPeerTable rows is the peer IPv4 address
	// (4 octets, dotted). gosnmp prepends a leading dot; tolerate both.
	parseSuffix := func(name, base string) (suffix string, ok bool) {
		n := strings.TrimPrefix(name, ".")
		if !strings.HasPrefix(n, base+".") {
			return "", false
		}
		return strings.TrimPrefix(n, base+"."), true
	}

	walk := func(oid string, fn func(suffix string, pdu gosnmp.SnmpPDU)) {
		_ = g.BulkWalk(oid, func(pdu gosnmp.SnmpPDU) error {
			suffix, ok := parseSuffix(pdu.Name, oid)
			if !ok {
				return nil
			}
			fn(suffix, pdu)
			return nil
		})
	}

	walk(OIDBgpPeerState, func(suffix string, pdu gosnmp.SnmpPDU) {
		ensure(suffix).state = integerValue(pdu)
	})
	if len(by) == 0 {
		// Device doesn't speak BGP4-MIB. Return early so we don't waste
		// round-trips on the rest of the columns.
		return nil, nil
	}
	walk(OIDBgpPeerAdminStatus, func(suffix string, pdu gosnmp.SnmpPDU) {
		ensure(suffix).adminStatus = integerValue(pdu)
	})
	walk(OIDBgpPeerRemoteAS, func(suffix string, pdu gosnmp.SnmpPDU) {
		ensure(suffix).remoteAS = integerValue(pdu)
	})
	walk(OIDBgpPeerFsmEstablishedTime, func(suffix string, pdu gosnmp.SnmpPDU) {
		ensure(suffix).fsmEstablished = uint32Value(pdu)
	})
	walk(OIDBgpPeerInUpdateElapsedTime, func(suffix string, pdu gosnmp.SnmpPDU) {
		ensure(suffix).inUpdateAge = uint32Value(pdu)
	})

	// bgpLocalAs is a scalar; one Get is enough. Tolerate the failure
	// path silently — operators on devices that don't expose it still
	// get peer state, just not the local-ASN column.
	var localAS uint32
	if pkt, err := g.Get([]string{OIDBgpPeerLocalAS + ".0"}); err == nil && len(pkt.Variables) == 1 {
		localAS = uint32Value(pkt.Variables[0])
	}

	now := time.Now().UTC()
	out := make([]BGPPeer, 0, len(by))
	for suffix, e := range by {
		peerIP := net.ParseIP(suffix)
		if peerIP == nil {
			continue
		}
		var establishedAt time.Time
		if e.state == 6 && e.fsmEstablished > 0 {
			establishedAt = now.Add(-time.Duration(e.fsmEstablished) * time.Second)
		}
		var lastChangeAt time.Time
		if e.inUpdateAge > 0 {
			lastChangeAt = now.Add(-time.Duration(e.inUpdateAge) * time.Second)
		}
		out = append(out, BGPPeer{
			PolledAt:      now,
			Exporter:      exporter,
			VRF:           VRFDefault, // BGP4-MIB has no VRF concept — global table only
			PeerAddr:      peerIP.String(),
			PeerASN:       uint32(e.remoteAS),
			LocalASN:      localAS,
			State:         BgpPeerStateName(e.state),
			AdminStatus:   BgpAdminStatusName(e.adminStatus),
			EstablishedAt: establishedAt,
			LastChangeAt:  lastChangeAt,
			AFI:           "ipv4", // BGP4-MIB is IPv4-only
			SAFI:          "unicast",
			Source:        "bgp4",
		})
	}
	return out, nil
}

// uint32Value extracts a Counter32 / Unsigned32 value from a PDU.
// Falls back to integerValue's int truncation for INTEGER PDUs that
// gosnmp returned via the type system without coercing.
func uint32Value(pdu gosnmp.SnmpPDU) uint32 {
	switch v := pdu.Value.(type) {
	case uint:
		return uint32(v)
	case uint32:
		return v
	case uint64:
		return uint32(v)
	case int:
		if v < 0 {
			return 0
		}
		return uint32(v)
	case int32:
		if v < 0 {
			return 0
		}
		return uint32(v)
	case int64:
		if v < 0 {
			return 0
		}
		return uint32(v)
	}
	return 0
}

// WalkBGP is the Client-interface entry point: returns the BGP peers
// the device exposes, or an empty slice if it doesn't speak BGP at
// all. Logging the failure mode is the caller's job (so operators
// can correlate with scheduler metrics).
//
// Resolution order:
//
//  1. ARISTA-BGP4V2-MIB (Arista EOS). Returns peers across every
//     routing instance the device exposes. Renders instance==1 as
//     "default" and others as "vrf-<N>" (the MIB does not carry a
//     VRF-name lookup).
//  2. cbgpPeer3Table (Cisco VRF-aware) — TODO stub.
//  3. jnxBgpM2PeerTable (Junos VRF-aware) — TODO stub.
//  4. RFC 4273 BGP4-MIB. Global routing table only (vrf="default"),
//     IPv4 peers only, 16-bit ASN. Always works on standards-
//     compliant gear that runs BGP.
//
// The first walker that returns non-empty wins. The chain runs
// vendor-richest first so the same Cisco / Arista device doesn't
// produce both global-only BGP4-MIB rows and richer vendor rows
// (which would dupe under (exporter, peer_addr) at read time).
func (rc *RealClient) WalkBGP(ctx context.Context, target string) ([]BGPPeer, error) {
	g, err := buildGoSNMP(target, rc.cfg, ctx)
	if err != nil {
		return nil, fmt.Errorf("snmpx: bgp dial %s: %w", target, err)
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmpx: bgp connect %s: %w", target, err)
	}
	defer g.Conn.Close()
	if peers := walkAristaBGPV2(g, target); len(peers) > 0 {
		return peers, nil
	}
	if peers := walkCiscoBGPPeer3(g, target); len(peers) > 0 {
		return peers, nil
	}
	if peers := walkJuniperBGPM2(g, target); len(peers) > 0 {
		return peers, nil
	}
	return walkBGPPeers(g, target)
}

// walkAristaBGPV2 walks aristaBgp4V2PeerTable + aristaBgp4V2PeerEventTimesTable.
// Index suffix is `<instance>.<addrType>.<addrLen>.<addrBytes...>`
// (Unsigned32 instance + InetAddressType + length-prefixed
// InetAddress). Devices that don't run BGP, or run it without
// exposing the Arista MIB, return zero rows; the caller then falls
// through to BGP4-MIB.
//
// Per the MIB, aristaBgp4V2PeerInstance is "1 for single-instance
// impls"; vendors number additional VRFs sequentially. The MIB
// itself doesn't expose names, so we render instance 1 as "default"
// and others as "vrf-<N>". A friendly-name mapping (instance →
// operator-supplied VRF label) is a later UX layer.
func walkAristaBGPV2(g *gosnmp.GoSNMP, target string) []BGPPeer {
	type entry struct {
		instance       uint32
		addrType       int
		peerAddr       string
		state          int
		adminStatus    int
		remoteAS       uint32
		localAS        uint32
		descr          string
		fsmEstablished uint32
		inUpdateAge    uint32
	}
	by := map[string]*entry{}

	parseSuffix := func(name, base string) (string, bool) {
		n := strings.TrimPrefix(name, ".")
		if !strings.HasPrefix(n, base+".") {
			return "", false
		}
		return strings.TrimPrefix(n, base+"."), true
	}

	walk := func(oid string, fn func(suffix string, pdu gosnmp.SnmpPDU)) {
		_ = g.BulkWalk(oid, func(pdu gosnmp.SnmpPDU) error {
			suffix, ok := parseSuffix(pdu.Name, oid)
			if !ok {
				return nil
			}
			fn(suffix, pdu)
			return nil
		})
	}

	ensure := func(suffix string) *entry {
		if e, ok := by[suffix]; ok {
			return e
		}
		instance, addrType, addr, parsed := parseAristaBgpV2Index(suffix)
		if !parsed {
			return nil
		}
		e := &entry{instance: instance, addrType: addrType, peerAddr: addr}
		by[suffix] = e
		return e
	}

	walk(OIDAristaBgp4V2PeerState, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.state = integerValue(pdu)
		}
	})
	if len(by) == 0 {
		return nil
	}
	walk(OIDAristaBgp4V2PeerAdminStatus, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.adminStatus = integerValue(pdu)
		}
	})
	walk(OIDAristaBgp4V2PeerRemoteAs, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.remoteAS = uint32Value(pdu)
		}
	})
	walk(OIDAristaBgp4V2PeerLocalAs, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.localAS = uint32Value(pdu)
		}
	})
	walk(OIDAristaBgp4V2PeerDescription, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.descr = octetString(pdu)
		}
	})
	walk(OIDAristaBgp4V2PeerFsmEstTime, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.fsmEstablished = uint32Value(pdu)
		}
	})
	walk(OIDAristaBgp4V2PeerInUpdElapsed, func(suffix string, pdu gosnmp.SnmpPDU) {
		if e := ensure(suffix); e != nil {
			e.inUpdateAge = uint32Value(pdu)
		}
	})

	now := time.Now().UTC()
	out := make([]BGPPeer, 0, len(by))
	for _, e := range by {
		var establishedAt time.Time
		if e.state == 6 && e.fsmEstablished > 0 {
			establishedAt = now.Add(-time.Duration(e.fsmEstablished) * time.Second)
		}
		var lastChangeAt time.Time
		if e.inUpdateAge > 0 {
			lastChangeAt = now.Add(-time.Duration(e.inUpdateAge) * time.Second)
		}
		afi := "ipv4"
		if e.addrType == 2 {
			afi = "ipv6"
		}
		out = append(out, BGPPeer{
			PolledAt:        now,
			Exporter:        target,
			VRF:             aristaInstanceToVRF(e.instance),
			PeerAddr:        e.peerAddr,
			PeerASN:         e.remoteAS,
			LocalASN:        e.localAS,
			State:           BgpPeerStateName(e.state),
			AdminStatus:     BgpAdminStatusName(e.adminStatus),
			EstablishedAt:   establishedAt,
			LastChangeAt:    lastChangeAt,
			AFI:             afi,
			SAFI:            "unicast",
			PeerDescription: e.descr,
			Source:          "aristabgp",
		})
	}
	return out
}

// parseAristaBgpV2Index parses an aristaBgp4V2PeerEntry suffix
// (instance.addrType.addrLen.addrBytes...). Returns instance,
// InetAddressType (1=ipv4, 2=ipv6), and the peer address as a
// dotted-quad / canonical-IPv6 string.
func parseAristaBgpV2Index(suffix string) (instance uint32, addrType int, addr string, ok bool) {
	parts := strings.Split(suffix, ".")
	if len(parts) < 3 {
		return 0, 0, "", false
	}
	inst, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, "", false
	}
	at, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, "", false
	}
	alen, err := strconv.Atoi(parts[2])
	if err != nil || alen < 0 {
		return 0, 0, "", false
	}
	if len(parts) < 3+alen {
		return 0, 0, "", false
	}
	addrBytes := make([]byte, alen)
	for i := 0; i < alen; i++ {
		b, err := strconv.Atoi(parts[3+i])
		if err != nil || b < 0 || b > 255 {
			return 0, 0, "", false
		}
		addrBytes[i] = byte(b)
	}
	switch at {
	case 1: // ipv4
		if alen != 4 {
			return 0, 0, "", false
		}
		addr = net.IP(addrBytes).String()
	case 2: // ipv6
		if alen != 16 {
			return 0, 0, "", false
		}
		addr = net.IP(addrBytes).String()
	default:
		return 0, 0, "", false
	}
	return uint32(inst), at, addr, true
}

// aristaInstanceToVRF renders a numeric peer instance as a VRF
// string. Instance 1 is the global table per the MIB convention;
// others are formatted as "vrf-<N>" until an operator-supplied
// instance→name mapping ships.
func aristaInstanceToVRF(instance uint32) string {
	if instance == 0 || instance == 1 {
		return VRFDefault
	}
	return fmt.Sprintf("vrf-%d", instance)
}

// walkCiscoBGPPeer3 walks cbgpPeer3Table from CISCO-BGP4-MIB (the
// VRF-aware extension). The OID index is
// (vrfName.addrType.addrLen.addr...) so a non-trivial parser is
// needed to extract the VRF name (variable-length SnmpAdminString)
// and InetAddress encoding.
//
// TODO(phase 2.5b): implement the real walker. For now this returns
// nil so WalkBGP falls through to BGP4-MIB. Tracked in the design
// doc Phase 2 follow-up section. The mock client emits multi-VRF
// peers so the UI / alert template / scope filter are exercisable
// without this real walker landing first.
func walkCiscoBGPPeer3(g *gosnmp.GoSNMP, target string) []BGPPeer {
	_ = g
	_ = target
	return nil
}

// walkJuniperBGPM2 walks jnxBgpM2PeerTable. jnxBgpM2PeerRoutingInstance
// carries the VRF name. Same status as walkCiscoBGPPeer3 — TODO for a
// follow-up PR. Returning nil keeps WalkBGP's resolution chain honest:
// fall through to BGP4-MIB on devices we haven't taught yet.
func walkJuniperBGPM2(g *gosnmp.GoSNMP, target string) []BGPPeer {
	_ = g
	_ = target
	return nil
}
