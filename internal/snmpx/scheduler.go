package snmpx

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/obs"
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
	conn        driver.Conn
	creds       CredentialStore
	fallback    Client
	interval    time.Duration
	concurrency int

	// neighborInterval is the per-device cadence for LLDP/CDP walks.
	// Topology is stable, so 5 min is a deliberate slowdown vs the
	// per-device inventory cadence — see VISION.md §3.1 "pollerless-
	// first" and TASKS.md P3 #20.
	neighborInterval time.Duration

	// bgpInterval is the per-device cadence for bgpPeerTable walks.
	// BGP state changes infrequently relative to interface counters
	// (a flapping session is itself the alert signal), so 2 min is
	// chosen as a middle ground: fast enough that BGPNeighborDown
	// doesn't lag a real session drop by long, slow enough that fleet
	// load on real gear stays modest.
	bgpInterval time.Duration

	mu                  sync.Mutex
	inFlight            map[string]bool
	lastWalked          map[string]time.Time
	lastNeighborsWalked map[string]time.Time
	lastBGPWalked       map[string]time.Time
}

// NewScheduler returns a Scheduler. creds may be nil; in that case
// every walk uses fallback. fallback may also be nil; without it the
// scheduler logs and skips any exporter without an explicit
// credential binding.
func NewScheduler(conn driver.Conn, creds CredentialStore, fallback Client, interval time.Duration, concurrency int) *Scheduler {
	if interval <= 0 {
		// 60s default — fast enough that CPU/memory/sensor sparklines on
		// the Devices tab feel live, slow enough that the walk load on
		// real gear stays modest. Per-credential interval_sec still
		// wins for operators who need to dial it back on noisier
		// devices.
		interval = 60 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	return &Scheduler{
		conn:                conn,
		creds:               creds,
		fallback:            fallback,
		interval:            interval,
		concurrency:         concurrency,
		neighborInterval:    5 * time.Minute,
		bgpInterval:         2 * time.Minute,
		inFlight:            make(map[string]bool),
		lastWalked:          make(map[string]time.Time),
		lastNeighborsWalked: make(map[string]time.Time),
		lastBGPWalked:       make(map[string]time.Time),
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
		obs.SNMPWalkFailuresTotal.WithLabelValues(target, "inventory").Inc()
		slog.Warn("snmp: walk failed", "exporter", target, "err", err)
		return
	}
	if err := s.persist(ctx, inv); err != nil {
		obs.SNMPWalkFailuresTotal.WithLabelValues(target, "persist").Inc()
		slog.Warn("snmp: persist failed", "exporter", target, "err", err)
		return
	}

	// Neighbor walk runs on its own slower cadence (5 min by default).
	// Topology is stable so we don't burn LLDP TLVs every 60s. The
	// neighbor walker reuses the ifTable we just walked so it can
	// label local ports without a second pass.
	if s.shouldWalkNeighbors(target) {
		ifTable := make(map[uint32]string, len(inv.Interfaces))
		for _, i := range inv.Interfaces {
			if i.IfDescr != "" {
				ifTable[i.IfIndex] = i.IfDescr
			}
		}
		neighbors, nerr := client.WalkNeighbors(walkCtx, target, ifTable)
		if nerr != nil {
			obs.SNMPLLDPWalkFailuresTotal.WithLabelValues(target).Inc()
			slog.Warn("snmp: neighbor walk failed", "exporter", target, "err", nerr)
		} else {
			if perr := s.persistNeighbors(ctx, target, neighbors); perr != nil {
				obs.SNMPLLDPWalkFailuresTotal.WithLabelValues(target).Inc()
				slog.Warn("snmp: persist neighbors failed", "exporter", target, "err", perr)
			} else {
				obs.SNMPLLDPNeighborsTotal.WithLabelValues(target).Add(float64(len(neighbors)))
				s.mu.Lock()
				s.lastNeighborsWalked[target] = time.Now()
				s.mu.Unlock()
				slog.Info("snmp: neighbors walked",
					"exporter", target,
					"neighbors", len(neighbors),
				)
			}
		}
	}

	// BGP walk runs on its own cadence (2 min by default). We always
	// attempt the walk on first poll so the BGPNeighborDown rule can
	// see freshly-walked devices immediately. Devices that don't speak
	// BGP4-MIB return an empty slice — the walker logs nothing in that
	// path so an L2 switch in the fleet doesn't spam the logs every
	// few minutes.
	if s.shouldWalkBGP(target) {
		peers, berr := client.WalkBGP(walkCtx, target)
		if berr != nil {
			slog.Warn("snmp: bgp walk failed", "exporter", target, "err", berr)
		} else if len(peers) > 0 {
			if perr := s.persistBGPPeers(ctx, peers); perr != nil {
				slog.Warn("snmp: persist bgp peers failed", "exporter", target, "err", perr)
			} else {
				s.mu.Lock()
				s.lastBGPWalked[target] = time.Now()
				s.mu.Unlock()
				slog.Info("snmp: bgp peers walked",
					"exporter", target,
					"peers", len(peers),
				)
			}
		} else {
			// Mark as walked so the empty-result path doesn't re-attempt
			// every tick.
			s.mu.Lock()
			s.lastBGPWalked[target] = time.Now()
			s.mu.Unlock()
		}
	}

	slog.Info("snmp: walked",
		"exporter", target,
		"sys_name", inv.SysName,
		"interfaces", len(inv.Interfaces),
		"duration_ms", inv.PollDurationMs,
		"status", inv.Status,
	)
}

// shouldWalkBGP returns true if target hasn't had a BGP walk in the
// past bgpInterval. First walk always runs.
func (s *Scheduler) shouldWalkBGP(target string) bool {
	s.mu.Lock()
	last := s.lastBGPWalked[target]
	s.mu.Unlock()
	if last.IsZero() {
		return true
	}
	return time.Since(last) >= s.bgpInterval
}

// persistBGPPeers writes one row per peer to bgp_peers. Each walk
// produces a fresh sample (MergeTree append-only, partitioned by day,
// 30-day TTL) so the alert engine reads argMax(state, polled_at) for
// the latest known state per (exporter, peer_addr).
func (s *Scheduler) persistBGPPeers(ctx context.Context, peers []BGPPeer) error {
	if len(peers) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx,
		`INSERT INTO bgp_peers
		   (polled_at, exporter, peer_addr, peer_asn, local_asn,
		    state, admin_status, established_at, last_change_at,
		    afi, safi, peer_description, source)`,
	)
	if err != nil {
		return fmt.Errorf("prepare bgp batch: %w", err)
	}
	for _, p := range peers {
		exp16, eerr := ipv6Bytes(p.Exporter)
		if eerr != nil {
			continue
		}
		peer16, perr := ipv6Bytes(p.PeerAddr)
		if perr != nil {
			continue
		}
		// Zero-time established/last-change land as the schema default
		// (epoch). The read path treats epoch as "unknown" so the UI
		// renders an em-dash rather than 1970.
		est := p.EstablishedAt
		if est.IsZero() {
			est = time.Unix(0, 0).UTC()
		}
		chg := p.LastChangeAt
		if chg.IsZero() {
			chg = time.Unix(0, 0).UTC()
		}
		if err := batch.Append(
			p.PolledAt, exp16, peer16, p.PeerASN, p.LocalASN,
			p.State, p.AdminStatus, est, chg,
			p.AFI, p.SAFI, p.PeerDescription, p.Source,
		); err != nil {
			return fmt.Errorf("append bgp peer: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send bgp batch: %w", err)
	}
	return nil
}

// shouldWalkNeighbors returns true if target hasn't had a neighbor
// walk in the past neighborInterval. First walk always runs (zero
// time is older than any positive interval).
func (s *Scheduler) shouldWalkNeighbors(target string) bool {
	s.mu.Lock()
	last := s.lastNeighborsWalked[target]
	s.mu.Unlock()
	if last.IsZero() {
		return true
	}
	return time.Since(last) >= s.neighborInterval
}

// clientFor returns the SNMP Client to use for target. Resolution
// order:
//
//	1. Per-exporter binding (snmp_credentials)
//	   - binding_kind=custom: use the inline community / v3 fields.
//	   - binding_kind=global_v2c: resolve via snmp_global_defaults v2c.
//	   - binding_kind=global_v3:  resolve via snmp_global_defaults v3.
//	2. Whichever global has default_for_dynamic=1 (operator-controlled;
//	   defaults to v2c when both are flagged, mirroring legacy behavior).
//	3. The env-var fallback client (legacy FLOWSCOPE_SNMP_COMMUNITY).
//
// A misconfigured global indirection (binding points at a global that
// hasn't been configured yet) returns ErrCredNotFound semantics so the
// walk falls through to the fallback rather than failing the device.
func (s *Scheduler) clientFor(ctx context.Context, target string) (Client, error) {
	if s.creds != nil {
		c, err := s.creds.Get(ctx, target)
		if err == nil {
			switch c.BindingKind {
			case BindingKindGlobalV2c:
				if cl := s.clientFromGlobal(ctx, "v2c", c.Port); cl != nil {
					return cl, nil
				}
			case BindingKindGlobalV3:
				if cl := s.clientFromGlobal(ctx, "v3", c.Port); cl != nil {
					return cl, nil
				}
			default: // "" or "custom"
				return NewClient(FromCredential(c)), nil
			}
			// Global indirection requested but unconfigured — fall
			// through to the fallback client below so the device still
			// gets walked.
			slog.Warn("snmp: global default not configured, using fallback",
				"exporter", target, "binding_kind", c.BindingKind)
		} else if err != ErrCredNotFound {
			// ErrCredNotFound is the common case; anything else is logged.
			slog.Warn("snmp: credential lookup failed", "exporter", target, "err", err)
		} else {
			// No per-exporter binding — try whichever global is flagged
			// as the fleet-wide default for dynamic discovery. v2c wins
			// when both are flagged (legacy compat); v3 wins when only
			// v3 is flagged (v3-only deployments). If neither is
			// flagged we fall through to the env-var fallback.
			if cl := s.defaultDynamicClient(ctx); cl != nil {
				return cl, nil
			}
		}
	}
	if s.fallback != nil {
		return s.fallback, nil
	}
	return nil, fmt.Errorf("no credential and no fallback")
}

// defaultDynamicClient picks the global to use for an exporter that
// has no per-exporter binding. Looks at the default_for_dynamic flag
// on each global and prefers v2c when both are flagged.
func (s *Scheduler) defaultDynamicClient(ctx context.Context) Client {
	v2c, _ := s.creds.GetGlobal(ctx, "v2c")
	if v2c != nil && v2c.Configured && v2c.DefaultForDynamic {
		return NewClient(FromGlobalDefault(v2c))
	}
	v3, _ := s.creds.GetGlobal(ctx, "v3")
	if v3 != nil && v3.Configured && v3.DefaultForDynamic {
		return NewClient(FromGlobalDefault(v3))
	}
	return nil
}

// clientFromGlobal builds a Client from the v2c or v3 global default.
// Returns nil when the global has not been configured (Configured=false),
// or when a permission / decryption error occurs — callers fall through
// to the next resolution step. portOverride is non-zero when the
// per-exporter binding specified a custom port for this device.
func (s *Scheduler) clientFromGlobal(ctx context.Context, role string, portOverride uint16) Client {
	g, err := s.creds.GetGlobal(ctx, role)
	if err != nil {
		slog.Warn("snmp: global lookup failed", "role", role, "err", err)
		return nil
	}
	if g == nil || !g.Configured {
		return nil
	}
	cfg := FromGlobalDefault(g)
	if portOverride > 0 {
		cfg.Port = portOverride
	}
	return NewClient(cfg)
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
		    poll_duration_ms, poll_status, snmp_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.PolledAt, exp16, inv.SysDescr, inv.SysObjectID, inv.SysUpTimeMs,
		inv.SysName, inv.SysLocation, inv.SysContact, uint32(len(inv.Interfaces)),
		inv.PollDurationMs, inv.Status, inv.SNMPVersion,
	); err != nil {
		return fmt.Errorf("insert device_inventory: %w", err)
	}

	if len(inv.Interfaces) > 0 {
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
	}

	if len(inv.Resources) > 0 {
		batch, err := s.conn.PrepareBatch(ctx,
			`INSERT INTO device_resource_samples
			   (polled_at, exporter, kind, component,
			    value_percent, value_bytes, max_bytes, value_numeric, unit, source)`,
		)
		if err != nil {
			return fmt.Errorf("prepare resource batch: %w", err)
		}
		for _, r := range inv.Resources {
			if err := batch.Append(
				inv.PolledAt, exp16, string(r.Kind), r.Component,
				r.ValuePercent, r.ValueBytes, r.MaxBytes,
				r.ValueNumeric, r.Unit, string(r.Source),
			); err != nil {
				return fmt.Errorf("append resource: %w", err)
			}
		}
		if err := batch.Send(); err != nil {
			return fmt.Errorf("send resource batch: %w", err)
		}
	}
	return nil
}

// persistNeighbors writes one row per neighbor to lldp_neighbors. The
// table is a ReplacingMergeTree on last_seen so repeated walks
// "overwrite" the previous snapshot at merge time. first_seen is
// populated from the existing row when present (so we preserve the
// original discovery timestamp), defaulting to last_seen on insert.
//
// Empty neighbors slice is fine — we still write zero rows and
// touch nothing. The caller has already incremented the metric.
func (s *Scheduler) persistNeighbors(ctx context.Context, exporter string, neighbors []Neighbor) error {
	if len(neighbors) == 0 {
		return nil
	}
	exp16, err := ipv6Bytes(exporter)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	// Fetch existing first_seen per (local_ifindex, discovery_proto,
	// remote_chassis_id, remote_port_id) so a re-discovered neighbor
	// keeps its original "first seen" timestamp. A single query is
	// cheap on the small per-device key set.
	type fsKey struct {
		ifIdx   uint32
		proto   string
		chassis string
		portID  string
	}
	existing := make(map[fsKey]time.Time, len(neighbors))
	rows, err := s.conn.Query(ctx,
		`SELECT local_ifindex, discovery_proto, remote_chassis_id, remote_port_id, min(first_seen)
		 FROM lldp_neighbors FINAL
		 WHERE local_exporter = ?
		 GROUP BY local_ifindex, discovery_proto, remote_chassis_id, remote_port_id`,
		exp16,
	)
	if err == nil {
		for rows.Next() {
			var k fsKey
			var fs time.Time
			if err := rows.Scan(&k.ifIdx, &k.proto, &k.chassis, &k.portID, &fs); err == nil {
				existing[k] = fs
			}
		}
		rows.Close()
	}

	batch, err := s.conn.PrepareBatch(ctx,
		`INSERT INTO lldp_neighbors
		   (last_seen, first_seen, local_exporter, local_ifindex, local_port_name,
		    discovery_proto, remote_chassis_id, remote_sys_name, remote_sys_desc,
		    remote_port_id, remote_capabilities, remote_management_addr)`,
	)
	if err != nil {
		return fmt.Errorf("prepare neighbor batch: %w", err)
	}
	for _, n := range neighbors {
		fs := now
		k := fsKey{ifIdx: n.LocalIfIndex, proto: n.DiscoveryProto, chassis: n.RemoteChassisID, portID: n.RemotePortID}
		if prev, ok := existing[k]; ok && !prev.IsZero() {
			fs = prev
		}
		// Nullable(IPv6) wants a *net.IP. Empty string means "no
		// management address known"; pass nil so the column lands as
		// NULL rather than the v4-mapped zero address.
		var mgmt any
		if n.RemoteManagementAddr != "" {
			if ip, e2 := ipv6Bytes(n.RemoteManagementAddr); e2 == nil {
				mgmt = ip
			}
		}
		if err := batch.Append(
			now, fs, exp16, n.LocalIfIndex, n.LocalPortName,
			n.DiscoveryProto, n.RemoteChassisID, n.RemoteSysName, n.RemoteSysDesc,
			n.RemotePortID, n.RemoteCapabilities, mgmt,
		); err != nil {
			return fmt.Errorf("append neighbor: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send neighbor batch: %w", err)
	}
	return nil
}
