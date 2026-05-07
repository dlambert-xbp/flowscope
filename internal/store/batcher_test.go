package store

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// Most of FlowBatcher's interesting behaviour — the Sink contract,
// flush-on-size, flush-on-interval, Close-flushes-pending — exercises
// the buffering logic without touching ClickHouse. The full write path
// (PrepareBatch / Append / Send) is covered by the integration test
// (build tag `integration`) which spins up a real ClickHouse.

func TestFlowBatcher_Consume_AppendsToBuffer(t *testing.T) {
	b := &FlowBatcher{
		maxSize:     1000,
		maxInterval: time.Hour, // never auto-flush
		buf:         make([]record.Flow, 0),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	defer close(b.done)

	for i := 0; i < 5; i++ {
		if err := b.Consume(context.Background(), record.Flow{Bytes: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) != 5 {
		t.Errorf("buf len = %d, want 5", len(b.buf))
	}
}

func TestFlowBatcher_Consume_SignalsAtMaxSize(t *testing.T) {
	b := &FlowBatcher{
		maxSize:     3,
		maxInterval: time.Hour,
		buf:         make([]record.Flow, 0),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	defer close(b.done)

	for i := 0; i < 3; i++ {
		if err := b.Consume(context.Background(), record.Flow{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-b.flushCh:
		// good — flusher was signaled
	case <-time.After(50 * time.Millisecond):
		t.Fatal("flushCh not signaled at maxSize")
	}
}

func TestToIPv6_Always16Bytes(t *testing.T) {
	// ClickHouse's IPv6 column wants 16 bytes regardless of family.
	// net.IP.String() collapses 4-in-6 to its IPv4 rendering, which
	// would mask a length bug — assert on raw bytes instead.
	cases := []struct {
		in   string
		last [4]byte // for IPv4 inputs, the trailing 4 bytes
	}{
		{"10.2.0.11", [4]byte{10, 2, 0, 11}},
		{"0.0.0.0", [4]byte{0, 0, 0, 0}},
		{"172.16.4.9", [4]byte{172, 16, 4, 9}},
	}
	for _, tc := range cases {
		ip := toIPv6(netip.MustParseAddr(tc.in))
		if len(ip) != 16 {
			t.Errorf("toIPv6(%q) length = %d, want 16", tc.in, len(ip))
			continue
		}
		// IPv4-mapped form: ::ffff:a.b.c.d → bytes 10,11 = 0xff, last 4 = octets
		if ip[10] != 0xff || ip[11] != 0xff {
			t.Errorf("toIPv6(%q) bytes[10:12] = %x, want 0xffff", tc.in, ip[10:12])
		}
		if [4]byte(ip[12:16]) != tc.last {
			t.Errorf("toIPv6(%q) bytes[12:16] = %v, want %v", tc.in, ip[12:16], tc.last)
		}
	}

	// Native IPv6 round-trips byte-identically.
	addr := netip.MustParseAddr("2001:db8::1")
	want := addr.As16()
	got := toIPv6(addr)
	if [16]byte(got) != want {
		t.Errorf("toIPv6(IPv6) bytes mismatch: got %v, want %v", got, want)
	}
}
