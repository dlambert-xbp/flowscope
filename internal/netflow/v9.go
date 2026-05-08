package netflow

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/record"
)

// NetFlow v9 / IPFIX (v10) share a template-driven encoding. v9 was
// defined by Cisco; IPFIX is the IETF descendant (RFC 7011) and uses
// the same field IDs for the basics we care about, plus enterprise
// fields and absolute-time fields.
//
// Header (v9, 20 bytes):
//
//	uint16 version          (9)
//	uint16 count            (FlowSet count, NOT record count — confusingly named)
//	uint32 sysUpTime        (ms since exporter boot)
//	uint32 unixSecs         (epoch seconds at packet emit)
//	uint32 sequence
//	uint32 sourceID         (observation domain in v9 parlance)
//
// Header (IPFIX, 16 bytes):
//
//	uint16 version          (10)
//	uint16 length           (TOTAL bytes in datagram, header included)
//	uint32 exportTime       (epoch seconds)
//	uint32 sequence
//	uint32 observationDomainID
//
// FlowSet / Set (after the header):
//
//	uint16 flowsetID
//	   0 (v9) / 2 (IPFIX) → Template Set
//	   1 (v9) / 3 (IPFIX) → Options Template Set
//	   ≥ 256              → Data Set (the value IS the template ID)
//	uint16 length          (bytes including the 4-byte header)

const (
	v9Version    = 9
	ipfixVersion = 10

	v9HeaderLenBytes    = 20
	ipfixHeaderLenBytes = 16

	flowsetTemplateV9        = 0
	flowsetOptionsTemplateV9 = 1
	flowsetTemplateIPFIX     = 2
	flowsetOptionsIPFIX      = 3

	dataFlowsetMin = 256

	// IPFIX enterprise-bit on field type.
	ipfixEnterpriseBit = 0x8000

	// Variable-length sentinel for IPFIX fields.
	ipfixVariableLength = 0xFFFF
)

// Field IDs we extract. Anything outside this set is skipped by its
// declared length. Field ID values are identical for v9 and IPFIX
// for the basics; absolute-time fields (152, 153) are IPFIX-only.
const (
	fieldInBytes        = 1
	fieldInPackets      = 2
	fieldProtocol       = 4
	fieldTOS            = 5
	fieldTCPFlags       = 6
	fieldL4SrcPort      = 7
	fieldIPv4SrcAddr    = 8
	fieldInputSnmp      = 10
	fieldL4DstPort      = 11
	fieldIPv4DstAddr    = 12
	fieldOutputSnmp     = 14
	fieldVlan           = 58
	fieldLastSwitched   = 21 // ms since boot (v9 + IPFIX)
	fieldFirstSwitched  = 22
	fieldIPv6SrcAddr    = 27
	fieldIPv6DstAddr    = 28
	fieldFlowEndMillis  = 153 // absolute ms since epoch (IPFIX)
	fieldOutBytes       = 23
	fieldOutPackets     = 24
)

// Sentinel errors for v9 / IPFIX parsing. Pre-existing errors from
// v5.go (ErrShortPacket, ErrTruncated) are reused for similar cases.
var (
	ErrV9BadHeader     = errors.New("netflow: v9/IPFIX bad header")
	ErrV9TemplateMiss  = errors.New("netflow: data flowset before template")
	ErrV9BadFlowsetLen = errors.New("netflow: invalid flowset length")
)

// TemplateField describes one field inside a template. Length is the
// declared field length in bytes; for IPFIX a length of 0xFFFF means
// variable-length (we currently skip records that hit one — TODO).
type TemplateField struct {
	Type       uint16
	Length     uint16
	Enterprise uint32 // IPFIX only; 0 if not enterprise-specific
}

// templateKey scopes a template to a single exporter + observation
// domain. The 5-tuple-hashed UDP load balancer (VISION.md §3.4) keeps
// an exporter's traffic on one ingest replica, so this in-process
// cache is sufficient for correctness.
type templateKey struct {
	exporter   netip.Addr
	domainID   uint32
	templateID uint16
}

// TemplateCache holds parsed templates per exporter+domain+id. Safe
// for concurrent calls from one or more parser goroutines.
type TemplateCache struct {
	mu        sync.RWMutex
	templates map[templateKey][]TemplateField
}

// NewTemplateCache returns an empty cache.
func NewTemplateCache() *TemplateCache {
	return &TemplateCache{templates: make(map[templateKey][]TemplateField)}
}

// Len returns the number of templates currently cached. Used by
// tests and the /metrics endpoint.
func (c *TemplateCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.templates)
}

func (c *TemplateCache) get(k templateKey) ([]TemplateField, bool) {
	c.mu.RLock()
	t, ok := c.templates[k]
	c.mu.RUnlock()
	if ok {
		obs.TemplateCacheHits.Inc()
	} else {
		obs.TemplateCacheMisses.Inc()
	}
	return t, ok
}

func (c *TemplateCache) put(k templateKey, fields []TemplateField) {
	c.mu.Lock()
	c.templates[k] = fields
	size := len(c.templates)
	c.mu.Unlock()
	obs.TemplateCacheSize.Set(float64(size))
}

// ParseV9OrIPFIX decodes a v9 or IPFIX datagram, dispatching on the
// version word. Discovered templates are cached; data flowsets are
// decoded against the cache and emitted Flow records are appended to
// dst. Data flowsets that hit a template miss are dropped silently
// (the caller may instrument a counter — exposed for that purpose
// via TemplateMissesObserved).
func ParseV9OrIPFIX(
	cache *TemplateCache,
	buf []byte,
	exporter netip.Addr,
	dst []record.Flow,
) ([]record.Flow, error) {
	if len(buf) < 4 {
		return dst, ErrShortPacket
	}
	version := binary.BigEndian.Uint16(buf[0:2])
	switch version {
	case v9Version:
		return parseV9(cache, buf, exporter, dst)
	case ipfixVersion:
		return parseIPFIX(cache, buf, exporter, dst)
	default:
		return dst, ErrBadVersion
	}
}

// ReadV9Sequence extracts the 32-bit datagram sequence number from
// a NetFlow v9 header. Returns (0, false) on a buffer too short to
// hold the header. Caller should run this before ParseV9OrIPFIX so
// a parser failure on the body still credits the datagram for
// loss-detection bookkeeping.
func ReadV9Sequence(buf []byte) (uint32, bool) {
	if len(buf) < v9HeaderLenBytes {
		return 0, false
	}
	if binary.BigEndian.Uint16(buf[0:2]) != v9Version {
		return 0, false
	}
	return binary.BigEndian.Uint32(buf[12:16]), true
}

// ReadIPFIXSequence extracts the 32-bit message sequence number
// from an IPFIX (v10) header. IPFIX defines sequence as the count
// of records the exporter has emitted so the increment is
// per-record-count, not per-datagram — see RFC 7011 §3.1.
func ReadIPFIXSequence(buf []byte) (uint32, bool) {
	if len(buf) < ipfixHeaderLenBytes {
		return 0, false
	}
	if binary.BigEndian.Uint16(buf[0:2]) != ipfixVersion {
		return 0, false
	}
	return binary.BigEndian.Uint32(buf[8:12]), true
}

// ---------- v9 ----------

func parseV9(cache *TemplateCache, buf []byte, exporter netip.Addr, dst []record.Flow) ([]record.Flow, error) {
	if len(buf) < v9HeaderLenBytes {
		return dst, ErrShortPacket
	}
	count := binary.BigEndian.Uint16(buf[2:4])
	sysUpTime := binary.BigEndian.Uint32(buf[4:8])
	unixSecs := binary.BigEndian.Uint32(buf[8:12])
	domainID := binary.BigEndian.Uint32(buf[16:20])

	// bootWall: wall-clock time of exporter boot. Per-record
	// LAST_SWITCHED (ms since boot) maps onto wall time via this base.
	bootWall := time.Unix(int64(unixSecs), 0).Add(-time.Duration(sysUpTime) * time.Millisecond)

	off := v9HeaderLenBytes
	flowSetIdx := uint16(0)
	for flowSetIdx < count && off+4 <= len(buf) {
		flowsetID := binary.BigEndian.Uint16(buf[off : off+2])
		flowsetLen := binary.BigEndian.Uint16(buf[off+2 : off+4])
		if flowsetLen < 4 {
			return dst, ErrV9BadFlowsetLen
		}
		end := off + int(flowsetLen)
		if end > len(buf) {
			return dst, ErrTruncated
		}
		body := buf[off+4 : end]
		off = end

		switch flowsetID {
		case flowsetTemplateV9:
			parseV9Templates(cache, exporter, domainID, body)
		case flowsetOptionsTemplateV9:
			// Options templates carry metadata (sampling rates, etc.)
			// that we don't yet act on. Skip the template body.
		default:
			if flowsetID >= dataFlowsetMin {
				key := templateKey{exporter: exporter, domainID: domainID, templateID: flowsetID}
				if t, ok := cache.get(key); ok {
					dst = decodeDataRecords(t, body, exporter, dst, decoderContext{
						bootWall:  bootWall,
						isIPFIX:   false,
						exportTime: time.Unix(int64(unixSecs), 0),
					})
				}
				// else: silent drop. Counter exposed via metrics layer.
			}
		}
		flowSetIdx++
	}
	return dst, nil
}

// parseV9Templates: a Template FlowSet body can hold multiple templates
// back-to-back. Each is `template_id (u16) | field_count (u16) | N×(u16,u16)`.
func parseV9Templates(cache *TemplateCache, exporter netip.Addr, domainID uint32, body []byte) {
	off := 0
	for off+4 <= len(body) {
		tplID := binary.BigEndian.Uint16(body[off : off+2])
		fieldCount := binary.BigEndian.Uint16(body[off+2 : off+4])
		off += 4
		if off+int(fieldCount)*4 > len(body) {
			return // truncated; what we have is invalid, abandon
		}
		fields := make([]TemplateField, 0, fieldCount)
		for i := uint16(0); i < fieldCount; i++ {
			fields = append(fields, TemplateField{
				Type:   binary.BigEndian.Uint16(body[off : off+2]),
				Length: binary.BigEndian.Uint16(body[off+2 : off+4]),
			})
			off += 4
		}
		cache.put(templateKey{exporter: exporter, domainID: domainID, templateID: tplID}, fields)
	}
}

// ---------- IPFIX ----------

func parseIPFIX(cache *TemplateCache, buf []byte, exporter netip.Addr, dst []record.Flow) ([]record.Flow, error) {
	if len(buf) < ipfixHeaderLenBytes {
		return dst, ErrShortPacket
	}
	totalLen := binary.BigEndian.Uint16(buf[2:4])
	exportTime := binary.BigEndian.Uint32(buf[4:8])
	domainID := binary.BigEndian.Uint32(buf[12:16])

	if int(totalLen) > len(buf) {
		return dst, ErrTruncated
	}
	if int(totalLen) < ipfixHeaderLenBytes {
		return dst, ErrV9BadHeader
	}

	off := ipfixHeaderLenBytes
	for off+4 <= int(totalLen) {
		setID := binary.BigEndian.Uint16(buf[off : off+2])
		setLen := binary.BigEndian.Uint16(buf[off+2 : off+4])
		if setLen < 4 {
			return dst, ErrV9BadFlowsetLen
		}
		end := off + int(setLen)
		if end > int(totalLen) {
			return dst, ErrTruncated
		}
		body := buf[off+4 : end]
		off = end

		switch setID {
		case flowsetTemplateIPFIX:
			parseIPFIXTemplates(cache, exporter, domainID, body)
		case flowsetOptionsIPFIX:
			// Options templates not yet acted on.
		default:
			if setID >= dataFlowsetMin {
				key := templateKey{exporter: exporter, domainID: domainID, templateID: setID}
				if t, ok := cache.get(key); ok {
					dst = decodeDataRecords(t, body, exporter, dst, decoderContext{
						isIPFIX:    true,
						exportTime: time.Unix(int64(exportTime), 0),
					})
				}
			}
		}
	}
	return dst, nil
}

// IPFIX templates differ from v9: a field type with the high bit set
// means an enterprise-specific field, with 4 extra bytes for the
// enterprise number after (type, length). We parse and skip enterprise
// fields by length but do not attempt to decode them.
func parseIPFIXTemplates(cache *TemplateCache, exporter netip.Addr, domainID uint32, body []byte) {
	off := 0
	for off+4 <= len(body) {
		tplID := binary.BigEndian.Uint16(body[off : off+2])
		fieldCount := binary.BigEndian.Uint16(body[off+2 : off+4])
		off += 4
		fields := make([]TemplateField, 0, fieldCount)
		ok := true
		for i := uint16(0); i < fieldCount; i++ {
			if off+4 > len(body) {
				ok = false
				break
			}
			ftype := binary.BigEndian.Uint16(body[off : off+2])
			flen := binary.BigEndian.Uint16(body[off+2 : off+4])
			off += 4
			ent := uint32(0)
			if ftype&ipfixEnterpriseBit != 0 {
				if off+4 > len(body) {
					ok = false
					break
				}
				ent = binary.BigEndian.Uint32(body[off : off+4])
				off += 4
				ftype &^= ipfixEnterpriseBit
			}
			fields = append(fields, TemplateField{Type: ftype, Length: flen, Enterprise: ent})
		}
		if !ok {
			return
		}
		cache.put(templateKey{exporter: exporter, domainID: domainID, templateID: tplID}, fields)
	}
}

// ---------- Data record decode ----------

type decoderContext struct {
	bootWall   time.Time // v9 only
	isIPFIX    bool
	exportTime time.Time
}

// decodeDataRecords walks a data flowset body, producing one Flow per
// template-shaped record. Records hitting a variable-length IPFIX
// field abandon the rest of the body (TODO: full variable-length
// support).
func decodeDataRecords(
	template []TemplateField,
	body []byte,
	exporter netip.Addr,
	dst []record.Flow,
	ctx decoderContext,
) []record.Flow {
	recordLen := 0
	hasVar := false
	for _, f := range template {
		if f.Length == ipfixVariableLength {
			hasVar = true
			break
		}
		recordLen += int(f.Length)
	}
	if hasVar || recordLen == 0 {
		return dst // skip variable-length records for now
	}
	for off := 0; off+recordLen <= len(body); off += recordLen {
		if f, ok := decodeOneRecord(template, body[off:off+recordLen], exporter, ctx); ok {
			dst = append(dst, f)
		}
	}
	return dst
}

// decodeOneRecord walks one fixed-size record's bytes and pulls out
// the canonical fields we care about. Unknown / unhandled fields are
// skipped by their declared length.
func decodeOneRecord(template []TemplateField, rec []byte, exporter netip.Addr, ctx decoderContext) (record.Flow, bool) {
	out := record.Flow{
		Exporter: exporter,
		Source:   record.SourceNetFlowV9, // overwritten for IPFIX below
	}
	if ctx.isIPFIX {
		out.Source = record.SourceIPFIX
		out.Observed = ctx.exportTime
	}

	off := 0
	var lastSwitchedMs uint32
	var flowEndMs uint64
	haveLastSwitched := false
	haveFlowEnd := false

	for _, f := range template {
		if off+int(f.Length) > len(rec) {
			return out, false
		}
		val := rec[off : off+int(f.Length)]
		off += int(f.Length)

		// Enterprise fields are skipped (we already advanced `off`).
		if f.Enterprise != 0 {
			continue
		}
		switch f.Type {
		case fieldInBytes:
			out.Bytes = readUintBE(val)
		case fieldInPackets:
			out.Packets = readUintBE(val)
		case fieldOutBytes:
			if out.Bytes == 0 {
				out.Bytes = readUintBE(val)
			}
		case fieldOutPackets:
			if out.Packets == 0 {
				out.Packets = readUintBE(val)
			}
		case fieldProtocol:
			if len(val) >= 1 {
				out.Proto = val[0]
			}
		case fieldTOS:
			if len(val) >= 1 {
				out.Tos = val[0]
			}
		case fieldTCPFlags:
			if len(val) >= 1 {
				out.TCPFlags = val[0]
			}
		case fieldL4SrcPort:
			out.SrcPort = uint16(readUintBE(val))
		case fieldL4DstPort:
			out.DstPort = uint16(readUintBE(val))
		case fieldIPv4SrcAddr:
			if len(val) == 4 {
				out.SrcAddr = netip.AddrFrom4([4]byte{val[0], val[1], val[2], val[3]})
			}
		case fieldIPv4DstAddr:
			if len(val) == 4 {
				out.DstAddr = netip.AddrFrom4([4]byte{val[0], val[1], val[2], val[3]})
			}
		case fieldIPv6SrcAddr:
			if len(val) == 16 {
				var b16 [16]byte
				copy(b16[:], val)
				out.SrcAddr = netip.AddrFrom16(b16)
			}
		case fieldIPv6DstAddr:
			if len(val) == 16 {
				var b16 [16]byte
				copy(b16[:], val)
				out.DstAddr = netip.AddrFrom16(b16)
			}
		case fieldInputSnmp:
			out.InputIfIndex = uint32(readUintBE(val))
		case fieldOutputSnmp:
			out.OutputIfIndex = uint32(readUintBE(val))
		case fieldVlan:
			out.VlanID = uint16(readUintBE(val))
		case fieldLastSwitched:
			lastSwitchedMs = uint32(readUintBE(val))
			haveLastSwitched = true
		case fieldFlowEndMillis:
			flowEndMs = readUintBE(val)
			haveFlowEnd = true
		}
	}

	// Compute Observed:
	//  - IPFIX with absolute flowEndMillis → use it directly.
	//  - v9 with LAST_SWITCHED (ms since boot) → bootWall + ms.
	//  - Otherwise leave whatever default the caller stamped.
	if haveFlowEnd {
		out.Observed = time.UnixMilli(int64(flowEndMs)).UTC()
	} else if haveLastSwitched && !ctx.isIPFIX {
		out.Observed = ctx.bootWall.Add(time.Duration(lastSwitchedMs) * time.Millisecond)
	} else if out.Observed.IsZero() {
		out.Observed = ctx.exportTime
	}
	return out, true
}

// readUintBE returns a big-endian unsigned integer of length 1, 2, 4,
// or 8. Lengths outside that set fall back to the largest power-of-two
// prefix the value supports.
func readUintBE(b []byte) uint64 {
	switch len(b) {
	case 1:
		return uint64(b[0])
	case 2:
		return uint64(binary.BigEndian.Uint16(b))
	case 4:
		return uint64(binary.BigEndian.Uint32(b))
	case 8:
		return binary.BigEndian.Uint64(b)
	default:
		// Pad-left to nearest power of two and recurse.
		var v uint64
		for _, x := range b {
			v = (v << 8) | uint64(x)
		}
		return v
	}
}
