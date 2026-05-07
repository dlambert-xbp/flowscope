package store

import (
	"context"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/record"
)

// CounterBatcher buffering covered without a live ClickHouse — the
// PrepareBatch / Send path is exercised by integration tests in a
// later slice.

func TestCounterBatcher_Consume_AppendsToBuffer(t *testing.T) {
	b := &CounterBatcher{
		maxSize:     1000,
		maxInterval: time.Hour,
		buf:         make([]record.CounterSample, 0),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	defer close(b.done)

	for i := 0; i < 5; i++ {
		if err := b.Consume(context.Background(), record.CounterSample{IfIndex: uint32(i)}); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) != 5 {
		t.Errorf("buf len = %d, want 5", len(b.buf))
	}
}

func TestCounterBatcher_Consume_SignalsAtMaxSize(t *testing.T) {
	b := &CounterBatcher{
		maxSize:     3,
		maxInterval: time.Hour,
		buf:         make([]record.CounterSample, 0),
		flushCh:     make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	defer close(b.done)

	for i := 0; i < 3; i++ {
		if err := b.Consume(context.Background(), record.CounterSample{}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-b.flushCh:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("flushCh not signaled at maxSize")
	}
}
