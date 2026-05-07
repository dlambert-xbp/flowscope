package record

import "context"

// Sink consumes flows. Implementations MUST be safe for concurrent calls.
// A slow Sink creates backpressure that propagates back to the parser
// pool — this is intentional. If the ClickHouse batcher cannot keep up,
// drop counters increment on the listener side rather than memory
// growing unbounded.
type Sink interface {
	Consume(ctx context.Context, f Flow) error
}

// SinkFunc adapts a function to the Sink interface.
type SinkFunc func(ctx context.Context, f Flow) error

// Consume implements Sink.
func (fn SinkFunc) Consume(ctx context.Context, f Flow) error { return fn(ctx, f) }

// Emitter is the single fan-out point for parsed flows. A process
// registers exactly one Emitter at startup; every parser calls
// Emitter.Emit. New consumers register as Sinks; nothing parses-then-
// stores by bypassing the Emitter.
//
// Construction order matters: the in-process Ring is updated first
// (synchronously, cheap) so live views see the record before any slow
// downstream sink can stall.
type Emitter struct {
	ring  *Ring
	sinks []Sink
}

// NewEmitter constructs an Emitter that pushes to the given ring (may
// be nil) and to each provided sink, in registration order.
func NewEmitter(ring *Ring, sinks ...Sink) *Emitter {
	return &Emitter{ring: ring, sinks: sinks}
}

// Emit publishes a flow to the ring (if configured) and to every sink.
// The first sink error is returned; later sinks are not invoked. Callers
// (parser workers) increment the appropriate Prometheus counter on
// non-nil errors.
func (e *Emitter) Emit(ctx context.Context, f Flow) error {
	if e.ring != nil {
		e.ring.Push(f)
	}
	for _, s := range e.sinks {
		if err := s.Consume(ctx, f); err != nil {
			return err
		}
	}
	return nil
}
