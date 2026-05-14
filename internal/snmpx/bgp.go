package snmpx

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// BGPPeer is one entry on the bgp_peers table. The walker produces
// these from RFC 4273 bgpPeerTable; cbgpPeer2Table / jnxBgpM2 walks
// produce the same shape with the Source field set accordingly so
// the read path can prefer the richer vendor MIB when present.
//
// Pre-cbgp / pre-jnxbgp BGP4-MIB only exposes IPv4 peers; the walker
// emits PeerAddr in v4 dotted form for those. Storage code converts
// to v4-mapped IPv6 for the IPv6 column.
type BGPPeer struct {
	PolledAt        time.Time
	Exporter        string
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
func (rc *RealClient) WalkBGP(ctx context.Context, target string) ([]BGPPeer, error) {
	g, err := buildGoSNMP(target, rc.cfg, ctx)
	if err != nil {
		return nil, fmt.Errorf("snmpx: bgp dial %s: %w", target, err)
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmpx: bgp connect %s: %w", target, err)
	}
	defer g.Conn.Close()
	return walkBGPPeers(g, target)
}
