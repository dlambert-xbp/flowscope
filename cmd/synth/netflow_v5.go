package main

import (
	"encoding/binary"
	"log"
	"math/rand/v2"
	"time"
)

const (
	v5HeaderLen = 24
	v5RecordLen = 48
)

// runV5 emits NetFlow v5 datagrams to target at the configured rate.
// Each datagram carries recsPerPkt records, so packet rate is
// rate / recsPerPkt. Returns when deadline is reached or forever if
// deadline is zero.
func runV5(target string, rate, recsPerPkt int, deadline time.Time) {
	conn, err := dial(target)
	if err != nil {
		log.Fatalf("synth.v5: dial %s: %v", target, err)
	}
	defer conn.Close()

	pktsPerSec := rate / recsPerPkt
	if pktsPerSec < 1 {
		pktsPerSec = 1
	}
	tick := time.Second / time.Duration(pktsPerSec)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	bootTime := time.Now().Add(-30 * time.Minute)
	flowSeq := uint32(1)
	var totalRecords int

	pkt := make([]byte, v5HeaderLen+recsPerPkt*v5RecordLen)
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))

	log.Printf("synth.v5: emitting %d flows/s in %d-record packets to %s", rate, recsPerPkt, target)
	for range ticker.C {
		if !deadline.IsZero() && time.Now().After(deadline) {
			log.Printf("synth.v5: done — emitted %d records", totalRecords)
			return
		}
		now := time.Now()
		fillV5Packet(pkt, recsPerPkt, bootTime, now, flowSeq, rng)
		if _, err := conn.Write(pkt); err != nil {
			log.Printf("synth.v5: write: %v", err)
		}
		flowSeq += uint32(recsPerPkt)
		totalRecords += recsPerPkt
	}
}

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
		writeV5Record(buf[off:off+v5RecordLen], sysUpTime, rng)
		off += v5RecordLen
	}
}

func writeV5Record(rec []byte, sysUpTime uint32, rng *rand.Rand) {
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

	packets := uint32(1 + rng.IntN(64))
	bytes := packets * uint32(64+rng.IntN(1400))

	s4 := src.As4()
	d4 := dst.As4()
	copy(rec[0:4], s4[:])
	copy(rec[4:8], d4[:])
	binary.BigEndian.PutUint16(rec[12:14], uint16(2+rng.IntN(8)))
	binary.BigEndian.PutUint16(rec[14:16], uint16(2+rng.IntN(8)))
	binary.BigEndian.PutUint32(rec[16:20], packets)
	binary.BigEndian.PutUint32(rec[20:24], bytes)
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
	rec[39] = 0
}
