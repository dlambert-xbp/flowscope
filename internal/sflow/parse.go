// Package sflow implements an sFlow v5 datagram decoder. A single
// datagram contains zero or more samples; each sample is either a
// flow_sample (a captured packet plus exporter context) or a
// counters_sample (interface counter snapshot).
//
// Parse appends decoded values to two destination slices — flows and
// counter samples — and returns them. The caller (cmd/ingest) routes
// each through its respective Emitter / Sink fan-out. Keeping the
// parser pure and side-effect-free lets golden-bytes tests assert
// exact wire-to-record correctness.
//
// Wire reference: sFlow v5 specification (sFlow.org). All integers
// are big-endian. Strings and opaque blobs are zero-padded to 4-byte
// alignment.
package sflow

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// Sentinel errors. Callers bucket parse failures into Prometheus
// counters via errors.Is without coupling to internals.
var (
	ErrShortPacket = errors.New("sflow: short packet")
	ErrBadVersion  = errors.New("sflow: not v5")
	ErrTruncated   = errors.New("sflow: truncated sample area")
	ErrAddressKind = errors.New("sflow: unsupported agent address type")
)

// sFlow datagram header (v5):
//
//	uint32 version            (always 5)
//	uint32 ip_address_type    (1 = IPv4, 2 = IPv6)
//	bytes  agent_address      (4 if IPv4, 16 if IPv6)
//	uint32 sub_agent_id
//	uint32 sequence_number
//	uint32 sysUpTime          (ms since boot)
//	uint32 num_samples
//	... num_samples samples follow
//
// Each sample is preceded by:
//
//	uint32 sample_type        (top 12 bits = enterprise, bottom 20 = format)
//	uint32 sample_length      (bytes of sample body that follow)
const (
	sflowVersion = 5

	// Sample format codes (enterprise=0, format=N).
	formatFlowSample             = 1
	formatCountersSample         = 2
	formatFlowSampleExpanded     = 3
	formatCountersSampleExpanded = 4

	// Flow record formats inside a flow_sample.
	flowRecordRawPacketHeader = 1
	flowRecordExtSwitch       = 1001 // 802.1Q VLAN context — not yet decoded
	flowRecordExtRouter       = 1002 // next-hop info — not yet decoded
	flowRecordExtGateway      = 1003 // BGP route info: src_as, dst_as_path, communities

	// Counter record formats inside a counters_sample.
	counterRecordIfCounters = 1

	// Layer-3 protocols inside the sampled raw packet header.
	headerProtoEthernet = 1

	// EtherType values.
	etherTypeIPv4 = 0x0800
	etherTypeIPv6 = 0x86DD
	etherType8021Q = 0x8100

	// IP protocol numbers.
	ipProtoTCP = 6
	ipProtoUDP = 17
)

// ReadSequence extracts the sFlow datagram sequence number from
// a v5 header without doing the full sample-area decode. Returns
// (0, false) on a buffer that's too short or wrong-versioned.
// Caller should call this before Parse so a partial or malformed
// sample area still credits the datagram for loss-detection.
func ReadSequence(buf []byte) (uint32, bool) {
	if len(buf) < 8 {
		return 0, false
	}
	if binary.BigEndian.Uint32(buf[0:4]) != sflowVersion {
		return 0, false
	}
	addrType := binary.BigEndian.Uint32(buf[4:8])
	var seqOff int
	switch addrType {
	case 1: // IPv4 agent address (4 bytes)
		seqOff = 8 + 4 + 4 // header + agent + sub_agent_id
	case 2: // IPv6 agent address (16 bytes)
		seqOff = 8 + 16 + 4
	default:
		return 0, false
	}
	if len(buf) < seqOff+4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(buf[seqOff : seqOff+4]), true
}

// Parse decodes an sFlow v5 datagram from buf. Decoded flows are
// appended to flowDst; decoded counter samples are appended to
// counterDst. The caller's UDP source address is supplied as
// fallbackExporter and used only when the agent_address in the
// datagram header is 0.0.0.0 (per VISION.md §4.1).
//
// Parse is best-effort with respect to unknown sample formats and
// record types — it skips them and continues, mirroring the tolerance
// real switches expect.
func Parse(
	buf []byte,
	fallbackExporter netip.Addr,
	flowDst []record.Flow,
	counterDst []record.CounterSample,
) ([]record.Flow, []record.CounterSample, error) {
	if len(buf) < 28 {
		return flowDst, counterDst, ErrShortPacket
	}
	version := binary.BigEndian.Uint32(buf[0:4])
	if version != sflowVersion {
		return flowDst, counterDst, ErrBadVersion
	}
	addrType := binary.BigEndian.Uint32(buf[4:8])

	var (
		agent  netip.Addr
		offset int
	)
	switch addrType {
	case 1: // IPv4
		if len(buf) < 8+4+16 {
			return flowDst, counterDst, ErrShortPacket
		}
		agent = netip.AddrFrom4([4]byte{buf[8], buf[9], buf[10], buf[11]})
		offset = 12
	case 2: // IPv6
		if len(buf) < 8+16+16 {
			return flowDst, counterDst, ErrShortPacket
		}
		var b16 [16]byte
		copy(b16[:], buf[8:24])
		agent = netip.AddrFrom16(b16)
		offset = 24
	default:
		return flowDst, counterDst, ErrAddressKind
	}

	// Honour the §4.1 invariant: agent address overrides UDP source IP
	// unless the agent reports 0.0.0.0 / :: (rare but real).
	exporter := fallbackExporter
	if agent.IsValid() && !agent.IsUnspecified() {
		exporter = agent
	}

	if len(buf) < offset+12 {
		return flowDst, counterDst, ErrShortPacket
	}
	// subAgent (4) + sequence (4) + sysUpTime (4) -> skip.
	sysUpTime := time.Duration(binary.BigEndian.Uint32(buf[offset+8:offset+12])) * time.Millisecond
	_ = sysUpTime
	offset += 12

	numSamples := binary.BigEndian.Uint32(buf[offset : offset+4])
	offset += 4

	now := time.Now().UTC()
	for i := uint32(0); i < numSamples; i++ {
		if offset+8 > len(buf) {
			return flowDst, counterDst, ErrTruncated
		}
		sampleType := binary.BigEndian.Uint32(buf[offset : offset+4])
		sampleLen := binary.BigEndian.Uint32(buf[offset+4 : offset+8])
		offset += 8

		end := offset + int(sampleLen)
		if end > len(buf) {
			return flowDst, counterDst, ErrTruncated
		}
		body := buf[offset:end]
		offset = end

		// Bottom 20 bits are the format; top 12 bits are the
		// enterprise number (0 for standard). We only decode
		// enterprise=0 known formats; everything else is skipped.
		if (sampleType >> 20) != 0 {
			continue
		}
		switch sampleType & 0xFFFFF {
		case formatFlowSample:
			fl, err := parseFlowSample(body, exporter, false)
			if err != nil {
				continue // tolerate per-sample errors
			}
			for j := range fl {
				fl[j].Observed = now
			}
			flowDst = append(flowDst, fl...)

		case formatFlowSampleExpanded:
			fl, err := parseFlowSample(body, exporter, true)
			if err != nil {
				continue
			}
			for j := range fl {
				fl[j].Observed = now
			}
			flowDst = append(flowDst, fl...)

		case formatCountersSample:
			cs, err := parseCountersSample(body, exporter, false)
			if err != nil {
				continue
			}
			for j := range cs {
				cs[j].Observed = now
			}
			counterDst = append(counterDst, cs...)

		case formatCountersSampleExpanded:
			cs, err := parseCountersSample(body, exporter, true)
			if err != nil {
				continue
			}
			for j := range cs {
				cs[j].Observed = now
			}
			counterDst = append(counterDst, cs...)
		}
	}
	return flowDst, counterDst, nil
}

// parseFlowSample decodes one flow_sample (or flow_sample_expanded)
// body. A flow_sample contains exporter context (sampling rate, in/out
// ifIndex) plus a list of flow records, of which we currently decode
// only raw_packet_header. One Flow per decoded record is appended.
func parseFlowSample(body []byte, exporter netip.Addr, expanded bool) ([]record.Flow, error) {
	const minNonExpanded = 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 // 32 bytes
	const minExpanded = 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 // 44 bytes
	min := minNonExpanded
	if expanded {
		min = minExpanded
	}
	if len(body) < min {
		return nil, ErrTruncated
	}

	off := 4 // sequence
	if expanded {
		off += 4 + 4 // ds_class + ds_index
	} else {
		off += 4 // source_id
	}
	off += 4 // sampling rate
	off += 4 // sample pool
	off += 4 // drops

	var inIfIndex, outIfIndex uint32
	if expanded {
		// inputType, inputIndex, outputType, outputIndex (4×uint32)
		off += 4
		inIfIndex = binary.BigEndian.Uint32(body[off : off+4])
		off += 4
		off += 4
		outIfIndex = binary.BigEndian.Uint32(body[off : off+4])
		off += 4
	} else {
		// non-expanded packs (type:2 | ifIndex:30) into one uint32 each.
		inField := binary.BigEndian.Uint32(body[off : off+4])
		off += 4
		outField := binary.BigEndian.Uint32(body[off : off+4])
		off += 4
		inIfIndex = inField & 0x3FFFFFFF
		outIfIndex = outField & 0x3FFFFFFF
	}

	if off+4 > len(body) {
		return nil, ErrTruncated
	}
	numRecords := binary.BigEndian.Uint32(body[off : off+4])
	off += 4

	// Real-world sFlow flow_samples carry one raw_packet_header per
	// sample plus zero or more decorator records (extended_switch,
	// extended_router, extended_gateway). We merge the decorators
	// into the Flow built from raw_packet_header before appending.
	var (
		base    record.Flow
		baseOK  bool
		srcAS   uint32
		dstAS   uint32
	)
	for i := uint32(0); i < numRecords; i++ {
		if off+8 > len(body) {
			return nil, ErrTruncated
		}
		recType := binary.BigEndian.Uint32(body[off : off+4])
		recLen := binary.BigEndian.Uint32(body[off+4 : off+8])
		off += 8
		end := off + int(recLen)
		if end > len(body) {
			return nil, ErrTruncated
		}
		recBody := body[off:end]
		off = end

		if (recType >> 20) != 0 {
			continue // non-standard enterprise
		}
		switch recType & 0xFFFFF {
		case flowRecordRawPacketHeader:
			if f, ok := parseRawPacketHeader(recBody, exporter, inIfIndex, outIfIndex); ok {
				base = f
				baseOK = true
			}
		case flowRecordExtGateway:
			if s, d, ok := parseExtendedGateway(recBody); ok {
				srcAS = s
				dstAS = d
			}
		}
	}
	out := make([]record.Flow, 0, 1)
	if baseOK {
		base.SrcAS = srcAS
		base.DstAS = dstAS
		out = append(out, base)
	}
	return out, nil
}

// parseExtendedGateway decodes the BGP route-info record inside a
// flow_sample. We extract src_as and the last AS in the destination
// AS path (typically the origin AS for the dst prefix). Returns
// (0, 0, false) on a body too short to walk past the required
// fields — callers should treat this as "no ASN data" not an error.
//
// Wire layout (sFlow v5):
//
//	next_hop_type uint32        (1 = IPv4, 2 = IPv6)
//	next_hop      4 or 16 bytes
//	as            uint32        (router's AS — ignored)
//	src_as        uint32
//	src_peer_as   uint32        (ignored)
//	dst_as_path[] of as_path_segment, where each segment is:
//	    type      uint32        (1 = AS_SET, 2 = AS_SEQUENCE)
//	    as[]      uint32×N
//	communities[] uint32×N      (skipped)
//	localpref     uint32        (skipped)
func parseExtendedGateway(body []byte) (srcAS, dstAS uint32, ok bool) {
	if len(body) < 4 {
		return 0, 0, false
	}
	off := 0
	nextHopType := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	switch nextHopType {
	case 1: // IPv4
		off += 4
	case 2: // IPv6
		off += 16
	default:
		return 0, 0, false
	}
	// router as + src_as + src_peer_as + dst_as_path count
	if off+16 > len(body) {
		return 0, 0, false
	}
	off += 4 // router as
	srcAS = binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	off += 4 // src_peer_as
	pathSegCount := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	// Walk each path segment; remember the last AS we saw — that's
	// the destination AS for typical (single-segment) AS paths.
	for i := uint32(0); i < pathSegCount; i++ {
		if off+8 > len(body) {
			return srcAS, dstAS, true // partial; what we have is fine
		}
		off += 4 // segment type
		count := binary.BigEndian.Uint32(body[off : off+4])
		off += 4
		if count == 0 {
			continue
		}
		end := off + int(count)*4
		if end > len(body) {
			return srcAS, dstAS, true
		}
		// Last AS in this segment
		lastOff := end - 4
		dstAS = binary.BigEndian.Uint32(body[lastOff : lastOff+4])
		off = end
	}
	return srcAS, dstAS, true
}

// parseRawPacketHeader decodes the raw_packet_header record that
// carries the bytes of the actual sampled packet. We expect Ethernet
// (single 802.1Q VLAN tolerated), then IPv4, then TCP or UDP.
//
//	uint32 protocol           (1 = ETHERNET-ISO88023)
//	uint32 frame_length       (bytes on the wire, ignoring stripped FCS)
//	uint32 stripped_bytes
//	uint32 header_length      (bytes of header that follow)
//	bytes  header             (header_length, padded to 4-byte boundary)
func parseRawPacketHeader(body []byte, exporter netip.Addr, inIf, outIf uint32) (record.Flow, bool) {
	if len(body) < 16 {
		return record.Flow{}, false
	}
	protocol := binary.BigEndian.Uint32(body[0:4])
	if protocol != headerProtoEthernet {
		return record.Flow{}, false
	}
	frameLen := binary.BigEndian.Uint32(body[4:8])
	headerLen := binary.BigEndian.Uint32(body[12:16])
	if 16+int(headerLen) > len(body) {
		return record.Flow{}, false
	}
	hdr := body[16 : 16+headerLen]

	// Ethernet header: 6 dst + 6 src + 2 etherType, with optional
	// 802.1Q (4 bytes) before the etherType repeats.
	if len(hdr) < 14 {
		return record.Flow{}, false
	}
	etherType := binary.BigEndian.Uint16(hdr[12:14])
	l3 := hdr[14:]
	vlan := uint16(0)
	if etherType == etherType8021Q {
		if len(hdr) < 18 {
			return record.Flow{}, false
		}
		vlan = binary.BigEndian.Uint16(hdr[14:16]) & 0x0FFF
		etherType = binary.BigEndian.Uint16(hdr[16:18])
		l3 = hdr[18:]
	}

	switch etherType {
	case etherTypeIPv4:
		return parseIPv4(l3, exporter, inIf, outIf, vlan, frameLen)
	case etherTypeIPv6:
		return parseIPv6(l3, exporter, inIf, outIf, vlan, frameLen)
	default:
		return record.Flow{}, false
	}
}

func parseIPv4(l3 []byte, exporter netip.Addr, inIf, outIf uint32, vlan uint16, frameLen uint32) (record.Flow, bool) {
	if len(l3) < 20 {
		return record.Flow{}, false
	}
	ihl := int(l3[0]&0x0F) * 4
	if ihl < 20 || len(l3) < ihl {
		return record.Flow{}, false
	}
	tos := l3[1]
	proto := l3[9]
	src := netip.AddrFrom4([4]byte{l3[12], l3[13], l3[14], l3[15]})
	dst := netip.AddrFrom4([4]byte{l3[16], l3[17], l3[18], l3[19]})

	var srcPort, dstPort uint16
	var tcpFlags uint8
	l4 := l3[ihl:]
	switch proto {
	case ipProtoTCP:
		if len(l4) >= 14 {
			srcPort = binary.BigEndian.Uint16(l4[0:2])
			dstPort = binary.BigEndian.Uint16(l4[2:4])
			tcpFlags = l4[13]
		}
	case ipProtoUDP:
		if len(l4) >= 4 {
			srcPort = binary.BigEndian.Uint16(l4[0:2])
			dstPort = binary.BigEndian.Uint16(l4[2:4])
		}
	}
	return record.Flow{
		Exporter:      exporter,
		SrcAddr:       src,
		DstAddr:       dst,
		SrcPort:       srcPort,
		DstPort:       dstPort,
		Proto:         proto,
		Bytes:         uint64(frameLen),
		Packets:       1,
		InputIfIndex:  inIf,
		OutputIfIndex: outIf,
		VlanID:        vlan,
		Tos:           tos,
		TCPFlags:      tcpFlags,
		Source:        record.SourceSFlowV5,
	}, true
}

func parseIPv6(l3 []byte, exporter netip.Addr, inIf, outIf uint32, vlan uint16, frameLen uint32) (record.Flow, bool) {
	if len(l3) < 40 {
		return record.Flow{}, false
	}
	tos := uint8(((l3[0] & 0x0F) << 4) | (l3[1] >> 4)) // traffic class
	proto := l3[6]                                       // Next Header (no extension headers handled)
	var src16, dst16 [16]byte
	copy(src16[:], l3[8:24])
	copy(dst16[:], l3[24:40])
	src := netip.AddrFrom16(src16)
	dst := netip.AddrFrom16(dst16)
	var srcPort, dstPort uint16
	var tcpFlags uint8
	l4 := l3[40:]
	switch proto {
	case ipProtoTCP:
		if len(l4) >= 14 {
			srcPort = binary.BigEndian.Uint16(l4[0:2])
			dstPort = binary.BigEndian.Uint16(l4[2:4])
			tcpFlags = l4[13]
		}
	case ipProtoUDP:
		if len(l4) >= 4 {
			srcPort = binary.BigEndian.Uint16(l4[0:2])
			dstPort = binary.BigEndian.Uint16(l4[2:4])
		}
	}
	return record.Flow{
		Exporter:      exporter,
		SrcAddr:       src,
		DstAddr:       dst,
		SrcPort:       srcPort,
		DstPort:       dstPort,
		Proto:         proto,
		Bytes:         uint64(frameLen),
		Packets:       1,
		InputIfIndex:  inIf,
		OutputIfIndex: outIf,
		VlanID:        vlan,
		Tos:           tos,
		TCPFlags:      tcpFlags,
		Source:        record.SourceSFlowV5,
	}, true
}

// parseCountersSample decodes counters_sample / counters_sample_expanded
// bodies. We only decode if_counters records (record type 1); other
// record types (eth_counters, vlan_counters, etc.) are skipped.
//
// Wire layout for if_counters (88 bytes):
//
//	uint32 ifIndex
//	uint32 ifType
//	uint64 ifSpeed
//	uint32 ifDirection
//	uint32 ifStatus
//	uint64 ifInOctets
//	uint32 ifInUcastPkts
//	uint32 ifInMulticastPkts
//	uint32 ifInBroadcastPkts
//	uint32 ifInDiscards
//	uint32 ifInErrors
//	uint32 ifInUnknownProtos
//	uint64 ifOutOctets
//	uint32 ifOutUcastPkts
//	uint32 ifOutMulticastPkts
//	uint32 ifOutBroadcastPkts
//	uint32 ifOutDiscards
//	uint32 ifOutErrors
//	uint32 ifPromiscuousMode
func parseCountersSample(body []byte, exporter netip.Addr, expanded bool) ([]record.CounterSample, error) {
	const minNonExpanded = 4 + 4 + 4 // 12 bytes
	const minExpanded = 4 + 4 + 4 + 4 // 16 bytes
	min := minNonExpanded
	if expanded {
		min = minExpanded
	}
	if len(body) < min {
		return nil, ErrTruncated
	}

	off := 4 // sequence
	if expanded {
		off += 8 // ds_class + ds_index
	} else {
		off += 4 // source_id
	}
	if off+4 > len(body) {
		return nil, ErrTruncated
	}
	numRecords := binary.BigEndian.Uint32(body[off : off+4])
	off += 4

	out := make([]record.CounterSample, 0, numRecords)
	for i := uint32(0); i < numRecords; i++ {
		if off+8 > len(body) {
			return out, ErrTruncated
		}
		recType := binary.BigEndian.Uint32(body[off : off+4])
		recLen := binary.BigEndian.Uint32(body[off+4 : off+8])
		off += 8
		end := off + int(recLen)
		if end > len(body) {
			return out, ErrTruncated
		}
		recBody := body[off:end]
		off = end

		if (recType >> 20) != 0 {
			continue
		}
		if recType&0xFFFFF != counterRecordIfCounters {
			continue
		}
		c, ok := parseIfCounters(recBody, exporter)
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func parseIfCounters(body []byte, exporter netip.Addr) (record.CounterSample, bool) {
	const ifCountersLen = 4 + 4 + 8 + 4 + 4 + 8 + 4*5 + 8 + 4*5
	if len(body) < ifCountersLen {
		return record.CounterSample{}, false
	}
	ifIndex := binary.BigEndian.Uint32(body[0:4])
	// skip ifType (4), ifSpeed (8), ifDirection (4), ifStatus (4)
	off := 24
	inOctets := binary.BigEndian.Uint64(body[off : off+8])
	off += 8
	inUcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	inMcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	inBcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	inDiscards := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	inErrors := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	off += 4 // ifInUnknownProtos
	outOctets := binary.BigEndian.Uint64(body[off : off+8])
	off += 8
	outUcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	outMcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	outBcast := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	outDiscards := binary.BigEndian.Uint32(body[off : off+4])
	off += 4
	outErrors := binary.BigEndian.Uint32(body[off : off+4])

	return record.CounterSample{
		Exporter:    exporter,
		IfIndex:     ifIndex,
		InOctets:    inOctets,
		OutOctets:   outOctets,
		InPackets:   uint64(inUcast) + uint64(inMcast) + uint64(inBcast),
		OutPackets:  uint64(outUcast) + uint64(outMcast) + uint64(outBcast),
		InErrors:    uint64(inErrors),
		OutErrors:   uint64(outErrors),
		InDiscards:  uint64(inDiscards),
		OutDiscards: uint64(outDiscards),
		Source:      record.SourceSFlowV5,
	}, true
}
