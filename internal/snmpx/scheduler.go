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
type Scheduler struct {
	conn        driver.Conn
	client      Client
	interval    time.Duration
	concurrency int

	mu         sync.Mutex
	inFlight   map[string]bool   // exporter → walking now?
	lastWalked map[string]time.Time
}

// NewScheduler returns a Scheduler that polls each exporter at most
// once per interval, with at most concurrency walks running at any
// instant.
func NewScheduler(conn driver.Conn, client Client, interval time.Duration, concurrency int) *Scheduler {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	return &Scheduler{
		conn:        conn,
		client:      client,
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
func (s *Scheduler) dispatch(ctx context.Context, jobs chan<- string) {
	exporters, err := s.discoverExporters(ctx)
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
		if !last.IsZero() && now.Sub(last) < s.interval {
			s.mu.Unlock()
			continue
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

func (s *Scheduler) walkOne(ctx context.Context, target string) {
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, target)
		s.lastWalked[target] = time.Now()
		s.mu.Unlock()
	}()

	walkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	inv, err := s.client.Walk(walkCtx, target)
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

// discoverExporters reads the distinct set of exporters from the
// flows table over the last 24 hours. SNMP doesn't poll exporters
// that haven't shown up in flow data yet — they're either offline
// or not configured to send flows, and either way SNMP-only inventory
// is out of scope for v0.
func (s *Scheduler) discoverExporters(ctx context.Context) ([]string, error) {
	const q = `
SELECT DISTINCT IPv6NumToString(exporter)
FROM flows
WHERE observed >= now() - INTERVAL 1 DAY`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmp: discover query: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 16)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, unmap4in6(raw))
	}
	return out, rows.Err()
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
