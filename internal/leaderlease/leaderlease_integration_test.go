//go:build integration

package leaderlease_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/leaderlease"
	"github.com/dlambert-xbp/flowscope/test/integration"
)

// TestLeaderLease_TwoReplicas_ExactlyOneLeader spins up a real
// ClickHouse via testcontainers and runs two Lease instances
// concurrently. We assert:
//
//   - Exactly one of them wins the initial acquire.
//   - When the winner is killed (its Run ctx cancels and it calls
//     Release in the deferred path), the other wins within one TTL.
//   - During the steady state never more than one holder is observed.
//
// The test is intentionally aggressive on timing — short TTL + fast
// renew — so a regression in the WHERE-effective acquire query
// surfaces as a flapping holder rather than passing on luck.
func TestLeaderLease_TwoReplicas_ExactlyOneLeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	db := leaderlease.FromConn(h.Conn)

	const ttl = 1500 * time.Millisecond
	const renew = 400 * time.Millisecond

	mkLease := func(holder string) *leaderlease.Lease {
		return leaderlease.New(db, leaderlease.Config{
			Name:          "test-alert",
			Holder:        holder,
			TTL:           ttl,
			RenewInterval: renew,
		})
	}

	type leaderState struct {
		mu      sync.Mutex
		leaders map[string]bool
	}
	state := &leaderState{leaders: make(map[string]bool)}

	setLeader := func(name string, on bool) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if on {
			state.leaders[name] = true
		} else {
			delete(state.leaders, name)
		}
		if len(state.leaders) > 1 {
			// Capture for the assertion below; don't t.Fatal from
			// a non-test goroutine.
			atomic.AddInt32(&overlapCount, 1)
		}
	}

	leaderACtx, cancelA := context.WithCancel(ctx)
	leaderBCtx, cancelB := context.WithCancel(ctx)

	leaseA := mkLease("replica-a")
	leaseB := mkLease("replica-b")

	winnerA := make(chan struct{}, 1)
	winnerB := make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = leaseA.Run(leaderACtx, func(lctx context.Context) error {
			select {
			case winnerA <- struct{}{}:
			default:
			}
			setLeader("a", true)
			defer setLeader("a", false)
			<-lctx.Done()
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_ = leaseB.Run(leaderBCtx, func(lctx context.Context) error {
			select {
			case winnerB <- struct{}{}:
			default:
			}
			setLeader("b", true)
			defer setLeader("b", false)
			<-lctx.Done()
			return nil
		})
	}()

	// One of them should become leader within ~1 TTL.
	var firstWinner string
	select {
	case <-winnerA:
		firstWinner = "a"
	case <-winnerB:
		firstWinner = "b"
	case <-time.After(5 * time.Second):
		cancelA()
		cancelB()
		wg.Wait()
		t.Fatal("neither replica won the initial lease within 5s")
	}
	t.Logf("initial winner: %s", firstWinner)

	// Hold for a few renew cycles and confirm no overlap.
	time.Sleep(2 * time.Second)

	// Kill the current leader; the other replica must take over.
	if firstWinner == "a" {
		cancelA()
	} else {
		cancelB()
	}

	// Wait for handover. The follower must acquire within roughly
	// one TTL + one poll cycle.
	deadline := time.Now().Add(5 * time.Second)
	for {
		state.mu.Lock()
		hasLeader := len(state.leaders) >= 1
		state.mu.Unlock()
		if hasLeader && time.Since(deadline) < 0 {
			// Give the system a chance to fully transition: re-check
			// whether the surviving replica is the one holding.
			state.mu.Lock()
			leaders := make([]string, 0, len(state.leaders))
			for k := range state.leaders {
				leaders = append(leaders, k)
			}
			state.mu.Unlock()
			if len(leaders) == 1 && leaders[0] != firstWinner {
				break
			}
		}
		if time.Now().After(deadline) {
			cancelA()
			cancelB()
			wg.Wait()
			t.Fatalf("handover did not complete within 5s; current leaders=%v", state.leaders)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancelA()
	cancelB()
	wg.Wait()

	if got := atomic.LoadInt32(&overlapCount); got > 0 {
		t.Fatalf("observed split-brain: %d overlap events", got)
	}
}

var overlapCount int32

// TestLeaderLease_RestartAfterRelease verifies that a graceful
// shutdown (Run returns → defer Release fires) frees the lease fast
// enough that the next Lease.Run wins on its first poll, not after a
// full TTL.
func TestLeaderLease_RestartAfterRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	db := leaderlease.FromConn(h.Conn)
	ttl := 30 * time.Second // realistic prod default
	renew := 10 * time.Second

	first := leaderlease.New(db, leaderlease.Config{
		Name: "test-restart", Holder: "first", TTL: ttl, RenewInterval: renew,
	})
	if err := first.Acquire(ctx); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release(ctx)

	second := leaderlease.New(db, leaderlease.Config{
		Name: "test-restart", Holder: "second", TTL: ttl, RenewInterval: renew,
	})
	if err := second.Acquire(ctx); err != nil {
		t.Fatalf("second acquire after first release: %v (should win without waiting TTL)", err)
	}
}

// TestLeaderLease_ConcurrentRunReturnsErrNotLeader ensures a follower
// gets a clean ErrNotLeader rather than silently waiting when another
// holder is fresh.
func TestLeaderLease_ConcurrentRunReturnsErrNotLeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h := integration.StartClickHouse(t, ctx)
	t.Cleanup(h.Cleanup)

	db := leaderlease.FromConn(h.Conn)
	holder := leaderlease.New(db, leaderlease.Config{
		Name: "test-conflict", Holder: "a", TTL: time.Minute, RenewInterval: 20 * time.Second,
	})
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	follower := leaderlease.New(db, leaderlease.Config{
		Name: "test-conflict", Holder: "b", TTL: time.Minute, RenewInterval: 20 * time.Second,
	})
	err := follower.Acquire(ctx)
	if !errors.Is(err, leaderlease.ErrNotLeader) {
		t.Fatalf("follower got %v, want ErrNotLeader", err)
	}
}
