// Package seqtrack maintains per-(exporter, source) datagram-
// sequence counters used to derive ingest lossiness. Each parser
// (NetFlow v9, IPFIX, sFlow) calls Note on every received datagram
// with its sequence number; the tracker accumulates total
// datagrams + total seq gaps. A periodic flusher in cmd/ingest
// writes the accumulated counters into the exporter_health
// ClickHouse table so the api service can surface per-exporter
// loss % on the Overview Exporter accuracy panel.
//
// The tracker is intentionally simple and parser-agnostic: it
// just tracks the unsigned-32 seq number per stream key. Sequence
// numbers wrap at 2^32, which the gap math handles via uint32
// subtraction.
package seqtrack

import (
	"net/netip"
	"sync"
	"time"
)

// Source labels are the same labels the flow record carries.
type Source = string

const (
	SourceNetFlowV9 Source = "netflow_v9"
	SourceIPFIX     Source = "ipfix"
	SourceSFlow     Source = "sflow"
)

type key struct {
	exporter netip.Addr
	source   Source
}

type state struct {
	lastSeq    uint32
	hasSeen    bool
	datagrams  uint64
	seqGaps    uint64
	updatedAt  time.Time
}

// Tracker accumulates per-stream seq counters. Safe for concurrent
// use from any number of parser goroutines.
type Tracker struct {
	mu     sync.Mutex
	by     map[key]*state
}

func New() *Tracker {
	return &Tracker{by: make(map[key]*state)}
}

// Note records that a datagram with the given sequence number was
// received from exporter on the named source. Returns the gap
// observed for this datagram (0 if first seen or perfectly
// sequential).
//
// The sequence increment between successive datagrams is
// per-source-defined: for NetFlow v9 it's per-datagram (+1 each),
// for IPFIX it's per-record-count (+N), for sFlow v5 it's
// per-datagram (+1). We can't infer the expected increment without
// the parser also passing a hint, so this function tracks the raw
// uint32 and reports any non-1 difference as a "gap" — meaningful
// for v9 / sFlow, an over-count for IPFIX (the IPFIX caller may
// post-correct by passing a recordsInDatagram argument in a future
// extension; a TODO).
func (t *Tracker) Note(exporter netip.Addr, source Source, seq uint32) (gap uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key{exporter: exporter, source: source}
	s := t.by[k]
	if s == nil {
		s = &state{}
		t.by[k] = s
	}
	s.datagrams++
	s.updatedAt = time.Now()
	if !s.hasSeen {
		s.hasSeen = true
		s.lastSeq = seq
		return 0
	}
	delta := seq - s.lastSeq
	switch {
	case delta == 1:
		// perfectly sequential, no gap
	case delta == 0:
		// duplicate datagram — uncommon but seen on multipath; not a loss
	case delta > 1 && delta < 0x80000000:
		// forward jump — count the missed seq numbers
		gap = delta - 1
		s.seqGaps += uint64(gap)
	default:
		// reorder / wrap — don't penalize on either path
	}
	s.lastSeq = seq
	return gap
}

// Snapshot is a tracker state copy intended for periodic flush
// to ClickHouse. Each call returns the rows accumulated since the
// last call and resets the in-memory counters.
type Snapshot struct {
	Exporter  netip.Addr
	Source    Source
	Datagrams uint64
	SeqGaps   uint64
	LastSeq   uint32
}

func (t *Tracker) Drain() []Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Snapshot, 0, len(t.by))
	for k, s := range t.by {
		// Skip rows with no traffic since last drain so the
		// exporter_health table doesn't bloat with zero rows for
		// silent exporters.
		if s.datagrams == 0 && s.seqGaps == 0 {
			continue
		}
		out = append(out, Snapshot{
			Exporter:  k.exporter,
			Source:    k.source,
			Datagrams: s.datagrams,
			SeqGaps:   s.seqGaps,
			LastSeq:   s.lastSeq,
		})
		// Reset deltas; keep lastSeq + hasSeen so the next datagram
		// continues to compute gap correctly.
		s.datagrams = 0
		s.seqGaps = 0
	}
	return out
}
