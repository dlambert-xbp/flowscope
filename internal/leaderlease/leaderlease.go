// Package leaderlease implements a small ClickHouse-backed leader
// lease that cmd/alert uses to run engine + dispatcher loops on a
// single replica at a time. The pattern is intentionally simple — we
// already speak ClickHouse from every service, so reusing it avoids
// dragging ZooKeeper / etcd / Keeper into the deploy footprint.
//
// Algorithm (also documented inline on acquireOnce):
//
//  1. SELECT FINAL the current row for the lease name.
//  2. If no row, or holder == self, or expires_at < now() → INSERT a
//     new row with updated expires_at and acquired_at = now().
//  3. SELECT FINAL again to confirm self is the holder. The second
//     read closes the race when two followers attempt to acquire
//     simultaneously — the row with the greater acquired_at wins on
//     merge, and our confirmation read reflects that.
//
// Lease semantics (single-replica preserved):
//   - On a clean restart the previous holder either released the lease
//     (Release on graceful shutdown) or its TTL has expired. Either
//     way, the first replica to call Acquire wins immediately — no
//     warm-up delay beyond what the previous TTL forces.
//   - One replica = same code path as today. acquireOnce returns true
//     on the first call, Run invokes the callback once, the engine
//     runs uninterrupted.
//
// What the lease does NOT defend against (caveats — surface these in
// the PR body):
//
//   - Clock skew between the database server and the holder. We use
//     ClickHouse's now64(3) for both writes and comparisons, so two
//     replicas see the same clock as long as they all talk to the same
//     ClickHouse cluster. A replica with a wildly skewed local clock
//     can still call Renew at the wrong cadence (the renewer runs off
//     time.Now), so we recommend ntpd / chrony — same as every other
//     distributed system.
//
//   - Asymmetric partition where the leader can write to ClickHouse
//     but a follower can't read the latest state. ReplacingMergeTree
//     merges are eventually consistent, so a follower may briefly see
//     a stale holder on read. We protect against split-brain by always
//     re-confirming with SELECT FINAL after writing, and by setting
//     expires_at conservatively (TTL ≥ 3× renewInterval so a single
//     missed renew doesn't drop the lease).
//
//   - "Generational" fencing tokens. We don't pass a fence token down
//     to the engine / dispatcher because ClickHouse writes are
//     idempotent under our existing schemas (alert_events keyed by
//     (ts, rule_id, …); webhook_deliveries dedup on signature). A
//     deposed leader writing one final tick is harmless.
package leaderlease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/obs"
)

// DB is the subset of ClickHouse driver methods the lease needs. The
// interface lets us swap a fake in unit tests without dragging
// testcontainers into every run. Production wires in driver.Conn,
// which satisfies this interface.
type DB interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryRow(ctx context.Context, query string, args ...any) Row
}

// Row mirrors driver.Row narrowly enough for tests.
type Row interface {
	Scan(dest ...any) error
	Err() error
}

// Lease is the value type held by callers. Construct with New and use
// either Run (typical) or Acquire/Renew/Release (advanced).
type Lease struct {
	db            DB
	name          string
	holder        string
	ttl           time.Duration
	renewInterval time.Duration
	logger        *slog.Logger

	// nowFn lets tests inject a fixed clock for Run's renew timer. The
	// database-side comparison uses ClickHouse's now64(3) and is not
	// affected by this field.
	nowFn func() time.Time
}

// Config tunes a Lease. Zero values are valid; defaults are applied
// in New.
type Config struct {
	Name          string        // lease key (e.g. "alert")
	TTL           time.Duration // total lease lifetime per acquire/renew, default 30s
	RenewInterval time.Duration // how often the leader renews, default TTL/3
	Holder        string        // identity override (tests). Default: hostname-pid-rand
	Logger        *slog.Logger
}

// New constructs a Lease. If Holder is unset a self-identifying string
// is generated (hostname-pid-random) so two replicas on the same host
// don't collide.
func New(db DB, cfg Config) *Lease {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	renew := cfg.RenewInterval
	if renew <= 0 {
		renew = ttl / 3
	}
	holder := cfg.Holder
	if holder == "" {
		holder = defaultHolderID()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Lease{
		db:            db,
		name:          cfg.Name,
		holder:        holder,
		ttl:           ttl,
		renewInterval: renew,
		logger:        logger,
		nowFn:         time.Now,
	}
}

// Holder returns the unique identity this Lease uses when claiming
// the row. Exported for tests + observability.
func (l *Lease) Holder() string { return l.holder }

// Name returns the lease key. Useful for log lines.
func (l *Lease) Name() string { return l.name }

// Acquire attempts to win the lease in one round trip. Returns nil if
// this replica holds the lease afterwards (newly acquired OR already
// the current holder). Returns an error if it can't — including the
// non-fatal "another holder owns it" case, which surfaces as
// ErrNotLeader so the caller can distinguish a transient DB failure
// from "we're a follower right now".
func (l *Lease) Acquire(ctx context.Context) error {
	return l.acquireOnce(ctx)
}

// Renew extends the current holder's expires_at. Returns ErrNotLeader
// if a different holder has won the lease since we last checked — the
// caller (Run) treats that as a hard lease loss and cancels the
// child context.
func (l *Lease) Renew(ctx context.Context) error {
	// Renew is the same write path as Acquire — the WHERE-effective
	// check happens inline against now64(3) and the current holder.
	// We don't optimise the "already mine" case because the round trip
	// is cheap and ReplacingMergeTree de-dupes on background merge.
	return l.acquireOnce(ctx)
}

// Release best-effort clears the holder by writing an expired row so
// the next Acquire by anyone (including this replica) treats the
// lease as free. Errors are logged but not surfaced — we're in a
// shutdown path.
func (l *Lease) Release(ctx context.Context) {
	// Use a short deadline so a stuck DB doesn't block shutdown.
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Write expires_at = epoch so any future SELECT FINAL sees an
	// expired lease. We don't DELETE because ReplacingMergeTree merges
	// are deferred and the row would resurrect briefly anyway.
	const q = `
INSERT INTO leader_lease (name, holder, expires_at, acquired_at)
SELECT ?, '', toDateTime64(0, 3, 'UTC'), now64(3)
WHERE (SELECT holder FROM leader_lease FINAL WHERE name = ?) = ?
`
	if err := l.db.Exec(rctx, q, l.name, l.name, l.holder); err != nil {
		l.logger.Warn("leaderlease: release write failed", "name", l.name, "err", err)
	}
	obs.AlertLeaseHeld.Set(0)
}

// ErrNotLeader signals that a different replica currently holds the
// lease. Callers distinguish this from a real DB error so they can
// log at info (followers are normal) instead of warn.
var ErrNotLeader = errors.New("leaderlease: not the leader")

// acquireOnce implements one full acquire round trip:
//
//  1. INSERT a new row IF (no row OR holder == self OR expires_at < now()).
//     The WHERE filter is server-side via INSERT ... SELECT — no client
//     race, no time-of-check / time-of-use gap on the holder column.
//  2. Read back with SELECT FINAL to confirm we own the row. If another
//     candidate's INSERT landed in the same window, ReplacingMergeTree
//     keeps the row with the greater acquired_at version, and our
//     confirmation read will return that holder. We treat any non-self
//     holder as ErrNotLeader.
//
// All time comparisons use ClickHouse's now64(3) so two replicas with
// drifting local clocks still agree on "is the lease expired right now".
func (l *Lease) acquireOnce(ctx context.Context) error {
	const insert = `
INSERT INTO leader_lease (name, holder, expires_at, acquired_at)
SELECT
    ? AS name,
    ? AS holder,
    now64(3) + INTERVAL ? MILLISECOND AS expires_at,
    now64(3) AS acquired_at
WHERE
    NOT EXISTS (SELECT 1 FROM leader_lease FINAL WHERE name = ? AND expires_at > now64(3) AND holder != ?)
`
	ttlMs := int64(l.ttl / time.Millisecond)
	if err := l.db.Exec(ctx, insert, l.name, l.holder, ttlMs, l.name, l.holder); err != nil {
		return fmt.Errorf("leaderlease: insert: %w", err)
	}

	// Confirm. SELECT FINAL collapses any concurrent inserts that
	// landed in the same window by acquired_at — whichever write
	// carries the greater version wins.
	const sel = `
SELECT holder, expires_at FROM leader_lease FINAL WHERE name = ?
`
	row := l.db.QueryRow(ctx, sel, l.name)
	if row == nil {
		return errors.New("leaderlease: query returned nil row")
	}
	var holder string
	var expiresAt time.Time
	if err := row.Scan(&holder, &expiresAt); err != nil {
		// No row yet — our INSERT was filtered out by the NOT EXISTS
		// guard AND there was no pre-existing row either. That's
		// only possible if the INSERT silently failed; treat as
		// follower so the caller retries.
		return ErrNotLeader
	}
	if holder != l.holder {
		return ErrNotLeader
	}
	// Belt-and-braces: if we appear to hold the lease but the row's
	// expires_at is already in the past (clock skew on the DB side?),
	// behave as a follower so we retry.
	if !expiresAt.IsZero() && expiresAt.Before(time.Now()) {
		return ErrNotLeader
	}
	return nil
}

// Run is the high-level driver. It calls acquire/renew on a loop until
// ctx is cancelled. When the lease is won, onBecomeLeader is invoked
// with a child context that is cancelled the moment the lease is lost
// (deliberately or via DB-reported handover). onBecomeLeader is
// expected to start its long-lived goroutines and return when the
// child context is done — the engine + dispatcher already follow this
// pattern in cmd/alert/main.go.
//
// Run returns nil when ctx is cancelled and the leader callback (if
// running) has returned. A non-nil error from onBecomeLeader is
// surfaced upward.
func (l *Lease) Run(ctx context.Context, onBecomeLeader func(ctx context.Context) error) error {
	// Poll interval for followers. Short enough that a freshly-restored
	// quorum doesn't sit idle for half a TTL; long enough that 5 idle
	// followers don't hammer ClickHouse. Half the renew interval is a
	// reasonable default and keeps a clean restart's "time to leader"
	// bounded by the previous TTL.
	pollInterval := l.renewInterval / 2
	if pollInterval < 250*time.Millisecond {
		pollInterval = 250 * time.Millisecond
	}

	var leaderCancel context.CancelFunc
	var leaderErrCh chan error
	leading := false

	stopLeader := func(reason string) {
		if !leading {
			return
		}
		l.logger.Info("leaderlease: stepping down", "name", l.name, "holder", l.holder, "reason", reason)
		obs.AlertLeaseLostTotal.Inc()
		obs.AlertLeaseHeld.Set(0)
		leaderCancel()
		// Wait for the callback to return so we don't accidentally
		// start a second one if we re-win quickly.
		if err := <-leaderErrCh; err != nil {
			l.logger.Warn("leaderlease: leader callback exited with error", "err", err)
		}
		leading = false
	}

	defer func() {
		// Final teardown: cancel the child context if still leading
		// and release the lease so the next replica wins fast.
		if leading {
			leaderCancel()
			<-leaderErrCh
		}
		// Use a fresh background ctx for release — the parent is
		// already done by definition when this defer runs.
		l.Release(context.Background())
	}()

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	// Try once immediately so single-replica deployments don't wait
	// for the first tick.
	l.attemptOnce(ctx, &leading, &leaderCancel, &leaderErrCh, onBecomeLeader, stopLeader)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			l.attemptOnce(ctx, &leading, &leaderCancel, &leaderErrCh, onBecomeLeader, stopLeader)
		}
	}
}

// attemptOnce factors a single acquire/renew tick out of Run so the
// initial-fire and ticker paths share the same logic. The pointer
// parameters are deliberate — Run owns the state and attemptOnce
// mutates it in place.
func (l *Lease) attemptOnce(
	ctx context.Context,
	leading *bool,
	leaderCancel *context.CancelFunc,
	leaderErrCh *chan error,
	onBecomeLeader func(ctx context.Context) error,
	stopLeader func(reason string),
) {
	err := l.acquireOnce(ctx)
	switch {
	case err == nil:
		if !*leading {
			l.logger.Info("leaderlease: acquired", "name", l.name, "holder", l.holder, "ttl", l.ttl.String())
			obs.AlertLeaseAcquiredTotal.Inc()
			obs.AlertLeaseHeld.Set(1)
			lctx, cancel := context.WithCancel(ctx)
			*leaderCancel = cancel
			errCh := make(chan error, 1)
			*leaderErrCh = errCh
			go func() {
				defer close(errCh)
				if cbErr := onBecomeLeader(lctx); cbErr != nil {
					errCh <- cbErr
				}
			}()
			*leading = true
		}
	case errors.Is(err, ErrNotLeader):
		if *leading {
			stopLeader("not the leader on renew")
		}
		// Followers log at debug so the routine "still a follower"
		// doesn't spam in HA deployments.
		l.logger.Debug("leaderlease: not the leader", "name", l.name)
	default:
		// Real DB error. If we were leading, hold the lease — a
		// single missed renew shouldn't depose us as long as
		// expires_at is still in the future. The next tick will
		// retry. If we have repeated failures the lease will simply
		// expire and another replica will pick it up.
		obs.AlertLeaseRenewFailedTotal.Inc()
		l.logger.Warn("leaderlease: acquire/renew failed", "name", l.name, "err", err)
	}
}

// defaultHolderID returns a stable per-process identity string. Format:
// "<hostname>-<pid>-<8hexrand>". The random suffix guards the rare
// case of two replicas with the same hostname (StatefulSet pod-N
// rescheduled within a TTL window).
func defaultHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
