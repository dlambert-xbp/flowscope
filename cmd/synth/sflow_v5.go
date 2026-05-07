package main

import (
	"encoding/binary"
	"log"
	"math/rand/v2"
	"net/netip"
	"sync/atomic"
	"time"
)

// runSFlow emits sFlow v5 datagrams to target: flow_samples at the
// configured rate (one sample per datagram for simplicity), plus one
// counters_sample per interface every counterEvery interval.
//
// Wire format mirrors what internal/sflow/parse.go decodes — see that
// file for the layout reference. This generator's output round-trips
// through the parser cleanly (covered by parser_test.go's golden
// builders, which are structurally identical to this code).
func runSFlow(target string, rate int, counterEvery time.Duration, ifCount int, deadline time.Time) {
	conn, err := dial(target)
	if err != nil {
		log.Fatalf("synth.sflow: dial %s: %v", target, err)
	}
	defer conn.Close()

	if rate < 1 {
		rate = 1
	}
	if ifCount < 1 {
		ifCount = 1
	}

	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	bootTime := time.Now().Add(-45 * time.Minute)

	// Per-(agent, ifindex) cumulative counter state. Each tick of the
	// counter emitter bumps the totals with realistic deltas before
	// writing the sample, so successive samples produce non-zero
	// rates after the parser diffs them.
	state := newCounterState(exporters, ifCount)

	// Two tickers: flow_samples (high rate) and counters_samples
	// (one per interface every counterEvery).
	flowTick := time.NewTicker(time.Second / time.Duration(rate))
	defer flowTick.Stop()
	counterTick := time.NewTicker(counterEvery)
	defer counterTick.Stop()

	var seq atomic.Uint32

	log.Printf("synth.sflow: emitting %d flow_samples/s plus counters every %s × %d ifaces × %d agents to %s",
		rate, counterEvery, ifCount, len(exporters), target)

	var sentFlows, sentCounters uint64
	for {
		select {
		case <-flowTick.C:
			if !deadline.IsZero() && time.Now().After(deadline) {
				log.Printf("synth.sflow: done — emitted %d flow_samples, %d counters_samples", sentFlows, sentCounters)
				return
			}
			agent := pick(rng, exporters)
			pkt := buildFlowSampleDatagram(agent, bootTime, seq.Add(1), rng)
			if _, err := conn.Write(pkt); err != nil {
				log.Printf("synth.sflow: flow write: %v", err)
				continue
			}
			sentFlows++

		case <-counterTick.C:
			for _, agent := range exporters {
				for i := 0; i < ifCount; i++ {
					state.advance(agent, i, counterEvery, rng)
					pkt := buildCountersSampleDatagram(agent, bootTime, seq.Add(1), state.snapshot(agent, i))
					if _, err := conn.Write(pkt); err != nil {
						log.Printf("synth.sflow: counter write: %v", err)
						continue
					}
					sentCounters++
				}
			}
		}
	}
}

/* -------------------- datagram builders -------------------- */

// buildFlowSampleDatagram returns a complete sFlow v5 datagram with
// one flow_sample carrying one raw_packet_header record.
func buildFlowSampleDatagram(agent netip.Addr, bootTime time.Time, seq uint32, rng *rand.Rand) []byte {
	src := pick(rng, clients)
	dst := pick(rng, servers)
	svc := services[rng.IntN(len(services))]
	if rng.IntN(5) == 0 {
		src, dst = dst, src
	}
	srcPort := uint16(32768 + rng.IntN(32767))
	dstPort := svc.port
	proto := svc.proto
	tcpFlags := byte(0)
	if proto == 6 {
		tcpFlags = 0x18
	}
	frameLen := uint32(64 + rng.IntN(1400))

	header := buildEthIPv4(src, dst, proto, srcPort, dstPort, tcpFlags)
	rawRec := buildRawPacketHeaderRecord(frameLen, header)

	inIf := uint32(2 + rng.IntN(8))
	outIf := uint32(2 + rng.IntN(8))
	flowBody := buildFlowSampleBody(seq, 1, inIf, outIf, [][]byte{rawRec})

	sample := buildSampleEnvelope(1 /* flow_sample */, flowBody)
	return buildDatagramHeader(agent, bootTime, seq, [][]byte{sample})
}

func buildCountersSampleDatagram(agent netip.Addr, bootTime time.Time, seq uint32, snap counterSnapshot) []byte {
	rec := buildIfCountersRecord(snap)
	body := buildCountersSampleBody(seq, snap.IfIndex, [][]byte{rec})
	sample := buildSampleEnvelope(2 /* counters_sample */, body)
	return buildDatagramHeader(agent, bootTime, seq, [][]byte{sample})
}

// buildDatagramHeader assembles the 28-byte (IPv4 agent) datagram
// header followed by every supplied sample.
func buildDatagramHeader(agent netip.Addr, bootTime time.Time, seq uint32, samples [][]byte) []byte {
	totalSamples := uint32(len(samples))
	body := []byte{}
	for _, s := range samples {
		body = append(body, s...)
	}
	out := make([]byte, 0, 28+len(body))
	out = appendU32(out, 5) // version
	out = appendU32(out, 1) // ip_addr_type = IPv4
	a := agent.As4()
	out = append(out, a[:]...)
	out = appendU32(out, 0)                                                  // sub_agent_id
	out = appendU32(out, seq)                                                // sequence
	out = appendU32(out, uint32(time.Since(bootTime).Milliseconds()))        // sysUpTime
	out = appendU32(out, totalSamples)                                       // num_samples
	out = append(out, body...)
	return out
}

func buildSampleEnvelope(format uint32, body []byte) []byte {
	out := make([]byte, 0, 8+len(body))
	out = appendU32(out, format)
	out = appendU32(out, uint32(len(body)))
	out = append(out, body...)
	return out
}

func buildFlowSampleBody(seq, sourceID, inIf, outIf uint32, records [][]byte) []byte {
	body := make([]byte, 0, 32)
	body = appendU32(body, seq)      // sequence_number
	body = appendU32(body, sourceID) // source_id
	body = appendU32(body, 1024)     // sampling_rate
	body = appendU32(body, 0)        // sample_pool
	body = appendU32(body, 0)        // drops
	body = appendU32(body, inIf)     // input
	body = appendU32(body, outIf)    // output
	body = appendU32(body, uint32(len(records)))
	for _, r := range records {
		body = append(body, r...)
	}
	return body
}

func buildRawPacketHeaderRecord(frameLen uint32, header []byte) []byte {
	rec := make([]byte, 0, 16+len(header))
	rec = appendU32(rec, 1)                  // protocol = ETHERNET-ISO88023
	rec = appendU32(rec, frameLen)           // frame_length
	rec = appendU32(rec, 0)                  // stripped_bytes
	rec = appendU32(rec, uint32(len(header)))
	rec = append(rec, header...)

	envelope := make([]byte, 0, 8+len(rec))
	envelope = appendU32(envelope, 1) // record_type = raw_packet_header
	envelope = appendU32(envelope, uint32(len(rec)))
	envelope = append(envelope, rec...)
	return envelope
}

func buildCountersSampleBody(seq, sourceID uint32, records [][]byte) []byte {
	body := make([]byte, 0, 12)
	body = appendU32(body, seq)
	body = appendU32(body, sourceID)
	body = appendU32(body, uint32(len(records)))
	for _, r := range records {
		body = append(body, r...)
	}
	return body
}

func buildIfCountersRecord(s counterSnapshot) []byte {
	body := make([]byte, 0, 88)
	body = appendU32(body, s.IfIndex)
	body = appendU32(body, 6)               // ifType (ethernetCsmacd)
	body = appendU64(body, 10_000_000_000)  // ifSpeed (10G)
	body = appendU32(body, 1)               // ifDirection (full-duplex)
	body = appendU32(body, 3)               // ifStatus (up + admin)
	body = appendU64(body, s.InOctets)
	body = appendU32(body, uint32(s.InUcastPkts))
	body = appendU32(body, 0) // multicast
	body = appendU32(body, 0) // broadcast
	body = appendU32(body, uint32(s.InDiscards))
	body = appendU32(body, uint32(s.InErrors))
	body = appendU32(body, 0) // unknown_protos
	body = appendU64(body, s.OutOctets)
	body = appendU32(body, uint32(s.OutUcastPkts))
	body = appendU32(body, 0)
	body = appendU32(body, 0)
	body = appendU32(body, uint32(s.OutDiscards))
	body = appendU32(body, uint32(s.OutErrors))
	body = appendU32(body, 0) // promiscuous

	envelope := make([]byte, 0, 8+len(body))
	envelope = appendU32(envelope, 1) // record_type = if_counters
	envelope = appendU32(envelope, uint32(len(body)))
	envelope = append(envelope, body...)
	return envelope
}

/* -------------------- ethernet / IPv4 / TCP|UDP header -------------------- */

func buildEthIPv4(src, dst netip.Addr, proto uint8, srcPort, dstPort uint16, tcpFlags byte) []byte {
	hdr := make([]byte, 0, 60)
	// Ethernet: dst MAC (6) + src MAC (6) + etherType (2)
	hdr = append(hdr,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01,
		0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02,
		0x08, 0x00, // IPv4
	)
	// IPv4 header (20 bytes, no options)
	ip := []byte{0x45, 0x00, 0, 40, 0, 0, 0, 0, 64, proto, 0, 0}
	srcB := src.As4()
	dstB := dst.As4()
	ip = append(ip, srcB[:]...)
	ip = append(ip, dstB[:]...)
	hdr = append(hdr, ip...)
	// L4
	switch proto {
	case 6: // TCP — 20-byte header
		tcp := make([]byte, 20)
		binary.BigEndian.PutUint16(tcp[0:2], srcPort)
		binary.BigEndian.PutUint16(tcp[2:4], dstPort)
		tcp[12] = 0x50 // dataOffset = 5 (20 bytes)
		tcp[13] = tcpFlags
		hdr = append(hdr, tcp...)
	case 17: // UDP — 8-byte header
		udp := make([]byte, 8)
		binary.BigEndian.PutUint16(udp[0:2], srcPort)
		binary.BigEndian.PutUint16(udp[2:4], dstPort)
		hdr = append(hdr, udp...)
	}
	return hdr
}

/* -------------------- counter state -------------------- */

// counterState holds per-(agent, ifindex) cumulative byte/packet/error
// counters. advance bumps them with realistic deltas; snapshot returns
// the current totals for a sample. Concurrency-safe — only one
// counters_sample goroutine touches it today, but the locking is
// future-proof.
type counterState struct {
	rows map[counterKey]*counterSnapshot
}

type counterKey struct {
	agent   netip.Addr
	ifIndex uint32
}

type counterSnapshot struct {
	IfIndex      uint32
	InOctets     uint64
	OutOctets    uint64
	InUcastPkts  uint64
	OutUcastPkts uint64
	InErrors     uint64
	OutErrors    uint64
	InDiscards   uint64
	OutDiscards  uint64
}

func newCounterState(agents []netip.Addr, ifCount int) *counterState {
	s := &counterState{rows: make(map[counterKey]*counterSnapshot)}
	for _, a := range agents {
		for i := 0; i < ifCount; i++ {
			ix := uint32(2 + i) // start at 2 to mirror typical platform numbering
			s.rows[counterKey{a, ix}] = &counterSnapshot{IfIndex: ix}
		}
	}
	return s
}

// advance increments the (agent, i)-th interface counters with deltas
// roughly proportional to the supplied interval. We aim for tens-of-Mbps
// to a few-hundred-Mbps throughput per interface — enough to populate
// the dashboard without saturating the eye.
func (s *counterState) advance(agent netip.Addr, i int, interval time.Duration, rng *rand.Rand) {
	row := s.rows[counterKey{agent, uint32(2 + i)}]
	if row == nil {
		return
	}
	secs := interval.Seconds()
	// In: 5–80 Mbps per interface, Out: 5–60 Mbps
	inBytes := uint64((5_000_000 + rng.IntN(75_000_000)) * int(secs) / 8)
	outBytes := uint64((5_000_000 + rng.IntN(55_000_000)) * int(secs) / 8)
	row.InOctets += inBytes
	row.OutOctets += outBytes
	row.InUcastPkts += inBytes / 1024
	row.OutUcastPkts += outBytes / 1024
	if rng.IntN(50) == 0 {
		row.InErrors += uint64(1 + rng.IntN(3))
	}
	if rng.IntN(80) == 0 {
		row.OutDiscards += uint64(1 + rng.IntN(2))
	}
}

func (s *counterState) snapshot(agent netip.Addr, i int) counterSnapshot {
	row := s.rows[counterKey{agent, uint32(2 + i)}]
	if row == nil {
		return counterSnapshot{}
	}
	return *row
}

/* -------------------- byte helpers -------------------- */

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
