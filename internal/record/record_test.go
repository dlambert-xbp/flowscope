package record

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestRing_PushAndSnapshot_OldestFirst(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.Push(Flow{Bytes: uint64(i)})
	}
	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []uint64{2, 3, 4} // first two pushes evicted
	for i, f := range got {
		if f.Bytes != want[i] {
			t.Errorf("got[%d].Bytes = %d, want %d", i, f.Bytes, want[i])
		}
	}
}

func TestRing_LenBeforeWrap(t *testing.T) {
	r := NewRing(10)
	for i := 0; i < 4; i++ {
		r.Push(Flow{})
	}
	if got := r.Len(); got != 4 {
		t.Fatalf("Len = %d, want 4", got)
	}
}

func TestRing_SnapshotEmpty(t *testing.T) {
	r := NewRing(8)
	if got := r.Snapshot(); len(got) != 0 {
		t.Errorf("empty snapshot len = %d", len(got))
	}
}

func TestRing_SnapshotIsCopy(t *testing.T) {
	r := NewRing(4)
	r.Push(Flow{Bytes: 1})
	snap := r.Snapshot()
	r.Push(Flow{Bytes: 2})
	if snap[0].Bytes != 1 || len(snap) != 1 {
		t.Fatalf("snapshot mutated: %+v", snap)
	}
}

func TestRing_ConcurrentPush(t *testing.T) {
	r := NewRing(1024)
	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 1000
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				r.Push(Flow{Bytes: uint64(j)})
			}
		}()
	}
	// concurrent reader
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = r.Snapshot()
		}
	}()
	wg.Wait()
	if r.Len() != 1024 {
		t.Fatalf("Len = %d, want 1024", r.Len())
	}
}

func TestEmitter_FanOut(t *testing.T) {
	r := NewRing(8)
	var sinkSeen int
	sink := SinkFunc(func(ctx context.Context, f Flow) error {
		sinkSeen++
		return nil
	})
	e := NewEmitter(r, sink)

	f := Flow{
		Observed: time.Unix(1, 0),
		Exporter: netip.MustParseAddr("10.0.0.1"),
		Bytes:    42,
		Source:   SourceNetFlowV5,
	}
	if err := e.Emit(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Errorf("ring not pushed: len = %d", r.Len())
	}
	if sinkSeen != 1 {
		t.Errorf("sink not called: seen = %d", sinkSeen)
	}
}

func TestEmitter_NilRing(t *testing.T) {
	called := false
	e := NewEmitter(nil, SinkFunc(func(_ context.Context, _ Flow) error {
		called = true
		return nil
	}))
	if err := e.Emit(context.Background(), Flow{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Errorf("sink not invoked when ring is nil")
	}
}

func TestSourceKind_String(t *testing.T) {
	tests := map[SourceKind]string{
		SourceNetFlowV5: "netflow.v5",
		SourceNetFlowV9: "netflow.v9",
		SourceIPFIX:     "ipfix",
		SourceSFlowV5:   "sflow.v5",
		SourceUnknown:   "unknown",
	}
	for k, want := range tests {
		if got := k.String(); got != want {
			t.Errorf("SourceKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}
