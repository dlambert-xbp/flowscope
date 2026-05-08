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
	lastSeq           uint32
	expectedIncrement uint32 // 1 for v9/sFlow, set per-call for IPFIX
	hasSeen           bool
	datagrams         uint64
	seqGaps           uint64
	updatedAt         time.Time
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

// Note records a datagram from a per-datagram-incrementing source
// (NetFlow v9, sFlow v5). Equivalent to NoteRecords with
// recordsInThisDatagram = 1.
func (t *Tracker) Note(exporter netip.Addr, source Source, seq uint32) (gap uint32) {
	return t.NoteRecords(exporter, source, seq, 1)
}

// NoteRecords records that a datagram with the given sequence
// number and record count was received from exporter on the named
// source. Returns the gap observed for this datagram (0 if first
// seen or perfectly sequential).
//
// The "expected increment" between consecutive datagrams varies by
// protocol:
//
//   - NetFlow v9 + sFlow v5 increment seq by 1 per datagram. Pass
//     recordsInThisDatagram = 1 (or call Note).
//   - IPFIX (RFC 7011 §3.1) increments seq by the number of Data
//     Records sent. So the *previous* datagram's record count is
//     what determines the expected jump in this datagram's seq.
//     Pass len(scratch) after parsing (the records this datagram
//     contributes) — the tracker remembers it across calls.
//
// Gap is computed as: current_seq - last_seq - last_expected_increment.
// We deliberately don't penalize duplicate / reorder / wrap.
func (t *Tracker) NoteRecords(exporter netip.Addr, source Source, seq uint32, recordsInThisDatagram uint32) (gap uint32) {
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
		s.expectedIncrement = recordsInThisDatagram
		return 0
	}
	delta := seq - s.lastSeq
	expected := s.expectedIncrement
	if expected == 0 {
		// We saw the previous datagram but never recorded an
		// expected increment for it — be permissive and treat any
		// forward jump as zero gap. Avoids false-positives for the
		// first IPFIX datagram after a v9 fallback / source switch.
		expected = delta
	}
	switch {
	case delta == expected:
		// perfectly sequential, no gap
	case delta == 0:
		// duplicate datagram — not a loss
	case delta > expected && delta < 0x80000000:
		// forward jump — count the missed seq numbers
		gap = delta - expected
		s.seqGaps += uint64(gap)
	default:
		// reorder / wrap — don't penalize on either path
	}
	s.lastSeq = seq
	s.expectedIncrement = recordsInThisDatagram
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
