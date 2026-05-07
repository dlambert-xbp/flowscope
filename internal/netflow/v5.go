// Package netflow implements parsers for NetFlow v5, NetFlow v9, and
// IPFIX. Each parser produces canonical record.Flow values; the caller
// (cmd/ingest) is responsible for invoking record.Emitter.Emit.
//
// Keeping parsers pure (no Emitter dependency) lets golden-pcap tests
// assert exact byte-to-Flow correctness without standing up a full
// pipeline.
package netflow

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// NetFlow v5 wire layout (RFC-equivalent; v5 predates the IETF process):
//
//   Header (24 bytes, big-endian throughout):
//     uint16 version       (always 5)
//     uint16 count         (1..30)
//     uint32 sysUpTime     (ms since exporter boot)
//     uint32 unixSecs      (epoch seconds at packet emit)
//     uint32 unixNsecs     (residual nanoseconds)
//     uint32 flowSequence  (running counter from this exporter)
//     uint8  engineType
//     uint8  engineID
//     uint16 sampling      (top 2 bits = mode; bottom 14 = interval)
//
//   Record (48 bytes), repeated `count` times:
//     uint32 srcAddr
//     uint32 dstAddr
//     uint32 nextHop
//     uint16 input         (ingress ifIndex)
//     uint16 output        (egress ifIndex)
//     uint32 packets
//     uint32 bytes
//     uint32 firstSwitched (ms since boot)
//     uint32 lastSwitched  (ms since boot)
//     uint16 srcPort
//     uint16 dstPort
//     uint8  pad
//     uint8  tcpFlags
//     uint8  proto
//     uint8  tos
//     uint16 srcAS
//     uint16 dstAS
//     uint8  srcMask
//     uint8  dstMask
//     uint16 pad

const (
	v5HeaderLen = 24
	v5RecordLen = 48
	v5Version   = 5
	v5MaxRecs   = 30
)

// Sentinel errors. Listener loops use errors.Is to bucket parse
// failures into Prometheus counters without coupling to internals.
var (
	ErrShortPacket = errors.New("netflow: short packet")
	ErrBadVersion  = errors.New("netflow: not v5")
	ErrBadCount    = errors.New("netflow: invalid record count")
	ErrTruncated   = errors.New("netflow: truncated record area")
)

// ParseV5 decodes a NetFlow v5 datagram from buf and appends one Flow
// per contained record to dst. Exporter is taken from the UDP source
// (the caller's responsibility) and stamped on every emitted Flow.
//
// The dst slice is reused across calls when callers provide a non-nil
// scratch buffer with `dst = dst[:0]`, avoiding per-packet allocation
// in the hot path.
func ParseV5(buf []byte, exporter netip.Addr, dst []record.Flow) ([]record.Flow, error) {
	if len(buf) < v5HeaderLen {
		return dst, ErrShortPacket
	}
	if version := binary.BigEndian.Uint16(buf[0:2]); version != v5Version {
		return dst, ErrBadVersion
	}
	count := binary.BigEndian.Uint16(buf[2:4])
	if count == 0 || count > v5MaxRecs {
		return dst, ErrBadCount
	}
	if len(buf) < v5HeaderLen+int(count)*v5RecordLen {
		return dst, ErrTruncated
	}

	// Boot-relative wall clock: bootWall = packet-time - sysUpTime.
	// Each record's lastSwitched (ms since boot) maps to wall time.
	sysUpTime := binary.BigEndian.Uint32(buf[4:8])
	unixSecs := binary.BigEndian.Uint32(buf[8:12])
	unixNsecs := binary.BigEndian.Uint32(buf[12:16])
	bootWall := time.Unix(int64(unixSecs), int64(unixNsecs)).
		Add(-time.Duration(sysUpTime) * time.Millisecond)

	off := v5HeaderLen
	for i := uint16(0); i < count; i++ {
		rec := buf[off : off+v5RecordLen]
		off += v5RecordLen

		srcAddr := netip.AddrFrom4([4]byte{rec[0], rec[1], rec[2], rec[3]})
		dstAddr := netip.AddrFrom4([4]byte{rec[4], rec[5], rec[6], rec[7]})
		input := binary.BigEndian.Uint16(rec[12:14])
		output := binary.BigEndian.Uint16(rec[14:16])
		packets := binary.BigEndian.Uint32(rec[16:20])
		bytes := binary.BigEndian.Uint32(rec[20:24])
		lastSwitched := binary.BigEndian.Uint32(rec[28:32])
		srcPort := binary.BigEndian.Uint16(rec[32:34])
		dstPort := binary.BigEndian.Uint16(rec[34:36])
		tcpFlags := rec[37]
		proto := rec[38]
		tos := rec[39]

		dst = append(dst, record.Flow{
			Observed:      bootWall.Add(time.Duration(lastSwitched) * time.Millisecond),
			Exporter:      exporter,
			SrcAddr:       srcAddr,
			DstAddr:       dstAddr,
			SrcPort:       srcPort,
			DstPort:       dstPort,
			Proto:         proto,
			Bytes:         uint64(bytes),
			Packets:       uint64(packets),
			InputIfIndex:  uint32(input),
			OutputIfIndex: uint32(output),
			Tos:           tos,
			TCPFlags:      tcpFlags,
			Source:        record.SourceNetFlowV5,
		})
	}
	return dst, nil
}
