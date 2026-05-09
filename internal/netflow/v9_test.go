package netflow

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// ---------- builder helpers ----------

func u16(b []byte, v uint16) []byte {
	var x [2]byte
	binary.BigEndian.PutUint16(x[:], v)
	return append(b, x[:]...)
}
func u32(b []byte, v uint32) []byte {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	return append(b, x[:]...)
}

// makeV9TemplateFlowSet wraps one or more templates into a Template
// FlowSet (id=0). Each template is supplied as ID + (type,length) pairs.
func makeV9TemplateFlowSet(templates ...v9Tpl) []byte {
	body := []byte{}
	for _, t := range templates {
		body = u16(body, t.id)
		body = u16(body, uint16(len(t.fields)))
		for _, f := range t.fields {
			body = u16(body, f.t)
			body = u16(body, f.l)
		}
	}
	out := []byte{}
	out = u16(out, 0)                   // flowset_id = template
	out = u16(out, uint16(4+len(body))) // length
	out = append(out, body...)
	return out
}

// makeDataFlowSet wraps record bytes into a Data FlowSet.
func makeDataFlowSet(templateID uint16, records []byte) []byte {
	out := []byte{}
	out = u16(out, templateID)
	out = u16(out, uint16(4+len(records)))
	out = append(out, records...)
	return out
}

func makeV9Header(count uint16, sysUpTime, unixSecs, sequence, sourceID uint32) []byte {
	h := []byte{}
	h = u16(h, 9)
	h = u16(h, count)
	h = u32(h, sysUpTime)
	h = u32(h, unixSecs)
	h = u32(h, sequence)
	h = u32(h, sourceID)
	return h
}

type v9Field struct {
	t, l uint16
}
type v9Tpl struct {
	id     uint16
	fields []v9Field
}

// ---------- v9 ----------

func TestParseV9_TemplateAndDataInOnePacket(t *testing.T) {
	cache := NewTemplateCache()
	exporter := netip.MustParseAddr("10.2.0.11")

	// Template 257: in_bytes(u32), in_pkts(u32), proto(u8), src_port(u16),
	// dst_port(u16), src_ipv4(u32), dst_ipv4(u32), input_snmp(u32),
	// output_snmp(u32), last_switched(u32). Sum: 31 bytes per record.
	tpl := makeV9TemplateFlowSet(v9Tpl{
		id: 257,
		fields: []v9Field{
			{fieldInBytes, 4}, {fieldInPackets, 4}, {fieldProtocol, 1},
			{fieldL4SrcPort, 2}, {fieldL4DstPort, 2},
			{fieldIPv4SrcAddr, 4}, {fieldIPv4DstAddr, 4},
			{fieldInputSnmp, 4}, {fieldOutputSnmp, 4},
			{fieldLastSwitched, 4},
		},
	})

	rec := []byte{}
	rec = u32(rec, 14_240) // bytes
	rec = u32(rec, 24)     // packets
	rec = append(rec, 6)   // tcp
	rec = u16(rec, 51422)  // srcPort
	rec = u16(rec, 443)    // dstPort
	rec = append(rec, 10, 4, 7, 21)
	rec = append(rec, 10, 8, 4, 130)
	rec = u32(rec, 2)      // input ifindex
	rec = u32(rec, 4)      // output ifindex
	rec = u32(rec, 60_000) // last switched (ms since boot, equal to sysUpTime)

	data := makeDataFlowSet(257, rec)

	pkt := makeV9Header(2, 60_000, 1_700_000_000, 1, 0)
	pkt = append(pkt, tpl...)
	pkt = append(pkt, data...)

	flows, err := ParseV9OrIPFIX(cache, pkt, exporter, nil)
	if err != nil {
		t.Fatalf("ParseV9OrIPFIX: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("len(flows) = %d, want 1", len(flows))
	}
	f := flows[0]
	if f.SrcAddr.String() != "10.4.7.21" || f.DstAddr.String() != "10.8.4.130" {
		t.Errorf("addrs: %v → %v", f.SrcAddr, f.DstAddr)
	}
	if f.SrcPort != 51422 || f.DstPort != 443 {
		t.Errorf("ports: %d → %d", f.SrcPort, f.DstPort)
	}
	if f.Proto != 6 || f.Bytes != 14_240 || f.Packets != 24 {
		t.Errorf("metrics: proto=%d bytes=%d packets=%d", f.Proto, f.Bytes, f.Packets)
	}
	if f.InputIfIndex != 2 || f.OutputIfIndex != 4 {
		t.Errorf("ifindex: %d/%d", f.InputIfIndex, f.OutputIfIndex)
	}
	if f.Source != record.SourceNetFlowV9 {
		t.Errorf("Source = %v", f.Source)
	}
	// lastSwitched == sysUpTime ⇒ Observed wall ≈ unixSecs (1_700_000_000)
	if f.Observed.Unix() != 1_700_000_000 {
		t.Errorf("Observed = %v, want unix 1700000000", f.Observed)
	}

	if cache.Len() != 1 {
		t.Errorf("cache size = %d, want 1", cache.Len())
	}
}

func TestParseV9_DataBeforeTemplate_Dropped(t *testing.T) {
	cache := NewTemplateCache()
	exporter := netip.MustParseAddr("10.2.0.11")

	rec := []byte{0, 0, 0, 0} // junk
	pkt := makeV9Header(1, 1000, 1_700_000_000, 1, 0)
	pkt = append(pkt, makeDataFlowSet(257, rec)...)

	flows, err := ParseV9OrIPFIX(cache, pkt, exporter, nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(flows) != 0 {
		t.Errorf("expected 0 flows on template miss, got %d", len(flows))
	}
}

func TestParseV9_TwoTemplatesSameFlowset(t *testing.T) {
	cache := NewTemplateCache()
	exporter := netip.MustParseAddr("10.2.0.11")

	tpl := makeV9TemplateFlowSet(
		v9Tpl{id: 256, fields: []v9Field{{fieldProtocol, 1}, {fieldInBytes, 4}}},
		v9Tpl{id: 257, fields: []v9Field{{fieldProtocol, 1}, {fieldInPackets, 4}}},
	)
	pkt := makeV9Header(1, 1000, 1_700_000_000, 1, 0)
	pkt = append(pkt, tpl...)
	if _, err := ParseV9OrIPFIX(cache, pkt, exporter, nil); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 2 {
		t.Errorf("cache = %d, want 2", cache.Len())
	}
}

func TestParseV9_DomainScope(t *testing.T) {
	// Same template id, two different observation domains, must not
	// collide in the cache.
	cache := NewTemplateCache()
	exporter := netip.MustParseAddr("10.2.0.11")

	mk := func(domain uint32) []byte {
		tpl := makeV9TemplateFlowSet(v9Tpl{id: 257, fields: []v9Field{{fieldProtocol, 1}}})
		p := makeV9Header(1, 1000, 1_700_000_000, 1, domain)
		return append(p, tpl...)
	}
	if _, err := ParseV9OrIPFIX(cache, mk(0), exporter, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseV9OrIPFIX(cache, mk(1), exporter, nil); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 2 {
		t.Errorf("cache = %d, want 2 (one per domain)", cache.Len())
	}
}

// ---------- IPFIX ----------

func makeIPFIXHeader(setsLen int, exportTime, sequence, domain uint32) []byte {
	h := []byte{}
	h = u16(h, 10)
	h = u16(h, uint16(ipfixHeaderLenBytes+setsLen)) // total length
	h = u32(h, exportTime)
	h = u32(h, sequence)
	h = u32(h, domain)
	return h
}

func makeIPFIXTemplateSet(templates ...v9Tpl) []byte {
	body := []byte{}
	for _, t := range templates {
		body = u16(body, t.id)
		body = u16(body, uint16(len(t.fields)))
		for _, f := range t.fields {
			body = u16(body, f.t)
			body = u16(body, f.l)
		}
	}
	out := []byte{}
	out = u16(out, 2) // set_id = template
	out = u16(out, uint16(4+len(body)))
	out = append(out, body...)
	return out
}

func TestParseIPFIX_TemplateAndData(t *testing.T) {
	cache := NewTemplateCache()
	exporter := netip.MustParseAddr("10.2.0.11")

	tpl := makeIPFIXTemplateSet(v9Tpl{
		id: 256,
		fields: []v9Field{
			{fieldInBytes, 8},
			{fieldInPackets, 8},
			{fieldProtocol, 1},
			{fieldL4SrcPort, 2},
			{fieldL4DstPort, 2},
			{fieldIPv4SrcAddr, 4},
			{fieldIPv4DstAddr, 4},
			{fieldInputSnmp, 4},
			{fieldOutputSnmp, 4},
		},
	})
	rec := []byte{}
	var bytesV uint64 = 14_240
	var packetsV uint64 = 24
	rec = appendU64BE(rec, bytesV)
	rec = appendU64BE(rec, packetsV)
	rec = append(rec, 6)
	rec = u16(rec, 51422)
	rec = u16(rec, 443)
	rec = append(rec, 10, 4, 7, 21)
	rec = append(rec, 10, 8, 4, 130)
	rec = u32(rec, 2)
	rec = u32(rec, 4)
	data := makeDataFlowSet(256, rec)

	sets := append([]byte{}, tpl...)
	sets = append(sets, data...)
	pkt := makeIPFIXHeader(len(sets), 1_700_000_000, 1, 0)
	pkt = append(pkt, sets...)

	flows, err := ParseV9OrIPFIX(cache, pkt, exporter, nil)
	if err != nil {
		t.Fatalf("ParseV9OrIPFIX: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("len = %d", len(flows))
	}
	f := flows[0]
	if f.Source != record.SourceIPFIX {
		t.Errorf("Source = %v, want IPFIX", f.Source)
	}
	if f.Bytes != 14_240 || f.Packets != 24 {
		t.Errorf("metrics: bytes=%d packets=%d", f.Bytes, f.Packets)
	}
	if f.SrcAddr.String() != "10.4.7.21" {
		t.Errorf("SrcAddr = %v", f.SrcAddr)
	}
}

func TestParseIPFIX_BadVersionDelegatesToV9(t *testing.T) {
	cache := NewTemplateCache()
	pkt := []byte{0, 8, 0, 0} // version 8
	if _, err := ParseV9OrIPFIX(cache, pkt, netip.MustParseAddr("1.2.3.4"), nil); err != ErrBadVersion {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func TestParseV9OrIPFIX_ShortPacket(t *testing.T) {
	cache := NewTemplateCache()
	if _, err := ParseV9OrIPFIX(cache, []byte{0, 9}, netip.MustParseAddr("1.2.3.4"), nil); err != ErrShortPacket {
		t.Errorf("err = %v, want ErrShortPacket", err)
	}
}

func TestReadUintBE_Lengths(t *testing.T) {
	if got := readUintBE([]byte{0xAB}); got != 0xAB {
		t.Errorf("u8 = %x", got)
	}
	if got := readUintBE([]byte{0xAB, 0xCD}); got != 0xABCD {
		t.Errorf("u16 = %x", got)
	}
	if got := readUintBE([]byte{0, 0, 0xAB, 0xCD}); got != 0xABCD {
		t.Errorf("u32 padded = %x", got)
	}
	if got := readUintBE([]byte{0, 0, 0, 0, 0, 0, 0xAB, 0xCD}); got != 0xABCD {
		t.Errorf("u64 padded = %x", got)
	}
	// 3-byte (non-power-of-two): falls through to manual accumulation.
	if got := readUintBE([]byte{0xAB, 0xCD, 0xEF}); got != 0xABCDEF {
		t.Errorf("u24 = %x", got)
	}
}

func appendU64BE(b []byte, v uint64) []byte {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	return append(b, x[:]...)
}

// ---------- Variable-length IPFIX records (RFC 7011 §7) ----------

// TestDecodeDataRecords_VarShort exercises a record whose template has
// a single variable-length field encoded with the short (1-byte length)
// prefix, followed by a fixed-length protocol byte. The variable-length
// field carries an unrecognized field ID so the decoder consumes its
// bytes and moves on; the protocol field validates that the walker
// landed on the correct offset.
func TestDecodeDataRecords_VarShort(t *testing.T) {
	template := []TemplateField{
		{Type: 82, Length: ipfixVariableLength}, // interfaceName, ignored
		{Type: fieldProtocol, Length: 1},
	}
	body := []byte{}
	name := []byte("eth0")          // 4 bytes
	body = append(body, byte(len(name)))
	body = append(body, name...)
	body = append(body, 6) // protocol = TCP

	flows := decodeDataRecords(template, body, netip.MustParseAddr("10.0.0.1"), nil, decoderContext{isIPFIX: true})
	if len(flows) != 1 {
		t.Fatalf("len(flows) = %d, want 1", len(flows))
	}
	if flows[0].Proto != 6 {
		t.Errorf("Proto = %d, want 6 (variable-length walker mis-aligned)", flows[0].Proto)
	}
}

// TestDecodeDataRecords_VarLong covers the 3-byte length form (255
// sentinel + uint16 length) used when a variable-length field is ≥255
// bytes. We use a 300-byte payload, then a fixed protocol byte.
func TestDecodeDataRecords_VarLong(t *testing.T) {
	template := []TemplateField{
		{Type: 82, Length: ipfixVariableLength},
		{Type: fieldProtocol, Length: 1},
	}
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i)
	}
	body := []byte{}
	body = append(body, 255) // long-length sentinel
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(payload)))
	body = append(body, lenBuf[:]...)
	body = append(body, payload...)
	body = append(body, 17) // protocol = UDP

	flows := decodeDataRecords(template, body, netip.MustParseAddr("10.0.0.1"), nil, decoderContext{isIPFIX: true})
	if len(flows) != 1 {
		t.Fatalf("len(flows) = %d, want 1", len(flows))
	}
	if flows[0].Proto != 17 {
		t.Errorf("Proto = %d, want 17 (long-length prefix mis-decoded)", flows[0].Proto)
	}
}

// TestDecodeDataRecords_VarTruncated ensures that a length prefix
// claiming more bytes than are present abandons the record cleanly
// without panicking and without emitting a partial flow.
func TestDecodeDataRecords_VarTruncated(t *testing.T) {
	template := []TemplateField{
		{Type: 82, Length: ipfixVariableLength},
		{Type: fieldProtocol, Length: 1},
	}
	body := []byte{}
	body = append(body, 100)                  // claim 100 bytes
	body = append(body, make([]byte, 50)...)  // only deliver 50

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decodeDataRecords panicked on truncated record: %v", r)
		}
	}()
	flows := decodeDataRecords(template, body, netip.MustParseAddr("10.0.0.1"), nil, decoderContext{isIPFIX: true})
	if len(flows) != 0 {
		t.Errorf("len(flows) = %d, want 0 on truncated record", len(flows))
	}
}

// TestDecodeDataRecords_VarMultipleRecords verifies the adaptive
// walker correctly advances across record boundaries when each record
// has a different variable-length payload size.
func TestDecodeDataRecords_VarMultipleRecords(t *testing.T) {
	template := []TemplateField{
		{Type: 82, Length: ipfixVariableLength},
		{Type: fieldProtocol, Length: 1},
	}
	// Record 1: 3-byte name + proto 6
	body := []byte{3, 'e', 't', 'h', 6}
	// Record 2: 5-byte name + proto 17
	body = append(body, 5, 'e', 'n', 'p', '1', 's', 17)

	flows := decodeDataRecords(template, body, netip.MustParseAddr("10.0.0.1"), nil, decoderContext{isIPFIX: true})
	if len(flows) != 2 {
		t.Fatalf("len(flows) = %d, want 2", len(flows))
	}
	if flows[0].Proto != 6 || flows[1].Proto != 17 {
		t.Errorf("protos = %d,%d; want 6,17", flows[0].Proto, flows[1].Proto)
	}
}
