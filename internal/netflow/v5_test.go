package netflow

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// v5RecFixture mirrors the on-wire NetFlow v5 record layout for the
// fields ParseV5 cares about. Used by buildV5Packet to assemble
// synthetic test packets.
type v5RecFixture struct {
	src, dst             netip.Addr
	input, output        uint16
	packets, bytes       uint32
	lastSwitched         uint32
	srcPort, dstPort     uint16
	tcpFlags, proto, tos uint8
}

// buildV5Packet assembles a synthetic NetFlow v5 datagram from the
// supplied header timing and record fixtures. Layout matches v5.go.
func buildV5Packet(sysUpTime, unixSecs uint32, recs []v5RecFixture) []byte {
	buf := make([]byte, v5HeaderLen+len(recs)*v5RecordLen)
	binary.BigEndian.PutUint16(buf[0:2], v5Version)
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(recs)))
	binary.BigEndian.PutUint32(buf[4:8], sysUpTime)
	binary.BigEndian.PutUint32(buf[8:12], unixSecs)
	binary.BigEndian.PutUint32(buf[12:16], 0) // nsecs
	binary.BigEndian.PutUint32(buf[16:20], 1) // flowSequence
	off := v5HeaderLen
	for _, r := range recs {
		s4 := r.src.As4()
		d4 := r.dst.As4()
		copy(buf[off:off+4], s4[:])
		copy(buf[off+4:off+8], d4[:])
		binary.BigEndian.PutUint16(buf[off+12:off+14], r.input)
		binary.BigEndian.PutUint16(buf[off+14:off+16], r.output)
		binary.BigEndian.PutUint32(buf[off+16:off+20], r.packets)
		binary.BigEndian.PutUint32(buf[off+20:off+24], r.bytes)
		binary.BigEndian.PutUint32(buf[off+28:off+32], r.lastSwitched)
		binary.BigEndian.PutUint16(buf[off+32:off+34], r.srcPort)
		binary.BigEndian.PutUint16(buf[off+34:off+36], r.dstPort)
		buf[off+37] = r.tcpFlags
		buf[off+38] = r.proto
		buf[off+39] = r.tos
		off += v5RecordLen
	}
	return buf
}

func TestParseV5_OneRecord(t *testing.T) {
	exporter := netip.MustParseAddr("10.2.0.11")
	now := time.Date(2026, 5, 7, 15, 42, 18, 0, time.UTC)
	pkt := buildV5Packet(60_000, uint32(now.Unix()), []v5RecFixture{
		{
			src: netip.MustParseAddr("10.4.7.21"), dst: netip.MustParseAddr("10.8.4.130"),
			input: 2, output: 4,
			packets: 24, bytes: 14_240,
			lastSwitched: 60_000, // exactly at sysUpTime → wall = unixSecs
			srcPort:      51422, dstPort: 443,
			tcpFlags: 0x18, proto: 6, tos: 0,
		},
	})

	out, err := ParseV5(pkt, exporter, nil)
	if err != nil {
		t.Fatalf("ParseV5: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	got := out[0]
	if got.Exporter != exporter {
		t.Errorf("Exporter = %v, want %v", got.Exporter, exporter)
	}
	if got.SrcAddr.String() != "10.4.7.21" {
		t.Errorf("SrcAddr = %v", got.SrcAddr)
	}
	if got.DstAddr.String() != "10.8.4.130" {
		t.Errorf("DstAddr = %v", got.DstAddr)
	}
	if got.SrcPort != 51422 {
		t.Errorf("SrcPort = %d", got.SrcPort)
	}
	if got.DstPort != 443 {
		t.Errorf("DstPort = %d", got.DstPort)
	}
	if got.Proto != 6 {
		t.Errorf("Proto = %d", got.Proto)
	}
	if got.Bytes != 14_240 {
		t.Errorf("Bytes = %d", got.Bytes)
	}
	if got.Packets != 24 {
		t.Errorf("Packets = %d", got.Packets)
	}
	if got.InputIfIndex != 2 || got.OutputIfIndex != 4 {
		t.Errorf("ifIndex = (%d,%d)", got.InputIfIndex, got.OutputIfIndex)
	}
	if got.TCPFlags != 0x18 {
		t.Errorf("TCPFlags = %#x", got.TCPFlags)
	}
	if got.Source != record.SourceNetFlowV5 {
		t.Errorf("Source = %v", got.Source)
	}
	// lastSwitched == sysUpTime ⇒ Observed wall ≈ unixSecs
	if got.Observed.Unix() != now.Unix() {
		t.Errorf("Observed = %v, want unix=%d", got.Observed, now.Unix())
	}
}

func TestParseV5_MultipleRecords(t *testing.T) {
	pkt := buildV5Packet(1000, 1_700_000_000, []v5RecFixture{
		{src: netip.MustParseAddr("10.0.0.1"), dst: netip.MustParseAddr("10.0.0.2"), bytes: 100, packets: 1, proto: 6, lastSwitched: 1000},
		{src: netip.MustParseAddr("10.0.0.3"), dst: netip.MustParseAddr("10.0.0.4"), bytes: 200, packets: 2, proto: 17, lastSwitched: 1000},
		{src: netip.MustParseAddr("10.0.0.5"), dst: netip.MustParseAddr("10.0.0.6"), bytes: 300, packets: 3, proto: 1, lastSwitched: 1000},
	})
	out, err := ParseV5(pkt, netip.MustParseAddr("10.2.0.11"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	wantBytes := []uint64{100, 200, 300}
	wantProto := []uint8{6, 17, 1}
	for i := range wantBytes {
		if out[i].Bytes != wantBytes[i] {
			t.Errorf("out[%d].Bytes = %d, want %d", i, out[i].Bytes, wantBytes[i])
		}
		if out[i].Proto != wantProto[i] {
			t.Errorf("out[%d].Proto = %d, want %d", i, out[i].Proto, wantProto[i])
		}
	}
}

func TestParseV5_AppendsToExisting(t *testing.T) {
	pkt := buildV5Packet(1000, 1_700_000_000, []v5RecFixture{
		{src: netip.MustParseAddr("10.0.0.1"), dst: netip.MustParseAddr("10.0.0.2"), bytes: 100, lastSwitched: 1000},
	})
	scratch := make([]record.Flow, 0, 8)
	scratch = append(scratch, record.Flow{Bytes: 999})
	out, err := ParseV5(pkt, netip.MustParseAddr("1.2.3.4"), scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (existing + parsed)", len(out))
	}
	if out[0].Bytes != 999 || out[1].Bytes != 100 {
		t.Errorf("append order wrong: %+v", out)
	}
}

func TestParseV5_ShortPacket(t *testing.T) {
	if _, err := ParseV5(make([]byte, 10), netip.MustParseAddr("1.2.3.4"), nil); err != ErrShortPacket {
		t.Errorf("err = %v, want ErrShortPacket", err)
	}
}

func TestParseV5_BadVersion(t *testing.T) {
	pkt := make([]byte, v5HeaderLen)
	binary.BigEndian.PutUint16(pkt[0:2], 9) // not v5
	binary.BigEndian.PutUint16(pkt[2:4], 0)
	if _, err := ParseV5(pkt, netip.MustParseAddr("1.2.3.4"), nil); err != ErrBadVersion {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func TestParseV5_BadCount(t *testing.T) {
	pkt := make([]byte, v5HeaderLen)
	binary.BigEndian.PutUint16(pkt[0:2], v5Version)
	binary.BigEndian.PutUint16(pkt[2:4], 31) // exceeds v5MaxRecs
	if _, err := ParseV5(pkt, netip.MustParseAddr("1.2.3.4"), nil); err != ErrBadCount {
		t.Errorf("err = %v, want ErrBadCount", err)
	}
}

func TestParseV5_ZeroCount(t *testing.T) {
	pkt := make([]byte, v5HeaderLen)
	binary.BigEndian.PutUint16(pkt[0:2], v5Version)
	binary.BigEndian.PutUint16(pkt[2:4], 0)
	if _, err := ParseV5(pkt, netip.MustParseAddr("1.2.3.4"), nil); err != ErrBadCount {
		t.Errorf("err = %v, want ErrBadCount", err)
	}
}

func TestParseV5_Truncated(t *testing.T) {
	// Header claims 1 record but the buffer is short by 5 bytes.
	pkt := make([]byte, v5HeaderLen+v5RecordLen-5)
	binary.BigEndian.PutUint16(pkt[0:2], v5Version)
	binary.BigEndian.PutUint16(pkt[2:4], 1)
	if _, err := ParseV5(pkt, netip.MustParseAddr("1.2.3.4"), nil); err != ErrTruncated {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}
