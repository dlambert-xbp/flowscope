// Command synth generates synthetic NetFlow v5 traffic for development
// and load testing. It replaces the Python-era synth_flows.py.
//
//	go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 5000 --duration 30s
//
// The generator produces realistic-looking flows: a small pool of
// exporters and endpoints, common service ports, plausible byte and
// packet counts. It is good enough to exercise the end-to-end pipeline
// (parser → emitter → ring + ClickHouse) and to drive integration
// tests; it is not a fuzzing tool.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"
)

const (
	v5HeaderLen = 24
	v5RecordLen = 48
)

// A small static endpoint pool keeps generated flows correlatable —
// "10.4.7.21" reappearing as a top talker is useful for visual
// validation in the dashboard.
var (
	exporters = []netip.Addr{
		netip.MustParseAddr("10.2.0.11"),
		netip.MustParseAddr("10.2.0.12"),
		netip.MustParseAddr("10.2.4.1"),
	}
	clients = []netip.Addr{
		netip.MustParseAddr("10.4.7.21"),
		netip.MustParseAddr("10.0.2.4"),
		netip.MustParseAddr("172.16.4.9"),
		netip.MustParseAddr("10.4.7.22"),
		netip.MustParseAddr("10.4.7.23"),
	}
	servers = []netip.Addr{
		netip.MustParseAddr("10.8.4.130"),
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("13.107.42.14"),
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("192.168.7.40"),
	}
	services = []struct {
		port  uint16
		proto uint8 // 6=TCP, 17=UDP
	}{
		{443, 6},
		{80, 6},
		{53, 17},
		{22, 6},
		{445, 6},
		{3389, 6},
	}
)

func main() {
	target := flag.String("target", "127.0.0.1:2055", "destination host:port for NetFlow v5 datagrams")
	rate := flag.Int("rate", 1000, "flows per second to generate")
	duration := flag.Duration("duration", 60*time.Second, "how long to run; 0 = forever")
	recsPerPkt := flag.Int("records-per-packet", 25, "flow records per UDP datagram (1..30)")
	flag.Parse()

	if *recsPerPkt < 1 || *recsPerPkt > 30 {
		log.Fatalf("--records-per-packet must be 1..30, got %d", *recsPerPkt)
	}
	if *rate <= 0 {
		log.Fatal("--rate must be positive")
	}

	addr, err := net.ResolveUDPAddr("udp", *target)
	if err != nil {
		log.Fatalf("resolve %s: %v", *target, err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Fatalf("dial %s: %v", *target, err)
	}
	defer conn.Close()

	// Compute pacing. Each datagram carries `recsPerPkt` flows; we
	// emit (rate / recsPerPkt) datagrams per second.
	pktsPerSec := *rate / *recsPerPkt
	if pktsPerSec < 1 {
		pktsPerSec = 1
	}
	tick := time.Second / time.Duration(pktsPerSec)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	deadline := time.Time{}
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
	}

	bootTime := time.Now().Add(-30 * time.Minute)
	flowSeq := uint32(1)
	var totalRecords int

	pkt := make([]byte, v5HeaderLen+(*recsPerPkt)*v5RecordLen)
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))

	log.Printf("synth: emitting %d flows/s in %d-record packets to %s", *rate, *recsPerPkt, *target)
	for {
		select {
		case <-ticker.C:
			if !deadline.IsZero() && time.Now().After(deadline) {
				log.Printf("synth: done — emitted %d flow records", totalRecords)
				return
			}
			now := time.Now()
			fillV5Packet(pkt, *recsPerPkt, bootTime, now, flowSeq, rng)
			if _, err := conn.Write(pkt); err != nil {
				log.Printf("synth: write: %v", err)
			}
			flowSeq += uint32(*recsPerPkt)
			totalRecords += *recsPerPkt
		}
	}
}

// fillV5Packet writes a complete NetFlow v5 datagram into buf.
func fillV5Packet(buf []byte, count int, bootTime, now time.Time, seq uint32, rng *rand.Rand) {
	binary.BigEndian.PutUint16(buf[0:2], 5)
	binary.BigEndian.PutUint16(buf[2:4], uint16(count))
	sysUpTime := uint32(now.Sub(bootTime).Milliseconds())
	binary.BigEndian.PutUint32(buf[4:8], sysUpTime)
	binary.BigEndian.PutUint32(buf[8:12], uint32(now.Unix()))
	binary.BigEndian.PutUint32(buf[12:16], uint32(now.Nanosecond()))
	binary.BigEndian.PutUint32(buf[16:20], seq)

	off := v5HeaderLen
	for i := 0; i < count; i++ {
		writeRecord(buf[off:off+v5RecordLen], sysUpTime, rng)
		off += v5RecordLen
	}
}

// writeRecord encodes a single 48-byte NetFlow v5 record.
func writeRecord(rec []byte, sysUpTime uint32, rng *rand.Rand) {
	src := pick(rng, clients)
	dst := pick(rng, servers)
	svc := services[rng.IntN(len(services))]

	// 80% of flows go client→server, 20% the response direction.
	if rng.IntN(5) == 0 {
		src, dst = dst, src
	}
	srcPort := uint16(32768 + rng.IntN(32767))
	dstPort := svc.port
	proto := svc.proto
	tcpFlags := byte(0)
	if proto == 6 {
		tcpFlags = 0x18 // PSH|ACK — typical mid-flow
	}

	// Plausible volumes: small majority sub-MTU, occasional bursts.
	packets := uint32(1 + rng.IntN(64))
	bytes := packets * uint32(64+rng.IntN(1400))

	s4 := src.As4()
	d4 := dst.As4()
	copy(rec[0:4], s4[:])
	copy(rec[4:8], d4[:])
	// nextHop unused in our schema but required by layout
	binary.BigEndian.PutUint16(rec[12:14], uint16(2+rng.IntN(8))) // input ifindex
	binary.BigEndian.PutUint16(rec[14:16], uint16(2+rng.IntN(8))) // output ifindex
	binary.BigEndian.PutUint32(rec[16:20], packets)
	binary.BigEndian.PutUint32(rec[20:24], bytes)
	// firstSwitched = sysUpTime - small jitter, lastSwitched = sysUpTime
	first := sysUpTime
	if first > 5000 {
		first = sysUpTime - uint32(rng.IntN(5000))
	}
	binary.BigEndian.PutUint32(rec[24:28], first)
	binary.BigEndian.PutUint32(rec[28:32], sysUpTime)
	binary.BigEndian.PutUint16(rec[32:34], srcPort)
	binary.BigEndian.PutUint16(rec[34:36], dstPort)
	rec[37] = tcpFlags
	rec[38] = proto
	rec[39] = 0 // tos
}

func pick(rng *rand.Rand, pool []netip.Addr) netip.Addr {
	return pool[rng.IntN(len(pool))]
}

// keep the imported fmt referenced; used by future verbose logging
var _ = fmt.Sprintf
