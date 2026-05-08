package snmpx

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Scheduler walks every observed exporter on a configurable cadence
// and persists the resulting Inventory + Interfaces to ClickHouse.
//
// VISION.md §4.2 — per-device interval is configurable, failures
// bypass the interval so flapping devices retry on the next tick,
// and in-flight walks are deduped so a slow device cannot stack
// work on the worker pool.
//
// Credential lookup: for each walk, the scheduler asks creds.Get()
// for the (decrypted) per-exporter binding. If none is configured
// the scheduler falls back to the FallbackClient (the old
// cluster-wide community / mock). This lets the dev loop work with
// no credential setup while real deployments configure per-target.
type Scheduler struct {
	conn           driver.Conn
	creds          CredentialStore
	fallback       Client
	interval       time.Duration
	concurrency    int

	mu         sync.Mutex
	inFlight   map[string]bool
	lastWalked map[string]time.Time
}

// NewScheduler returns a Scheduler. creds may be nil; in that case
// every walk uses fallback. fallback may also be nil; without it the
// scheduler logs and skips any exporter without an explicit
// credential binding.
func NewScheduler(conn driver.Conn, creds CredentialStore, fallback Client, interval time.Duration, concurrency int) *Scheduler {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	return &Scheduler{
		conn:        conn,
		creds:       creds,
		fallback:    fallback,
		interval:    interval,
		concurrency: concurrency,
		inFlight:    make(map[string]bool),
		lastWalked:  make(map[string]time.Time),
	}
}

// Run blocks until ctx is cancelled. Every tick (a small fraction of
// the interval) it discovers exporters from the flows table, picks
// any whose last walk is older than interval, and dispatches them
// onto the worker pool.
func (s *Scheduler) Run(ctx context.Context) error {
	tick := s.interval / 6
	if tick < 30*time.Second {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()

	// Worker pool — bounded concurrency.
	jobs := make(chan string, 256)
	var wg sync.WaitGroup
	for i := 0; i < s.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				s.walkOne(ctx, target)
			}
		}()
	}

	// Always do an initial pass on startup so the dashboard has
	// inventory data within seconds, not after a full interval.
	s.dispatch(ctx, jobs)

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil
		case <-t.C:
			s.dispatch(ctx, jobs)
		}
	}
}

// dispatch enqueues every exporter that's overdue for a walk.
//
// Cadence selection: per-binding interval (from snmp_credentials) wins
// when configured; the cluster-wide default is the fallback. The
// per-binding snapshot is taken once per dispatch pass via the
// redacted List query so the lookup is constant per pass and does not
// decrypt secrets. Note that the dispatch tick (configured in Run)
// floors how often we re-evaluate staleness — a per-binding interval
// shorter than that tick will be evaluated only as often as the tick
// fires.
//
// Operator-triggered walks: any exporter with a row in
// snmp_walk_requests whose requested_at post-dates lastWalked bypasses
// the staleness check entirely.
func (s *Scheduler) dispatch(ctx context.Context, jobs chan<- string) {
	intervals := s.snapshotIntervals(ctx)
	forced := s.snapshotForceRequests(ctx)
	exporters, err := s.discoverExporters(ctx, forced)
	if err != nil {
		slog.Warn("snmp: discover exporters", "err", err)
		return
	}
	now := time.Now()
	for _, exp := range exporters {
		s.mu.Lock()
		if s.inFlight[exp] {
			s.mu.Unlock()
			continue
		}
		last := s.lastWalked[exp]
		if !s.isForced(exp, last, forced) {
			eff := s.effectiveInterval(exp, intervals)
			if !last.IsZero() && now.Sub(last) < eff {
				s.mu.Unlock()
				continue
			}
		}
		s.inFlight[exp] = true
		s.mu.Unlock()

		select {
		case jobs <- exp:
		default:
			// Worker pool saturated; clear inFlight so we retry next tick.
			s.mu.Lock()
			delete(s.inFlight, exp)
			s.mu.Unlock()
		}
	}
}

// snapshotForceRequests returns max(requested_at) per exporter from
// the operator-triggered walk queue. Returns nil when no credential
// store is wired or the listing fails — callers must treat a nil map
// as "no forced walks".
func (s *Scheduler) snapshotForceRequests(ctx context.Context) map[string]time.Time {
	if s.creds == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	reqs, err := s.creds.WalkRequests(listCtx)
	if err != nil {
		slog.Warn("snmp: list walk requests", "err", err)
		return nil
	}
	return reqs
}

// isForced returns true if exporter has a pending walk request that
// post-dates its last completed walk.
func (s *Scheduler) isForced(exp string, last time.Time, forced map[string]time.Time) bool {
	req, ok := forced[exp]
	if !ok {
		return false
	}
	return req.After(last)
}

// snapshotIntervals returns a map of exporter → configured walk
// cadence, populated only for exporters that have a binding row with
// interval_sec > 0. Returns nil when no credential store is wired or
// the listing fails — callers must treat a nil map as "no overrides".
func (s *Scheduler) snapshotIntervals(ctx context.Context) map[string]time.Duration {
	if s.creds == nil {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	creds, err := s.creds.List(listCtx)
	if err != nil {
		slog.Warn("snmp: list credential intervals", "err", err)
		return nil
	}
	out := make(map[string]time.Duration, len(creds))
	for _, c := range creds {
		if c.Interval > 0 {
			out[c.Exporter] = c.Interval
		}
	}
	return out
}

// effectiveInterval returns the walk cadence for exp: the per-binding
// override when present, otherwise the cluster-wide default.
func (s *Scheduler) effectiveInterval(exp string, overrides map[string]time.Duration) time.Duration {
	if d, ok := overrides[exp]; ok && d > 0 {
		return d
	}
	return s.interval
}

func (s *Scheduler) walkOne(ctx context.Context, target string) {
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, target)
		s.lastWalked[target] = time.Now()
		s.mu.Unlock()
	}()

	walkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := s.clientFor(walkCtx, target)
	if err != nil {
		slog.Warn("snmp: no credential for exporter and no fallback configured", "exporter", target, "err", err)
		return
	}
	inv, err := client.Walk(walkCtx, target)
	if err != nil {
		slog.Warn("snmp: walk failed", "exporter", target, "err", err)
		return
	}
	if err := s.persist(ctx, inv); err != nil {
		slog.Warn("snmp: persist failed", "exporter", target, "err", err)
		return
	}
	slog.Info("snmp: walked",
		"exporter", target,
		"sys_name", inv.SysName,
		"interfaces", len(inv.Interfaces),
		"duration_ms", inv.PollDurationMs,
		"status", inv.Status,
	)
}

// clientFor returns the SNMP Client to use for target. If a
// per-exporter credential exists, a new RealClient is built from it.
// Otherwise the fallback client is used (typically the cluster-wide
// v2c community or the mock).
func (s *Scheduler) clientFor(ctx context.Context, target string) (Client, error) {
	if s.creds != nil {
		c, err := s.creds.Get(ctx, target)
		if err == nil {
			return NewClient(FromCredential(c)), nil
		}
		// ErrCredNotFound is the common case; anything else is logged.
		if err != ErrCredNotFound {
			slog.Warn("snmp: credential lookup failed", "exporter", target, "err", err)
		}
	}
	if s.fallback != nil {
		return s.fallback, nil
	}
	return nil, fmt.Errorf("no credential and no fallback")
}

// discoverExporters returns the union of:
//   - exporters seen in the flows table over the last 24 hours
//   - exporters configured in the credential store (so a brand-new
//     binding gets walked on the next tick rather than waiting for
//     the device to also start sending flows)
//   - exporters with an outstanding operator walk-request
//
// Order is not meaningful; duplicates are collapsed via a set.
func (s *Scheduler) discoverExporters(ctx context.Context, forced map[string]time.Time) ([]string, error) {
	seen := make(map[string]struct{}, 32)
	const q = `
SELECT DISTINCT IPv6NumToString(exporter)
FROM flows
WHERE observed >= now() - INTERVAL 1 DAY`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmp: discover query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		seen[unmap4in6(raw)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if s.creds != nil {
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		creds, err := s.creds.List(listCtx)
		cancel()
		if err != nil {
			slog.Warn("snmp: list credentials for discovery", "err", err)
		} else {
			for _, c := range creds {
				seen[c.Exporter] = struct{}{}
			}
		}
	}
	for exp := range forced {
		seen[exp] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for exp := range seen {
		out = append(out, exp)
	}
	return out, nil
}

// persist writes one inventory row + one snmp-interfaces row per
// interface. Both tables are append-only; latest snapshot wins via
// argMax at query time.
func (s *Scheduler) persist(ctx context.Context, inv *Inventory) error {
	exp16, err := ipv6Bytes(inv.Exporter)
	if err != nil {
		return err
	}

	if err := s.conn.Exec(ctx,
		`INSERT INTO device_inventory
		   (polled_at, exporter, sys_descr, sys_object_id, sys_uptime_ms,
		    sys_name, sys_location, sys_contact, iface_count,
		    poll_duration_ms, poll_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.PolledAt, exp16, inv.SysDescr, inv.SysObjectID, inv.SysUpTimeMs,
		inv.SysName, inv.SysLocation, inv.SysContact, uint32(len(inv.Interfaces)),
		inv.PollDurationMs, inv.Status,
	); err != nil {
		return fmt.Errorf("insert device_inventory: %w", err)
	}

	if len(inv.Interfaces) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO device_snmp_interfaces")
	if err != nil {
		return fmt.Errorf("prepare iface batch: %w", err)
	}
	for _, i := range inv.Interfaces {
		if err := batch.Append(
			inv.PolledAt, exp16, i.IfIndex,
			i.IfDescr, i.IfAlias, i.IfType, i.IfSpeedBps, i.IfMtu,
			i.AdminStatus, i.OperStatus,
			i.InErrors, i.OutErrors, i.InDiscards, i.OutDiscards,
		); err != nil {
			return fmt.Errorf("append iface: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send iface batch: %w", err)
	}
	return nil
}
