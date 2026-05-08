// Package record defines the canonical flow record and the single Emitter
// fan-out point through which every parsed flow flows.
//
// See VISION.md §3.2 / §4.1 and CLAUDE.md "Data flow" — every parser
// (NetFlow v5, NetFlow v9 / IPFIX, sFlow v5) produces canonical Flow
// values and calls Emitter.Emit. New consumers (ClickHouse batcher,
// Prometheus aggregator, anomaly detector) extend this fan-out;
// nothing parses-then-stores by bypassing it.
package record

import (
	"net/netip"
	"time"
)

// Flow is the canonical representation of one flow record, normalized
// across NetFlow v5, NetFlow v9, IPFIX, and sFlow v5. Parsers produce
// Flow values; nothing else does.
type Flow struct {
	// Observed is when the flow ended, in wall-clock time, derived by
	// the parser from exporter sysUpTime + the packet's unix timestamp.
	Observed time.Time

	// Exporter is the canonical exporter address. For sFlow this comes
	// from the agent address in the datagram header (overriding UDP
	// source IP) except when that field is 0.0.0.0.
	Exporter netip.Addr

	// 5-tuple
	SrcAddr netip.Addr
	DstAddr netip.Addr
	SrcPort uint16
	DstPort uint16
	Proto   uint8 // IP protocol number (TCP=6, UDP=17, ICMP=1, ...)

	// Volume — what crossed the link.
	Bytes   uint64
	Packets uint64

	// Interface context (ifIndex on the exporter).
	InputIfIndex  uint32
	OutputIfIndex uint32

	// VLAN, when known. 0 = unknown / untagged.
	VlanID uint16

	// TOS / DSCP byte from the IP header.
	Tos uint8

	// TCP flags ORed across the flow's lifetime, when known.
	TCPFlags uint8

	// Autonomous System numbers from the exporter's BGP table at the
	// time the flow was metered. NetFlow v9 / IPFIX field IDs 16
	// (src_as) and 17 (dst_as). Zero = not exported / unknown — sFlow
	// today doesn't extract these (would need the BGP gateway
	// extension in the parser).
	SrcAS uint32
	DstAS uint32

	// Source identifies which decoder produced this record. Used by
	// telemetry counters and by the API to label data provenance.
	Source SourceKind
}

// SourceKind identifies which parser produced a Flow.
type SourceKind uint8

const (
	SourceUnknown SourceKind = iota
	SourceNetFlowV5
	SourceNetFlowV9
	SourceIPFIX
	SourceSFlowV5
)

// String returns the wire-format-neutral label used in metrics and JSON.
func (s SourceKind) String() string {
	switch s {
	case SourceNetFlowV5:
		return "netflow.v5"
	case SourceNetFlowV9:
		return "netflow.v9"
	case SourceIPFIX:
		return "ipfix"
	case SourceSFlowV5:
		return "sflow.v5"
	default:
		return "unknown"
	}
}
