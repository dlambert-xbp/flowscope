package record

import (
	"context"
	"net/netip"
	"time"
)

// CounterSample is the canonical interface counter snapshot, normalized
// across sFlow if_counters and (eventually) gNMI OpenConfig
// /interfaces/interface/state/counters subscriptions. It carries
// ABSOLUTE octet/packet totals; rates are computed by diffing
// successive samples in the api layer.
//
// Per VISION.md §3.3, counter samples are AUTHORITATIVE for interface
// throughput. Anywhere the UI shows a rate, it prefers counter-sample
// diffs over flow-bucketed estimates and labels the source.
type CounterSample struct {
	// Observed wall-clock time the sample was produced (parser-local).
	Observed time.Time

	// Exporter is the canonical exporter address (sFlow agent IP for
	// sFlow; subscription target for gNMI).
	Exporter netip.Addr

	// IfIndex is the interface index on the exporter.
	IfIndex uint32

	// Octet totals. Counter type is uint64 because real switches
	// rotate uint32 in seconds at 10G+; we always promote.
	InOctets  uint64
	OutOctets uint64

	// Packet totals (sum of unicast + multicast + broadcast at parse
	// time when the wire format breaks them out separately).
	InPackets  uint64
	OutPackets uint64

	// Error / discard counters. Normalised to "since boot" cumulative;
	// rate-of-change is the api layer's responsibility.
	InErrors    uint64
	OutErrors   uint64
	InDiscards  uint64
	OutDiscards uint64

	// Source identifies which decoder produced this sample.
	Source SourceKind
}

// CounterSink consumes counter samples. Same semantics as Sink:
// implementations must be safe for concurrent calls and must not block
// longer than necessary — slow sinks create backpressure.
type CounterSink interface {
	Consume(ctx context.Context, c CounterSample) error
}

// CounterSinkFunc adapts a function to CounterSink.
type CounterSinkFunc func(ctx context.Context, c CounterSample) error

// Consume implements CounterSink.
func (fn CounterSinkFunc) Consume(ctx context.Context, c CounterSample) error { return fn(ctx, c) }

// CounterEmitter is the single fan-out point for counter samples.
// Mirrors Emitter but without a ring — counter samples are time-series
// data; live views diff successive rows from ClickHouse, they do not
// tail a hot buffer.
type CounterEmitter struct {
	sinks []CounterSink
}

// NewCounterEmitter constructs a CounterEmitter that writes to each
// provided sink in registration order.
func NewCounterEmitter(sinks ...CounterSink) *CounterEmitter {
	return &CounterEmitter{sinks: sinks}
}

// Emit publishes a counter sample to every sink. The first sink error
// short-circuits and is returned to the caller.
func (e *CounterEmitter) Emit(ctx context.Context, c CounterSample) error {
	for _, s := range e.sinks {
		if err := s.Consume(ctx, c); err != nil {
			return err
		}
	}
	return nil
}
