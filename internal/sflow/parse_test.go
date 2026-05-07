package sflow

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// builder assembles synthetic sFlow v5 datagrams for parser tests.
// All offsets and lengths mirror the real wire layout.
type builder struct {
	hdr     []byte
	samples []byte
	count   uint32
}

func newBuilder(agent netip.Addr) *builder {
	b := &builder{}
	hdr := make([]byte, 0, 28)
	hdr = appendU32(hdr, sflowVersion)
	if agent.Is4() {
		hdr = appendU32(hdr, 1) // ip type IPv4
		a := agent.As4()
		hdr = append(hdr, a[:]...)
	} else {
		hdr = appendU32(hdr, 2) // ip type IPv6
		a := agent.As16()
		hdr = append(hdr, a[:]...)
	}
	hdr = appendU32(hdr, 0)    // sub agent
	hdr = appendU32(hdr, 1)    // sequence
	hdr = appendU32(hdr, 1000) // sysUpTime ms
	b.hdr = hdr
	return b
}

func (b *builder) addSample(format uint32, body []byte) *builder {
	b.samples = appendU32(b.samples, format)
	b.samples = appendU32(b.samples, uint32(len(body)))
	b.samples = append(b.samples, body...)
	b.count++
	return b
}

func (b *builder) build() []byte {
	out := append([]byte{}, b.hdr...)
	out = appendU32(out, b.count)
	out = append(out, b.samples...)
	return out
}

func appendU32(b []byte, v uint32) []byte {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	return append(b, x[:]...)
}
func appendU64(b []byte, v uint64) []byte {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	return append(b, x[:]...)
}

// makeIPv4TCPHeader builds a synthetic Ethernet + IPv4 + TCP header
// (60 bytes total: 14 eth + 20 ip + 20 tcp + 6 unused) matching the
// real on-wire layout. Used as the raw_packet_header payload.
func makeIPv4TCPHeader(srcIP, dstIP netip.Addr, srcPort, dstPort uint16, tcpFlags byte) []byte {
	hdr := make([]byte, 0, 60)
	// dst MAC (6) + src MAC (6) + ethertype (2)
	hdr = append(hdr,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02,
	)
	hdr = append(hdr, 0x08, 0x00) // IPv4
	// IPv4: version+IHL=0x45, tos=0, total length=40, id=0, flags+frag=0,
	// ttl=64, proto=6 (TCP), checksum=0, src, dst
	ip := []byte{
		0x45, 0x00, 0, 40, 0, 0, 0, 0, 64, 6, 0, 0,
	}
	srcB := srcIP.As4()
	dstB := dstIP.As4()
	ip = append(ip, srcB[:]...)
	ip = append(ip, dstB[:]...)
	hdr = append(hdr, ip...)
	// TCP: srcPort, dstPort, seq, ack, dataOff+flags, window, csum, urg
	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	tcp[12] = 0x50 // dataOffset = 5 (20 bytes)
	tcp[13] = tcpFlags
	hdr = append(hdr, tcp...)
	return hdr
}

func makeIPv4UDPHeader(srcIP, dstIP netip.Addr, srcPort, dstPort uint16) []byte {
	hdr := make([]byte, 0, 42)
	hdr = append(hdr,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02,
	)
	hdr = append(hdr, 0x08, 0x00)
	ip := []byte{
		0x45, 0x00, 0, 28, 0, 0, 0, 0, 64, 17, 0, 0,
	}
	srcB := srcIP.As4()
	dstB := dstIP.As4()
	ip = append(ip, srcB[:]...)
	ip = append(ip, dstB[:]...)
	hdr = append(hdr, ip...)
	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	hdr = append(hdr, udp...)
	return hdr
}

// makeFlowSampleBody composes one non-expanded flow_sample body with a
// single raw_packet_header record holding the supplied L2/L3/L4 bytes.
func makeFlowSampleBody(inIf, outIf, frameLen uint32, header []byte) []byte {
	// flow_sample body
	body := make([]byte, 0, 64+len(header))
	body = appendU32(body, 1)       // sequence
	body = appendU32(body, 0)       // source_id
	body = appendU32(body, 1)       // sampling_rate
	body = appendU32(body, 0)       // sample_pool
	body = appendU32(body, 0)       // drops
	body = appendU32(body, inIf)    // input
	body = appendU32(body, outIf)   // output
	body = appendU32(body, 1)       // num_records

	// one raw_packet_header record
	rec := make([]byte, 0, 16+len(header))
	rec = appendU32(rec, 1)               // protocol = ETHERNET
	rec = appendU32(rec, frameLen)        // frame_length
	rec = appendU32(rec, 0)               // stripped
	rec = appendU32(rec, uint32(len(header))) // header_length
	rec = append(rec, header...)
	body = appendU32(body, flowRecordRawPacketHeader) // record type
	body = appendU32(body, uint32(len(rec)))          // record length
	body = append(body, rec...)
	return body
}

// makeIfCountersBody composes one non-expanded counters_sample body
// with a single if_counters record.
func makeIfCountersBody(ifIndex uint32, inO, outO uint64, inP, outP, inE, outE, inD, outD uint32) []byte {
	body := make([]byte, 0, 24+88)
	body = appendU32(body, 1) // sequence
	body = appendU32(body, 0) // source_id
	body = appendU32(body, 1) // num_records

	rec := make([]byte, 0, 88)
	rec = appendU32(rec, ifIndex)
	rec = appendU32(rec, 6)        // ifType
	rec = appendU64(rec, 10_000_000_000) // 10G
	rec = appendU32(rec, 1)        // ifDirection
	rec = appendU32(rec, 3)        // ifStatus = up
	rec = appendU64(rec, inO)
	rec = appendU32(rec, inP) // ifInUcast
	rec = appendU32(rec, 0)   // ifInMulticast
	rec = appendU32(rec, 0)   // ifInBroadcast
	rec = appendU32(rec, inD) // ifInDiscards
	rec = appendU32(rec, inE) // ifInErrors
	rec = appendU32(rec, 0)   // ifInUnknownProtos
	rec = appendU64(rec, outO)
	rec = appendU32(rec, outP) // ifOutUcast
	rec = appendU32(rec, 0)
	rec = appendU32(rec, 0)
	rec = appendU32(rec, outD)
	rec = appendU32(rec, outE)
	rec = appendU32(rec, 0) // ifPromiscuousMode

	body = appendU32(body, counterRecordIfCounters)
	body = appendU32(body, uint32(len(rec)))
	body = append(body, rec...)
	return body
}

func TestParse_FlowSample_TCP(t *testing.T) {
	agent := netip.MustParseAddr("10.2.0.11")
	src := netip.MustParseAddr("10.4.7.21")
	dst := netip.MustParseAddr("10.8.4.130")

	hdr := makeIPv4TCPHeader(src, dst, 51422, 443, 0x18)
	pkt := newBuilder(agent).
		addSample(formatFlowSample, makeFlowSampleBody(2, 4, 1500, hdr)).
		build()

	flows, counters, err := Parse(pkt, netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("len(flows) = %d, want 1", len(flows))
	}
	if len(counters) != 0 {
		t.Errorf("len(counters) = %d, want 0", len(counters))
	}
	f := flows[0]
	if f.Exporter != agent {
		t.Errorf("Exporter = %v, want agent %v", f.Exporter, agent)
	}
	if f.SrcAddr.String() != "10.4.7.21" {
		t.Errorf("SrcAddr = %v", f.SrcAddr)
	}
	if f.DstAddr.String() != "10.8.4.130" {
		t.Errorf("DstAddr = %v", f.DstAddr)
	}
	if f.SrcPort != 51422 || f.DstPort != 443 {
		t.Errorf("ports = (%d,%d)", f.SrcPort, f.DstPort)
	}
	if f.Proto != 6 {
		t.Errorf("Proto = %d, want 6", f.Proto)
	}
	if f.TCPFlags != 0x18 {
		t.Errorf("TCPFlags = %#x", f.TCPFlags)
	}
	if f.Bytes != 1500 {
		t.Errorf("Bytes = %d", f.Bytes)
	}
	if f.Packets != 1 {
		t.Errorf("Packets = %d", f.Packets)
	}
	if f.InputIfIndex != 2 || f.OutputIfIndex != 4 {
		t.Errorf("ifIndex = (%d,%d)", f.InputIfIndex, f.OutputIfIndex)
	}
	if f.Source != record.SourceSFlowV5 {
		t.Errorf("Source = %v", f.Source)
	}
}

func TestParse_FlowSample_UDP(t *testing.T) {
	agent := netip.MustParseAddr("10.2.0.11")
	src := netip.MustParseAddr("10.4.7.21")
	dst := netip.MustParseAddr("8.8.8.8")

	hdr := makeIPv4UDPHeader(src, dst, 35211, 53)
	pkt := newBuilder(agent).
		addSample(formatFlowSample, makeFlowSampleBody(2, 4, 412, hdr)).
		build()

	flows, _, err := Parse(pkt, netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("len = %d", len(flows))
	}
	if flows[0].Proto != 17 || flows[0].DstPort != 53 {
		t.Errorf("UDP/53 not decoded: %+v", flows[0])
	}
}

func TestParse_AgentZeroFallsBackToUDPSource(t *testing.T) {
	agent := netip.MustParseAddr("0.0.0.0")
	src := netip.MustParseAddr("10.4.7.21")
	dst := netip.MustParseAddr("10.8.4.130")
	udpSource := netip.MustParseAddr("172.16.0.99")

	hdr := makeIPv4TCPHeader(src, dst, 1, 80, 0)
	pkt := newBuilder(agent).
		addSample(formatFlowSample, makeFlowSampleBody(1, 1, 64, hdr)).
		build()

	flows, _, err := Parse(pkt, udpSource, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if flows[0].Exporter != udpSource {
		t.Errorf("Exporter = %v, want UDP source %v (agent was 0.0.0.0)", flows[0].Exporter, udpSource)
	}
}

func TestParse_CountersSample_IfCounters(t *testing.T) {
	agent := netip.MustParseAddr("10.2.0.11")
	pkt := newBuilder(agent).
		addSample(formatCountersSample, makeIfCountersBody(7,
			1_000_000, 2_000_000,
			100, 200, 1, 2, 3, 4,
		)).
		build()

	_, counters, err := Parse(pkt, netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(counters) != 1 {
		t.Fatalf("len(counters) = %d, want 1", len(counters))
	}
	c := counters[0]
	if c.Exporter != agent {
		t.Errorf("Exporter = %v", c.Exporter)
	}
	if c.IfIndex != 7 {
		t.Errorf("IfIndex = %d", c.IfIndex)
	}
	if c.InOctets != 1_000_000 || c.OutOctets != 2_000_000 {
		t.Errorf("octets = (%d,%d)", c.InOctets, c.OutOctets)
	}
	if c.InErrors != 1 || c.OutErrors != 2 || c.InDiscards != 3 || c.OutDiscards != 4 {
		t.Errorf("err/disc = (%d,%d,%d,%d)", c.InErrors, c.OutErrors, c.InDiscards, c.OutDiscards)
	}
	if c.InPackets != 100 || c.OutPackets != 200 {
		t.Errorf("packets = (%d,%d)", c.InPackets, c.OutPackets)
	}
	if c.Source != record.SourceSFlowV5 {
		t.Errorf("Source = %v", c.Source)
	}
}

func TestParse_MixedSamplesInOneDatagram(t *testing.T) {
	agent := netip.MustParseAddr("10.2.0.11")
	src := netip.MustParseAddr("10.4.7.21")
	dst := netip.MustParseAddr("10.8.4.130")
	flowHdr := makeIPv4TCPHeader(src, dst, 51422, 443, 0x18)

	pkt := newBuilder(agent).
		addSample(formatFlowSample, makeFlowSampleBody(2, 4, 1500, flowHdr)).
		addSample(formatCountersSample, makeIfCountersBody(2,
			500_000, 750_000, 50, 75, 0, 0, 0, 0,
		)).
		addSample(formatFlowSample, makeFlowSampleBody(2, 4, 1500, flowHdr)).
		build()

	flows, counters, err := Parse(pkt, netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(flows) != 2 {
		t.Errorf("len(flows) = %d, want 2", len(flows))
	}
	if len(counters) != 1 {
		t.Errorf("len(counters) = %d, want 1", len(counters))
	}
}

func TestParse_BadVersion(t *testing.T) {
	buf := make([]byte, 28)
	binary.BigEndian.PutUint32(buf[0:4], 4) // not v5
	_, _, err := Parse(buf, netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != ErrBadVersion {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func TestParse_ShortPacket(t *testing.T) {
	_, _, err := Parse(make([]byte, 10), netip.MustParseAddr("1.2.3.4"), nil, nil)
	if err != ErrShortPacket {
		t.Errorf("err = %v, want ErrShortPacket", err)
	}
}

func TestParse_AppendsToExistingSlices(t *testing.T) {
	agent := netip.MustParseAddr("10.2.0.11")
	hdr := makeIPv4TCPHeader(
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("10.0.0.2"),
		1234, 80, 0,
	)
	pkt := newBuilder(agent).
		addSample(formatFlowSample, makeFlowSampleBody(1, 1, 64, hdr)).
		build()

	flows := []record.Flow{{Bytes: 9999}}
	counters := []record.CounterSample{{IfIndex: 999}}
	flows, counters, err := Parse(pkt, netip.MustParseAddr("1.2.3.4"), flows, counters)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 2 {
		t.Errorf("len(flows) = %d, want 2 (existing + parsed)", len(flows))
	}
	if flows[0].Bytes != 9999 {
		t.Errorf("existing flow modified: %v", flows[0])
	}
	if len(counters) != 1 || counters[0].IfIndex != 999 {
		t.Errorf("counters slice mutated: %v", counters)
	}
}
