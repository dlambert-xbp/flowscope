package main

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// allowlistGate enforces the exporter_allowlist deny-by-default
// policy at packet ingress.
//
// Semantics (verbatim from TASKS.md Session C):
//   - Empty allowlist (zero rows) → accept-all (preserve current
//     behavior; safety net for the security cutover).
//   - Non-empty allowlist + row enabled = 1 → accept that source IP.
//   - Source not in allowlist OR row enabled = 0 → drop, increment
//     flowscope_ingest_dropped_unauthorized_total{exporter=<ip>}.
//
// The gate is evaluated BEFORE parse so we don't burn CPU decoding
// packets we are about to throw away. It is also the cheapest place
// to enforce policy: a single map lookup under an RLock.
//
// Concurrency: the embedded map is guarded by mu. Refresh is the
// only writer; listeners are readers. The refresh tick is 30s by
// default (see refreshInterval); allowlist mutations made via the
// API are visible to ingest within that window. Operators can speed
// the convergence up by reducing the interval, at the cost of more
// ClickHouse load.
type allowlistGate struct {
	mu sync.RWMutex
	// enabled[addr] = true means addr has an enabled=1 row.
	// enabled[addr] = false means the row exists but enabled=0.
	// Absent key in a non-empty map means "not on allowlist" → drop.
	enabled map[netip.Addr]bool
	// empty == true when the table has zero rows; gate accepts all.
	empty bool
}

func newAllowlistGate() *allowlistGate {
	return &allowlistGate{
		enabled: map[netip.Addr]bool{},
		// Default open until first refresh completes — matches the
		// "empty table = accept all" rule and avoids dropping packets
		// during the priming round-trip if ClickHouse is slow.
		empty: true,
	}
}

// Allow reports whether a datagram from src should be accepted.
// Allow is called once per UDP datagram in the listener hot path.
// Cost: one RLock + one map read on the non-empty path; cheaper than
// even a single allocation, so safe to call inline.
//
// On a deny decision the dropped counter is bumped here so callers
// don't have to remember; they only need to `continue` on false.
func (g *allowlistGate) Allow(src netip.Addr) bool {
	src = src.Unmap()
	g.mu.RLock()
	if g.empty {
		g.mu.RUnlock()
		return true
	}
	enabled, ok := g.enabled[src]
	g.mu.RUnlock()
	if ok && enabled {
		return true
	}
	obs.IngestDroppedUnauthorized.WithLabelValues(src.String()).Inc()
	return false
}

// replace atomically swaps the gate's view of the allowlist. Callers
// build the next map outside the lock so we hold the write lock for
// only a pointer swap, never the duration of a ClickHouse round-trip.
func (g *allowlistGate) replace(next map[netip.Addr]bool, empty bool) {
	g.mu.Lock()
	g.enabled = next
	g.empty = empty
	g.mu.Unlock()
}

// refreshOnce reloads the allowlist into a fresh map and swaps it in.
// It is split out so unit tests can exercise the priming + refresh
// path without a ticker.
func (g *allowlistGate) refreshOnce(ctx context.Context, store settings.AllowlistStore) error {
	rows, err := store.List(ctx)
	if err != nil {
		return err
	}
	next := make(map[netip.Addr]bool, len(rows))
	for _, e := range rows {
		addr, err := netip.ParseAddr(strings.TrimSpace(e.Exporter))
		if err != nil {
			// Skip malformed rows but don't fail the whole refresh —
			// a single bad insert shouldn't blind the gate.
			slog.Warn("allowlist: skipping unparseable exporter", "exporter", e.Exporter, "err", err)
			continue
		}
		next[addr.Unmap()] = e.Enabled
	}
	g.replace(next, len(next) == 0)
	return nil
}

// runRefresher primes the gate once synchronously, then loops on a
// ticker. Returns when ctx is cancelled. A failed refresh logs a
// warning and keeps the previously-loaded map in place — losing one
// refresh is strictly safer than going back to permissive mode on a
// transient ClickHouse blip.
func (g *allowlistGate) runRefresher(ctx context.Context, store settings.AllowlistStore, interval time.Duration) {
	if err := g.refreshOnce(ctx, store); err != nil {
		slog.Warn("allowlist: initial refresh failed (gate still in default-open mode)", "err", err)
	} else {
		g.mu.RLock()
		empty := g.empty
		size := len(g.enabled)
		g.mu.RUnlock()
		slog.Info("allowlist: primed", "rows", size, "deny_by_default", !empty)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.refreshOnce(ctx, store); err != nil {
				slog.Warn("allowlist: refresh failed (keeping previous map)", "err", err)
			}
		}
	}
}
