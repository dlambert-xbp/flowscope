// Command synth generates synthetic NetFlow v5 and sFlow v5 traffic
// for development and load testing. It replaces the Python-era
// synth_flows.py and now also exercises the sFlow ingest path.
//
//	# NetFlow v5 only
//	go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 5000 --duration 30s
//
//	# sFlow v5 only (flow + counter samples)
//	go run ./cmd/synth -- --rate 0 --sflow-target 127.0.0.1:6343 --sflow-rate 2000
//
//	# Both, in parallel
//	go run ./cmd/synth -- --target 127.0.0.1:2055 --rate 3000 \
//	                     --sflow-target 127.0.0.1:6343 --sflow-rate 2000
//
// The generator produces realistic-looking flows: a small pool of
// exporters and endpoints, common service ports, plausible byte and
// packet counts. It exercises the end-to-end pipeline (parser →
// emitter → ring + ClickHouse) for QA. It is not a fuzzing tool.
package main

import (
	"flag"
	"log"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
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
	target := flag.String("target", "127.0.0.1:2055", "destination host:port for NetFlow v5 datagrams (set --rate 0 to disable)")
	rate := flag.Int("rate", 1000, "NetFlow v5 flows/sec to generate; 0 disables")
	recsPerPkt := flag.Int("records-per-packet", 25, "NetFlow v5 records per UDP datagram (1..30)")

	sflowTarget := flag.String("sflow-target", "", "destination host:port for sFlow v5 datagrams (empty disables)")
	sflowRate := flag.Int("sflow-rate", 1000, "sFlow flow_samples/sec to generate")
	sflowCounterEvery := flag.Duration("sflow-counter-interval", 5*time.Second, "emit one counters_sample per interface every N (default 5s)")
	sflowIfCount := flag.Int("sflow-interfaces", 4, "interfaces per sFlow agent that emit counter samples")

	duration := flag.Duration("duration", 60*time.Second, "how long to run; 0 = forever")
	flag.Parse()

	if *recsPerPkt < 1 || *recsPerPkt > 30 {
		log.Fatalf("--records-per-packet must be 1..30, got %d", *recsPerPkt)
	}
	if *rate == 0 && *sflowTarget == "" {
		log.Fatal("nothing to do: set --rate > 0 and/or --sflow-target")
	}

	deadline := time.Time{}
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
	}

	var wg sync.WaitGroup
	if *rate > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runV5(*target, *rate, *recsPerPkt, deadline)
		}()
	}
	if *sflowTarget != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSFlow(*sflowTarget, *sflowRate, *sflowCounterEvery, *sflowIfCount, deadline)
		}()
	}
	wg.Wait()
}

func pick[T any](rng *rand.Rand, pool []T) T {
	return pool[rng.IntN(len(pool))]
}

func dial(target string) (*net.UDPConn, error) {
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	return net.DialUDP("udp", nil, addr)
}
