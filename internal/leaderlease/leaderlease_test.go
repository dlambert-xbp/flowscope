package leaderlease

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDB is the in-memory DB used by every state-machine test. It
// records exec calls, tracks the simulated current row, and lets a
// test override the holder returned by QueryRow to model contention.
type fakeDB struct {
	mu sync.Mutex

	// currentHolder is what SELECT FINAL returns. Empty means "no row".
	currentHolder string
	expiresAt     time.Time

	// hookExec runs on every Exec call. If it returns non-nil, the
	// Exec returns that error. Otherwise the fake applies the
	// "would-have-won" semantics by updating currentHolder to the
	// holder parameter unless overrideHolder is set.
	hookExec func(query string, args []any) error

	// overrideHolder, when non-empty, replaces whatever the Exec
	// would have written so the next QueryRow returns this string.
	// Used to simulate "another replica won simultaneously".
	overrideHolder string

	execCount    atomic.Int64
	queryCount   atomic.Int64
}

func (f *fakeDB) Exec(ctx context.Context, query string, args ...any) error {
	f.execCount.Add(1)
	if f.hookExec != nil {
		if err := f.hookExec(query, args); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// The lease writes two distinct INSERTs:
	//
	// Acquire/Renew: args = (name, holder, ttlMs, name, holder)
	//   len 5; arg[2] is int64 ttl.
	//
	// Release: args = (name, name, holder)
	//   len 3; all strings, conditional WHERE clears the row.
	switch len(args) {
	case 3:
		claimant, _ := args[2].(string)
		if claimant == f.currentHolder {
			f.currentHolder = ""
			f.expiresAt = time.Time{}
		}
		return nil
	case 5:
		inHolder, _ := args[1].(string)
		ttlMs, _ := args[2].(int64)
		now := time.Now()
		if f.currentHolder == "" || f.currentHolder == inHolder || f.expiresAt.Before(now) {
			if f.overrideHolder != "" {
				f.currentHolder = f.overrideHolder
			} else {
				f.currentHolder = inHolder
			}
			f.expiresAt = now.Add(time.Duration(ttlMs) * time.Millisecond)
		}
		return nil
	default:
		return nil
	}
}

func (f *fakeDB) QueryRow(ctx context.Context, query string, args ...any) Row {
	f.queryCount.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.currentHolder == "" {
		return &fakeRow{err: errEmptyRow}
	}
	return &fakeRow{holder: f.currentHolder, expiresAt: f.expiresAt}
}

var errEmptyRow = errors.New("fake: no row")

type fakeRow struct {
	holder    string
	expiresAt time.Time
	err       error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) >= 1 {
		if p, ok := dest[0].(*string); ok {
			*p = r.holder
		}
	}
	if len(dest) >= 2 {
		if p, ok := dest[1].(*time.Time); ok {
			*p = r.expiresAt
		}
	}
	return nil
}
func (r *fakeRow) Err() error { return r.err }

// ---------------------------------------------------------------------
// State-machine tests
// ---------------------------------------------------------------------

func TestAcquire_FreshLease(t *testing.T) {
	db := &fakeDB{}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("expected nil acquire, got %v", err)
	}
	if db.currentHolder != "node-a" {
		t.Fatalf("expected holder=node-a, got %q", db.currentHolder)
	}
}

func TestAcquire_AlreadyHeldBySelf(t *testing.T) {
	db := &fakeDB{currentHolder: "node-a", expiresAt: time.Now().Add(time.Minute)}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("expected nil acquire (already mine), got %v", err)
	}
}

func TestAcquire_HeldByOther_ReturnsNotLeader(t *testing.T) {
	db := &fakeDB{currentHolder: "node-b", expiresAt: time.Now().Add(time.Minute)}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	err := l.Acquire(context.Background())
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader, got %v", err)
	}
}

func TestAcquire_WhenExpired_Wins(t *testing.T) {
	db := &fakeDB{currentHolder: "node-b", expiresAt: time.Now().Add(-time.Minute)}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("expected nil acquire (expired), got %v", err)
	}
	if db.currentHolder != "node-a" {
		t.Fatalf("expected holder rotation to node-a, got %q", db.currentHolder)
	}
}

func TestAcquire_RaceLostToConcurrentWriter(t *testing.T) {
	db := &fakeDB{}
	// Simulate another candidate's INSERT landing between our INSERT
	// and our confirmation read by overriding the holder before
	// QueryRow reads it.
	db.overrideHolder = "node-b"
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	err := l.Acquire(context.Background())
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader on race-loss, got %v", err)
	}
}

func TestRenew_StillLeader(t *testing.T) {
	db := &fakeDB{}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	initialExpiry := db.expiresAt
	// Sleep a hair so the renewed expires_at is measurably later.
	time.Sleep(5 * time.Millisecond)
	if err := l.Renew(context.Background()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !db.expiresAt.After(initialExpiry) {
		t.Fatalf("renew should advance expires_at; got %v, was %v", db.expiresAt, initialExpiry)
	}
}

func TestRelease_ClearsHolder(t *testing.T) {
	db := &fakeDB{}
	l := New(db, Config{Name: "alert", Holder: "node-a", TTL: 30 * time.Second})
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release(context.Background())
	if db.currentHolder != "" {
		t.Fatalf("release should clear holder, got %q", db.currentHolder)
	}
}

func TestRun_BecomeLeaderCallbackRuns(t *testing.T) {
	db := &fakeDB{}
	l := New(db, Config{
		Name:          "alert",
		Holder:        "node-a",
		TTL:           300 * time.Millisecond,
		RenewInterval: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := make(chan struct{}, 1)
	cbCtx := make(chan context.Context, 1)
	done := make(chan struct{})

	go func() {
		_ = l.Run(ctx, func(lctx context.Context) error {
			called <- struct{}{}
			cbCtx <- lctx
			<-lctx.Done()
			return nil
		})
		close(done)
	}()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onBecomeLeader was never invoked")
	}

	// The callback received a derived context — verify cancellation
	// of the parent propagates.
	c := <-cbCtx
	if c == nil {
		t.Fatal("callback received nil context")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after parent cancel")
	}
}

func TestRun_OnLeaseLossCancelsChildContext(t *testing.T) {
	db := &fakeDB{}
	l := New(db, Config{
		Name:          "alert",
		Holder:        "node-a",
		TTL:           200 * time.Millisecond,
		RenewInterval: 30 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cbCtx := make(chan context.Context, 1)
	cbDone := make(chan struct{}, 1)
	runDone := make(chan struct{})

	go func() {
		_ = l.Run(ctx, func(lctx context.Context) error {
			cbCtx <- lctx
			<-lctx.Done()
			cbDone <- struct{}{}
			return nil
		})
		close(runDone)
	}()

	var leaderCtx context.Context
	select {
	case leaderCtx = <-cbCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("never became leader")
	}

	// Steal the lease from under us by overwriting state.
	db.mu.Lock()
	db.currentHolder = "node-b"
	db.expiresAt = time.Now().Add(time.Minute)
	db.mu.Unlock()

	// Wait for child context to be cancelled.
	select {
	case <-leaderCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("leader child context was not cancelled on lease loss")
	}
	select {
	case <-cbDone:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not return after lease loss")
	}

	cancel()
	<-runDone
}

func TestDefaultHolderID_NonEmpty(t *testing.T) {
	id := defaultHolderID()
	if id == "" {
		t.Fatal("default holder id should never be empty")
	}
}
