package store

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TimeRange represents either a trailing window (From.IsZero(), To.IsZero(),
// Window > 0) or an absolute range (both From and To set). Queries use
// Predicate() to get the SQL fragment and bound args.
type TimeRange struct {
	Window time.Duration // trailing window; used when From/To are zero
	From   time.Time     // absolute start (inclusive)
	To     time.Time     // absolute end (exclusive)
}

// TrailingWindow returns a TimeRange for a trailing window of the given
// duration. This is the backwards-compatible default.
func TrailingWindow(d time.Duration) TimeRange {
	if d <= 0 {
		d = 5 * time.Minute
	}
	return TimeRange{Window: d}
}

// AbsoluteRange returns a TimeRange for an absolute [from, to) interval.
// The span is clamped to 168h (7 days) to keep ClickHouse queries bounded.
func AbsoluteRange(from, to time.Time) TimeRange {
	if to.Before(from) {
		from, to = to, from
	}
	const maxSpan = 168 * time.Hour
	if to.Sub(from) > maxSpan {
		from = to.Add(-maxSpan)
	}
	return TimeRange{From: from, To: to}
}

// IsAbsolute returns true when From and To are both set.
func (tr TimeRange) IsAbsolute() bool {
	return !tr.From.IsZero() && !tr.To.IsZero()
}

// Seconds returns the span in seconds — either the window duration or
// the absolute range span.
func (tr TimeRange) Seconds() float64 {
	if tr.IsAbsolute() {
		return tr.To.Sub(tr.From).Seconds()
	}
	return tr.Window.Seconds()
}

// WindowDuration returns the effective window for display purposes.
func (tr TimeRange) WindowDuration() time.Duration {
	if tr.IsAbsolute() {
		return tr.To.Sub(tr.From)
	}
	return tr.Window
}

// Predicate returns the SQL WHERE fragment and bound args for the time
// column named col (e.g. "observed" or "ts").
func (tr TimeRange) Predicate(col string) (string, []any) {
	if tr.IsAbsolute() {
		return col + " >= ? AND " + col + " < ?", []any{tr.From, tr.To}
	}
	w := tr.Window
	if w <= 0 {
		w = 5 * time.Minute
	}
	return col + " >= now() - INTERVAL ? SECOND", []any{uint64(w.Seconds())}
}

// SQL fragments that compute the latest SNMP enrichment per
// (exporter) and per (exporter, ifindex). Inlined into per-query
// CTEs so we don't depend on a SELECT FINAL on the underlying
// MergeTrees (which are append-only by design — see VISION.md §4.2
// and migration 000003_snmp.sql).
const sqlLatestInventory = `
SELECT exporter,
       argMax(sys_name, polled_at)     AS sys_name,
       argMax(sys_location, polled_at) AS sys_location
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 7 DAY
GROUP BY exporter`

const sqlLatestSNMPInterfaces = `
SELECT exporter, ifindex,
       argMax(if_descr, polled_at) AS if_descr,
       argMax(if_alias, polled_at) AS if_alias
FROM device_snmp_interfaces
WHERE polled_at >= now() - INTERVAL 7 DAY
GROUP BY exporter, ifindex`

// Summary captures the aggregate view used by /api/summary and the
// Overview tab. All counts are computed over the trailing window
// passed to QuerySummary.
type Summary struct {
	Window     time.Duration `json:"window"`
	Flows      uint64        `json:"flows"`
	Bytes      uint64        `json:"bytes"`
	Packets    uint64        `json:"packets"`
	Exporters  uint64        `json:"exporters"`
	Newest     time.Time     `json:"newest"`
	Oldest     time.Time     `json:"oldest"`
}

// InterfaceRow is the row shape returned by /api/interfaces. It
// summarises one (exporter, ifindex) pair over the requested window:
// the most recent counter sample plus the trailing-window peak rate.
type InterfaceRow struct {
	Exporter      string    `json:"exporter"`
	SysName       string    `json:"sys_name"` // populated when SNMP has walked the exporter
	IfIndex       uint32    `json:"ifindex"`
	IfDescr       string    `json:"if_descr"` // e.g. Te1/0/47, populated when SNMP has walked
	IfAlias       string    `json:"if_alias"` // operator description, optional
	LastSeen      time.Time `json:"last_seen"`
	InBpsLatest   uint64    `json:"in_bps_latest"`
	OutBpsLatest  uint64    `json:"out_bps_latest"`
	InBpsPeak     uint64    `json:"in_bps_peak"`
	OutBpsPeak    uint64    `json:"out_bps_peak"`
	Source        string    `json:"source"`
}

// InterfaceTimeseries is the JSON-friendly response for
// /api/interfaces/{exp}/{ifindex}/timeseries. Source is always
// "counters" today; once flow-bucketed fallback lands the field will
// switch to "flows" when no counter samples are available
// (VISION.md §3.3).
type InterfaceTimeseries struct {
	Exporter      string                  `json:"exporter"`
	SysName       string                  `json:"sys_name"`
	IfIndex       uint32                  `json:"ifindex"`
	IfDescr       string                  `json:"if_descr"`
	IfAlias       string                  `json:"if_alias"`
	WindowSeconds int                     `json:"window_seconds"`
	Source        string                  `json:"source"`
	Points        []InterfaceTimeseriesPt `json:"points"`
}

type InterfaceTimeseriesPt struct {
	Ts     time.Time `json:"ts"`
	InBps  uint64    `json:"in_bps"`
	OutBps uint64    `json:"out_bps"`
}

// RecentFlow is the row shape returned by /api/flows/recent. It mirrors
// the canonical record.Flow but with JSON-friendly types. TCPFlags is
// the OR of flags observed across the flow's lifetime — useful for
// the drawer raw-record view's flag-decode badge. 0 for non-TCP
// records.
type RecentFlow struct {
	Observed       time.Time `json:"observed"`
	Exporter       string    `json:"exporter"`
	ExporterName   string    `json:"exporter_name"` // sys_name when SNMP has walked
	SrcAddr        string    `json:"src_addr"`
	DstAddr        string    `json:"dst_addr"`
	SrcPort        uint16    `json:"src_port"`
	DstPort        uint16    `json:"dst_port"`
	Proto          uint8     `json:"proto"`
	Bytes          uint64    `json:"bytes"`
	Packets        uint64    `json:"packets"`
	InputIfIndex   uint32    `json:"input_ifindex"`
	OutputIfIndex  uint32    `json:"output_ifindex"`
	SrcAS          uint32    `json:"src_as"`
	DstAS          uint32    `json:"dst_as"`
	TCPFlags       uint8     `json:"tcp_flags"`
	Source         string    `json:"source"`
}

// StorageHealth captures ClickHouse-side write health for the
// Overview Storage panel. Insert lag is the gap between now and the
// most recent flow row's observed timestamp — under healthy ingest
// it stays under a second. Recent rate is rows/sec computed over
// the last 60s.
type StorageHealth struct {
	InsertLagSeconds   float64   `json:"insert_lag_seconds"`
	RowsPerSecRecent   float64   `json:"rows_per_sec_recent"`
	RowsLast60s        uint64    `json:"rows_last_60s"`
	NewestObserved     time.Time `json:"newest_observed"`
	OldestObserved     time.Time `json:"oldest_observed"`
	FlowsRows          uint64    `json:"flows_rows_estimate"`
	IfaceCounterRows   uint64    `json:"iface_counter_samples_rows_estimate"`
	DeviceInventoryRows uint64   `json:"device_inventory_rows_estimate"`
}

// QueryStorageHealth returns first-class write/lag indicators read
// from ClickHouse system tables and the flows table directly.
// Cheap — bounded scans on small system tables and an aggregate
// over the trailing minute on flows.
func QueryStorageHealth(ctx context.Context, conn driver.Conn) (StorageHealth, error) {
	var h StorageHealth
	row := conn.QueryRow(ctx, `
SELECT
    coalesce(toUnixTimestamp(now()) - toUnixTimestamp(max(observed)), 0) AS lag_seconds,
    coalesce(countIf(observed >= now() - INTERVAL 60 SECOND) / 60.0, 0) AS rate_recent,
    coalesce(countIf(observed >= now() - INTERVAL 60 SECOND), 0) AS rows_60s,
    coalesce(max(observed), toDateTime(0)) AS newest,
    coalesce(min(observed), toDateTime(0)) AS oldest
FROM flows
WHERE observed >= now() - INTERVAL 24 HOUR`)
	if err := row.Scan(
		&h.InsertLagSeconds,
		&h.RowsPerSecRecent,
		&h.RowsLast60s,
		&h.NewestObserved,
		&h.OldestObserved,
	); err != nil {
		return h, fmt.Errorf("store: query storage health flows: %w", err)
	}
	// Per-table row count estimates. system.tables.total_rows is an
	// estimate (replicated/sharded clusters report partial values),
	// fine for the Storage panel.
	rows, err := conn.Query(ctx, `
SELECT name, total_rows
FROM system.tables
WHERE database = currentDatabase()
  AND name IN ('flows', 'iface_counter_samples', 'device_inventory')`)
	if err != nil {
		return h, fmt.Errorf("store: query storage tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var total uint64
		if err := rows.Scan(&name, &total); err != nil {
			return h, fmt.Errorf("store: scan storage table: %w", err)
		}
		switch name {
		case "flows":
			h.FlowsRows = total
		case "iface_counter_samples":
			h.IfaceCounterRows = total
		case "device_inventory":
			h.DeviceInventoryRows = total
		}
	}
	return h, rows.Err()
}

// ExporterHealthRow is one (exporter, source) aggregate over the
// trailing window. LossPct = seq_gaps / (seq_gaps + datagrams) when
// datagrams > 0; 0 when there's been no traffic.
type ExporterHealthRow struct {
	Exporter   string  `json:"exporter"`
	SysName    string  `json:"sys_name"`
	Source     string  `json:"source"`
	Datagrams  uint64  `json:"datagrams"`
	SeqGaps    uint64  `json:"seq_gaps"`
	LossPct    float64 `json:"loss_pct"`
	LastSeen   time.Time `json:"last_seen"`
}

// QueryExporterHealth aggregates per-(exporter, source) datagram +
// gap counts from the exporter_health table over the time range.
// LEFT JOINs SNMP inventory for the human sys_name.
func QueryExporterHealth(ctx context.Context, conn driver.Conn, tr TimeRange) ([]ExporterHealthRow, error) {
	pred, args := tr.Predicate("ts")
	q := `
WITH inv AS (` + sqlLatestInventory + `)
SELECT
    e.exporter,
    ifNull(inv.sys_name, '') AS sys_name,
    e.source,
    sum(e.datagrams) AS datagrams,
    sum(e.seq_gaps)  AS seq_gaps,
    max(e.ts)        AS last_seen
FROM exporter_health AS e
LEFT JOIN inv ON e.exporter = inv.exporter
WHERE ` + pred + `
GROUP BY e.exporter, sys_name, e.source
ORDER BY seq_gaps DESC, datagrams DESC`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query exporter health: %w", err)
	}
	defer rows.Close()
	out := make([]ExporterHealthRow, 0, 16)
	for rows.Next() {
		var r ExporterHealthRow
		var exp netip.Addr
		if err := rows.Scan(&exp, &r.SysName, &r.Source, &r.Datagrams, &r.SeqGaps, &r.LastSeen); err != nil {
			return nil, fmt.Errorf("store: scan exporter health row: %w", err)
		}
		r.Exporter = exp.Unmap().String()
		denom := r.Datagrams + r.SeqGaps
		if denom > 0 {
			r.LossPct = float64(r.SeqGaps) / float64(denom) * 100
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SourceBreakdown is one row per ingest source label observed in the
// flows table over the window — typically "netflow_v5", "netflow_v9",
// "ipfix", "sflow", "gnmi". The Overview tab uses this to show stream
// health per protocol family.
type SourceBreakdown struct {
	Source    string `json:"source"`
	Flows     uint64 `json:"flows"`
	Bytes     uint64 `json:"bytes"`
	Packets   uint64 `json:"packets"`
	Exporters uint64 `json:"exporters"`
}

// QuerySourceBreakdown returns one row per ingest source observed
// over the supplied time range, ordered by flow count desc.
func QuerySourceBreakdown(ctx context.Context, conn driver.Conn, tr TimeRange) ([]SourceBreakdown, error) {
	pred, args := tr.Predicate("observed")
	q := `
SELECT source,
       count()        AS flows,
       sum(bytes)     AS bytes,
       sum(packets)   AS packets,
       uniq(exporter) AS exporters
FROM flows
WHERE ` + pred + `
GROUP BY source
ORDER BY flows DESC`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query source breakdown: %w", err)
	}
	defer rows.Close()
	out := make([]SourceBreakdown, 0, 4)
	for rows.Next() {
		var s SourceBreakdown
		if err := rows.Scan(&s.Source, &s.Flows, &s.Bytes, &s.Packets, &s.Exporters); err != nil {
			return nil, fmt.Errorf("store: scan source breakdown: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QuerySummary returns aggregate stats over the supplied time range.
func QuerySummary(ctx context.Context, conn driver.Conn, tr TimeRange) (Summary, error) {
	pred, args := tr.Predicate("observed")
	q := `
SELECT
    count() AS flows,
    sum(bytes) AS bytes,
    sum(packets) AS packets,
    uniq(exporter) AS exporters,
    max(observed) AS newest,
    min(observed) AS oldest
FROM flows
WHERE ` + pred
	row := conn.QueryRow(ctx, q, args...)
	var s Summary
	s.Window = tr.WindowDuration()
	if err := row.Scan(&s.Flows, &s.Bytes, &s.Packets, &s.Exporters, &s.Newest, &s.Oldest); err != nil {
		return s, fmt.Errorf("store: query summary: %w", err)
	}
	return s, nil
}

// QueryInterfaces lists (exporter, ifindex) pairs that produced at
// least one counter sample in the trailing window, ordered by most
// recent peak in_bps + out_bps. Rates come from successive-sample
// diffs — see comment in QueryInterfaceTimeseries.
//
// If exporter is non-empty, the result is filtered to that single
// exporter.
func QueryInterfaces(ctx context.Context, conn driver.Conn, tr TimeRange, exporter string) ([]InterfaceRow, error) {
	tsPred, args := tr.Predicate("ts")
	exporterPredicate := ""
	if exporter != "" {
		addr, err := netip.ParseAddr(exporter)
		if err != nil {
			return nil, fmt.Errorf("store: invalid exporter address: %w", err)
		}
		exporterPredicate = " AND exporter = ?"
		args = append(args, toIPv6(addr))
	}
	q := `
WITH
diffed AS (
    SELECT
        ts,
        exporter,
        ifindex,
        toFloat64(in_octets - lagInFrame(in_octets) OVER w) AS d_in,
        toFloat64(out_octets - lagInFrame(out_octets) OVER w) AS d_out,
        date_diff('millisecond', lagInFrame(ts) OVER w, ts) AS dt_ms
    FROM iface_counter_samples
    WHERE ` + tsPred + exporterPredicate + `
    WINDOW w AS (PARTITION BY exporter, ifindex ORDER BY ts)
),
agg AS (
    SELECT
        exporter,
        ifindex,
        max(ts) AS last_seen,
        toUInt64(argMax(if(d_in >= 0 AND dt_ms > 0, d_in * 8000 / dt_ms, 0), ts))  AS in_latest,
        toUInt64(argMax(if(d_out >= 0 AND dt_ms > 0, d_out * 8000 / dt_ms, 0), ts)) AS out_latest,
        toUInt64(max(if(d_in >= 0 AND dt_ms > 0, d_in * 8000 / dt_ms, 0)))  AS in_peak,
        toUInt64(max(if(d_out >= 0 AND dt_ms > 0, d_out * 8000 / dt_ms, 0))) AS out_peak
    FROM diffed
    WHERE dt_ms > 0
    GROUP BY exporter, ifindex
),
inv AS (` + sqlLatestInventory + `),
sif AS (` + sqlLatestSNMPInterfaces + `)
SELECT
    a.exporter,
    ifNull(inv.sys_name, '') AS sys_name,
    a.ifindex,
    ifNull(sif.if_descr, '') AS if_descr,
    ifNull(sif.if_alias, '') AS if_alias,
    a.last_seen, a.in_latest, a.out_latest, a.in_peak, a.out_peak
FROM agg AS a
LEFT JOIN inv ON a.exporter = inv.exporter
LEFT JOIN sif ON a.exporter = sif.exporter AND a.ifindex = sif.ifindex
ORDER BY (a.in_peak + a.out_peak) DESC
LIMIT 50`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query interfaces: %w", err)
	}
	defer rows.Close()
	out := make([]InterfaceRow, 0, 16)
	for rows.Next() {
		var (
			r        InterfaceRow
			exporter netip.Addr
		)
		if err := rows.Scan(
			&exporter, &r.SysName, &r.IfIndex, &r.IfDescr, &r.IfAlias,
			&r.LastSeen, &r.InBpsLatest, &r.OutBpsLatest, &r.InBpsPeak, &r.OutBpsPeak,
		); err != nil {
			return nil, fmt.Errorf("store: scan interface: %w", err)
		}
		r.Exporter = exporter.Unmap().String()
		r.Source = "counters"
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryInterfaceTimeseries returns successive-sample bytes/sec rates
// for one (exporter, ifindex) over the trailing window. Counter
// samples carry ABSOLUTE octets; rate-per-second comes from differing
// adjacent samples and dividing by the inter-sample interval.
// Negative diffs (counter rollover, device reboot) are clamped to
// zero.
func QueryInterfaceTimeseries(ctx context.Context, conn driver.Conn, exporter netip.Addr, ifindex uint32, tr TimeRange) (*InterfaceTimeseries, error) {
	tsPred, tsArgs := tr.Predicate("ts")
	q := `
WITH diffed AS (
    SELECT
        ts,
        toFloat64(in_octets - lagInFrame(in_octets) OVER w) AS d_in,
        toFloat64(out_octets - lagInFrame(out_octets) OVER w) AS d_out,
        date_diff('millisecond', lagInFrame(ts) OVER w, ts) AS dt_ms
    FROM iface_counter_samples
    WHERE exporter = ? AND ifindex = ?
      AND ` + tsPred + `
    WINDOW w AS (ORDER BY ts)
)
SELECT
    ts,
    toUInt64(if(d_in  >= 0 AND dt_ms > 0, d_in  * 8000 / dt_ms, 0)) AS in_bps,
    toUInt64(if(d_out >= 0 AND dt_ms > 0, d_out * 8000 / dt_ms, 0)) AS out_bps
FROM diffed
WHERE dt_ms > 0
ORDER BY ts`
	args := append([]any{toIPv6(exporter), ifindex}, tsArgs...)
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query interface timeseries: %w", err)
	}
	defer rows.Close()
	out := &InterfaceTimeseries{
		Exporter:      exporter.Unmap().String(),
		IfIndex:       ifindex,
		WindowSeconds: int(tr.Seconds()),
		Source:        "counters",
		Points:        make([]InterfaceTimeseriesPt, 0, 64),
	}
	for rows.Next() {
		var p InterfaceTimeseriesPt
		if err := rows.Scan(&p.Ts, &p.InBps, &p.OutBps); err != nil {
			return nil, fmt.Errorf("store: scan ts point: %w", err)
		}
		out.Points = append(out.Points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// SNMP enrichment — non-fatal if missing.
	const qm = `
SELECT
    argMax(sys_name, polled_at)
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 7 DAY AND exporter = ?
GROUP BY exporter`
	_ = conn.QueryRow(ctx, qm, toIPv6(exporter)).Scan(&out.SysName)

	const qif = `
SELECT
    argMax(if_descr, polled_at),
    argMax(if_alias, polled_at)
FROM device_snmp_interfaces
WHERE polled_at >= now() - INTERVAL 7 DAY AND exporter = ? AND ifindex = ?
GROUP BY exporter, ifindex`
	_ = conn.QueryRow(ctx, qif, toIPv6(exporter), ifindex).Scan(&out.IfDescr, &out.IfAlias)
	return out, nil
}

// Device is one exporter's traffic summary over a window. Returned by
// /api/devices. The platform infers exporters from observed flows;
// SNMP-driven inventory enrichment (model, OS, uptime, location)
// arrives in a later slice.
type Device struct {
	Exporter    string    `json:"exporter"`
	SysName     string    `json:"sys_name"`     // populated from device_inventory when SNMP has walked
	SysLocation string    `json:"sys_location"` // populated from device_inventory when SNMP has walked; left-rail uses it to group exporters by site
	Flows       uint64    `json:"flows"`
	Bytes       uint64    `json:"bytes"`
	Packets     uint64    `json:"packets"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`   // epoch when SNMP knows the device but no flow records in window
	IfaceCount  uint64    `json:"iface_count"`
	LastWalked  time.Time `json:"last_walked"` // latest polled_at from device_inventory; epoch when never walked
}

// QueryDevices lists every exporter known to FlowScope, ranked by
// total flow bytes in the window. The list is the union of:
//
//   - exporters that produced flow records in the trailing window
//   - exporters present in device_inventory (SNMP has walked them
//     within the 7-day retention) regardless of recent flow activity
//
// SNMP-only rows have flow stats of zero and last_seen at epoch — the
// UI uses that to render them as "discovered" (walked but silent)
// instead of dropping them. iface_count is the number of unique
// ifindex values that produced counter samples in the same window —
// populated only for sFlow / gNMI-capable exporters; zero otherwise.
// last_walked is the latest polled_at from device_inventory; epoch
// when SNMP has not walked this exporter yet.
func QueryDevices(ctx context.Context, conn driver.Conn, tr TimeRange) ([]Device, error) {
	obsPred, obsArgs := tr.Predicate("observed")
	tsPred, tsArgs := tr.Predicate("ts")
	q := `
WITH
  inv AS (
    SELECT exporter,
           argMax(sys_name, polled_at)     AS sys_name,
           argMax(sys_location, polled_at) AS sys_location,
           max(polled_at)                  AS last_polled
    FROM device_inventory
    WHERE polled_at >= now() - INTERVAL 7 DAY
    GROUP BY exporter
  ),
  flow_agg AS (
    SELECT
        exporter,
        count() AS flows,
        sum(bytes)   AS bytes,
        sum(packets) AS packets,
        min(observed) AS first_seen,
        max(observed) AS last_seen
    FROM flows
    WHERE ` + obsPred + `
    GROUP BY exporter
  ),
  iface_agg AS (
    SELECT exporter, uniq(ifindex) AS iface_count
    FROM iface_counter_samples
    WHERE ` + tsPred + `
    GROUP BY exporter
  ),
  all_exporters AS (
    SELECT exporter FROM (
        SELECT exporter FROM flow_agg
        UNION ALL
        SELECT exporter FROM inv
    )
    GROUP BY exporter
  )
SELECT
    a.exporter,
    ifNull(inv.sys_name, '')     AS sys_name,
    ifNull(inv.sys_location, '') AS sys_location,
    ifNull(f.flows,   toUInt64(0)) AS flows,
    ifNull(f.bytes,   toUInt64(0)) AS bytes,
    ifNull(f.packets, toUInt64(0)) AS packets,
    ifNull(f.first_seen, toDateTime64('1970-01-01 00:00:00', 3, 'UTC')) AS first_seen,
    ifNull(f.last_seen,  toDateTime64('1970-01-01 00:00:00', 3, 'UTC')) AS last_seen,
    ifNull(i.iface_count, toUInt64(0)) AS iface_count,
    ifNull(inv.last_polled, toDateTime64('1970-01-01 00:00:00', 3, 'UTC')) AS last_walked
FROM all_exporters AS a
LEFT JOIN flow_agg  AS f ON a.exporter = f.exporter
LEFT JOIN iface_agg AS i ON a.exporter = i.exporter
LEFT JOIN inv             ON a.exporter = inv.exporter
ORDER BY bytes DESC, sys_name, a.exporter`
	args := append([]any{}, obsArgs...)
	args = append(args, tsArgs...)
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query devices: %w", err)
	}
	defer rows.Close()
	out := make([]Device, 0, 16)
	for rows.Next() {
		var (
			d        Device
			exporter netip.Addr
		)
		if err := rows.Scan(
			&exporter,
			&d.SysName, &d.SysLocation,
			&d.Flows, &d.Bytes, &d.Packets,
			&d.FirstSeen, &d.LastSeen,
			&d.IfaceCount,
			&d.LastWalked,
		); err != nil {
			return nil, fmt.Errorf("store: scan device: %w", err)
		}
		d.Exporter = exporter.Unmap().String()
		out = append(out, d)
	}
	return out, rows.Err()
}

// QueryDevice returns the same shape as one row of QueryDevices,
// scoped to the supplied exporter address. Returns ErrNotFound only
// when both the flows table AND device_inventory have no record of
// the exporter within retention. SNMP-only devices return a row with
// zeroed flow stats and a populated last_walked — the api maps this
// to 200 so the UI can render the device detail without a hard 404.
func QueryDevice(ctx context.Context, conn driver.Conn, exporter netip.Addr, tr TimeRange) (*Device, error) {
	obsPred, obsArgs := tr.Predicate("observed")
	q := `
SELECT
    count() AS flows,
    sum(bytes)   AS bytes,
    sum(packets) AS packets,
    min(observed) AS first_seen,
    max(observed) AS last_seen
FROM flows
WHERE ` + obsPred + ` AND exporter = ?
GROUP BY exporter`
	expIP := toIPv6(exporter)
	args := append([]any{}, obsArgs...)
	args = append(args, expIP)
	row := conn.QueryRow(ctx, q, args...)
	var (
		d           Device
		hasFlowRow  bool
	)
	if err := row.Scan(&d.Flows, &d.Bytes, &d.Packets, &d.FirstSeen, &d.LastSeen); err != nil {
		if !isNoRows(err) {
			return nil, fmt.Errorf("store: query device: %w", err)
		}
	} else {
		hasFlowRow = true
	}
	d.Exporter = exporter.Unmap().String()

	// Latest SNMP sys_name + sys_location + polled_at for this exporter.
	// Non-fatal if SNMP has not yet walked.
	const qn = `
SELECT
    argMax(sys_name, polled_at)     AS sys_name,
    argMax(sys_location, polled_at) AS sys_location,
    max(polled_at)                  AS last_polled
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 7 DAY AND exporter = ?
GROUP BY exporter`
	hasInvRow := true
	if err := conn.QueryRow(ctx, qn, expIP).Scan(&d.SysName, &d.SysLocation, &d.LastWalked); err != nil {
		if !isNoRows(err) {
			return nil, fmt.Errorf("store: query device inventory: %w", err)
		}
		hasInvRow = false
		d.SysName = ""
		d.SysLocation = ""
	}

	// Neither flows nor inventory knew about this exporter — 404.
	if !hasFlowRow && !hasInvRow {
		return nil, ErrNotFound
	}

	// Interface count for this exporter.
	tsPred, tsArgs := tr.Predicate("ts")
	qi := `
SELECT uniq(ifindex)
FROM iface_counter_samples
WHERE ` + tsPred + ` AND exporter = ?`
	ifaceArgs := append([]any{}, tsArgs...)
	ifaceArgs = append(ifaceArgs, expIP)
	if err := conn.QueryRow(ctx, qi, ifaceArgs...).Scan(&d.IfaceCount); err != nil {
		// Non-fatal; counter samples may not exist for NetFlow-only
		// exporters.
		d.IfaceCount = 0
	}
	return &d, nil
}

// ErrNotFound is returned by single-row queries when the row does not
// exist. The api layer maps this to HTTP 404.
var ErrNotFound = fmt.Errorf("not found")

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	// clickhouse-go scans return "sql: no rows in result set" via the
	// stdlib database/sql sentinel; matching by string keeps us free
	// of a database/sql import here.
	return err.Error() == "sql: no rows in result set"
}

// DeviceInventory is the latest SNMP-derived snapshot for one
// exporter, joined with the per-interface SNMP attributes. Returned
// by /api/devices/{exporter}/inventory and rendered on the Devices
// tab Summary sub-tab.
type DeviceInventory struct {
	PolledAt       time.Time         `json:"polled_at"`
	Exporter       string            `json:"exporter"`
	SysDescr       string            `json:"sys_descr"`
	SysObjectID    string            `json:"sys_object_id"`
	SysUptimeMs    uint64            `json:"sys_uptime_ms"`
	SysName        string            `json:"sys_name"`
	SysLocation    string            `json:"sys_location"`
	SysContact     string            `json:"sys_contact"`
	IfaceCount     uint32            `json:"iface_count"`
	PollDurationMs uint32            `json:"poll_duration_ms"`
	PollStatus     string            `json:"poll_status"`
	// SNMPVersion is the wire version used to walk this device on the
	// latest poll ('v2c' | 'v3'). Empty string for rows written before
	// migration 000016 added the column.
	SNMPVersion    string            `json:"snmp_version"`
	Interfaces     []SNMPInterface   `json:"interfaces"`
}

// SNMPInterface mirrors a row from device_snmp_interfaces.
type SNMPInterface struct {
	IfIndex     uint32 `json:"ifindex"`
	IfDescr     string `json:"if_descr"`
	IfAlias     string `json:"if_alias"`
	IfType      uint32 `json:"if_type"`
	IfSpeedBps  uint64 `json:"if_speed_bps"`
	IfMtu       uint32 `json:"if_mtu"`
	AdminStatus string `json:"admin_status"`
	OperStatus  string `json:"oper_status"`
	InErrors    uint64 `json:"in_errors"`
	OutErrors   uint64 `json:"out_errors"`
	InDiscards  uint64 `json:"in_discards"`
	OutDiscards uint64 `json:"out_discards"`
}

// QueryDeviceInventory returns the freshest SNMP snapshot for an
// exporter plus all interfaces from the same poll. Returns
// ErrNotFound when SNMP has never walked this device.
func QueryDeviceInventory(ctx context.Context, conn driver.Conn, exporter netip.Addr) (*DeviceInventory, error) {
	expIP := toIPv6(exporter)

	const q = `
SELECT
    polled_at, sys_descr, sys_object_id, sys_uptime_ms,
    sys_name, sys_location, sys_contact, iface_count,
    poll_duration_ms, poll_status,
    ifNull(snmp_version, '') AS snmp_version
FROM device_inventory
WHERE exporter = ?
ORDER BY polled_at DESC
LIMIT 1`
	row := conn.QueryRow(ctx, q, expIP)
	var inv DeviceInventory
	if err := row.Scan(
		&inv.PolledAt, &inv.SysDescr, &inv.SysObjectID, &inv.SysUptimeMs,
		&inv.SysName, &inv.SysLocation, &inv.SysContact, &inv.IfaceCount,
		&inv.PollDurationMs, &inv.PollStatus,
		&inv.SNMPVersion,
	); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: query inventory: %w", err)
	}
	inv.Exporter = exporter.Unmap().String()

	const qi = `
SELECT
    ifindex, if_descr, if_alias, if_type, if_speed_bps, if_mtu,
    if_admin_status, if_oper_status,
    if_in_errors, if_out_errors, if_in_discards, if_out_discards
FROM device_snmp_interfaces
WHERE exporter = ? AND polled_at = ?
ORDER BY ifindex`
	rows, err := conn.Query(ctx, qi, expIP, inv.PolledAt)
	if err != nil {
		return nil, fmt.Errorf("store: query inventory interfaces: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i SNMPInterface
		if err := rows.Scan(
			&i.IfIndex, &i.IfDescr, &i.IfAlias, &i.IfType, &i.IfSpeedBps, &i.IfMtu,
			&i.AdminStatus, &i.OperStatus,
			&i.InErrors, &i.OutErrors, &i.InDiscards, &i.OutDiscards,
		); err != nil {
			return nil, fmt.Errorf("store: scan inventory interface: %w", err)
		}
		inv.Interfaces = append(inv.Interfaces, i)
	}
	return &inv, rows.Err()
}

// DeviceResource is one row of the /api/devices/{exporter}/resources
// response — a SNMP-derived health metric (CPU / memory / storage)
// for one component on the exporter, with the most recent reading
// and a trailing-window sparkline of percent values. The UI renders
// LatestPercent / LatestBytes / MaxBytes in the tile and Points in
// a small inline chart.
type DeviceResource struct {
	Kind           string                `json:"kind"`
	Component      string                `json:"component"`
	Source         string                `json:"source"`
	LatestTs       time.Time             `json:"latest_ts"`
	LatestPercent  float32               `json:"latest_percent"`
	LatestBytes    uint64                `json:"latest_bytes"`
	MaxBytes       uint64                `json:"max_bytes"`
	LatestNumeric  float64               `json:"latest_numeric"`
	Unit           string                `json:"unit"`
	Points         []DeviceResourcePoint `json:"points"`
}

// DeviceResourcePoint is one (ts, percent, numeric) data point on
// the per-component sparkline. Utilization kinds (cpu / memory /
// storage) drive the chart from ValuePercent; sensor kinds
// (temperature / fan / voltage / current / power) drive it from
// ValueNumeric and the corresponding `unit`. The UI picks based on
// the row's Unit field.
type DeviceResourcePoint struct {
	Ts           time.Time `json:"ts"`
	ValuePercent float32   `json:"value_percent"`
	ValueNumeric float64   `json:"value_numeric"`
}

// QueryDeviceResources returns the per-component health timeseries
// for an exporter over the trailing window. One row per
// (kind, component, source); points are ordered by ts ASC so the UI
// can feed them straight into a sparkline. The query argMax-picks the
// latest reading server-side so the response carries the freshness
// signal the tiles need.
//
// Empty result (no rows) is fine — devices that haven't been walked
// or that don't implement the relevant MIBs simply contribute nothing.
func QueryDeviceResources(ctx context.Context, conn driver.Conn, exporter netip.Addr, tr TimeRange) ([]DeviceResource, error) {
	tsPred, args := tr.Predicate("polled_at")
	args = append([]any{toIPv6(exporter)}, args...)
	// Parallel arrays (ts_points + pct_points) keep the Go scan
	// trivial — no nested-tuple driver dance. The inner subquery's
	// ORDER BY is preserved by groupArray, so the two arrays land
	// aligned and pre-sorted for the UI.
	q := `
SELECT
    kind,
    component,
    argMax(source,        polled_at) AS source,
    max(polled_at)                    AS latest_ts,
    argMax(value_percent, polled_at) AS latest_percent,
    argMax(value_bytes,   polled_at) AS latest_bytes,
    argMax(max_bytes,     polled_at) AS max_bytes,
    argMax(value_numeric, polled_at) AS latest_numeric,
    argMax(unit,          polled_at) AS unit,
    groupArray(polled_at)             AS ts_points,
    groupArray(value_percent)         AS pct_points,
    groupArray(value_numeric)         AS num_points
FROM (
    SELECT *
    FROM device_resource_samples
    WHERE exporter = ? AND ` + tsPred + `
    ORDER BY polled_at
) AS ordered
GROUP BY kind, component
ORDER BY kind, component`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query device resources: %w", err)
	}
	defer rows.Close()
	out := make([]DeviceResource, 0, 6)
	for rows.Next() {
		var (
			r         DeviceResource
			tsPoints  []time.Time
			pctPoints []float32
			numPoints []float64
		)
		if err := rows.Scan(
			&r.Kind, &r.Component, &r.Source, &r.LatestTs,
			&r.LatestPercent, &r.LatestBytes, &r.MaxBytes,
			&r.LatestNumeric, &r.Unit,
			&tsPoints, &pctPoints, &numPoints,
		); err != nil {
			return nil, fmt.Errorf("store: scan device resource: %w", err)
		}
		n := len(tsPoints)
		if len(pctPoints) < n {
			n = len(pctPoints)
		}
		if len(numPoints) < n {
			n = len(numPoints)
		}
		r.Points = make([]DeviceResourcePoint, 0, n)
		for i := 0; i < n; i++ {
			r.Points = append(r.Points, DeviceResourcePoint{
				Ts:           tsPoints[i],
				ValuePercent: pctPoints[i],
				ValueNumeric: numPoints[i],
			})
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Alert is the current state of one alert as derived from the
// append-only alert_events ledger via argMax aggregation. Fields
// match the JSON the api returns to the React Alerts tab.
type Alert struct {
	ID           string            `json:"id"` // hash of (rule_id, scope, group_key)
	RuleID       string            `json:"rule_id"`
	Severity     string            `json:"severity"`
	State        string            `json:"state"`
	Scope        string            `json:"scope"`         // raw, stable identifier (IP, "src→dst", etc.)
	ScopeDisplay string            `json:"scope_display"` // human-friendly enrichment of Scope; falls back to Scope
	GroupKey     string            `json:"group_key"`
	Title        string            `json:"title"`
	Body         string            `json:"body"`
	Runbook      string            `json:"runbook"`
	Actor        string            `json:"actor"`
	OpenedAt     time.Time         `json:"opened_at"`
	LastActiveAt time.Time         `json:"last_active_at"`
	Labels       map[string]string `json:"labels"`
}

// AlertSummary is the four-bucket count over the open + recent
// closed sets, used by the Alerts tab summary stats.
type AlertSummary struct {
	OpenCritical   uint64 `json:"open_critical"`
	OpenWarning    uint64 `json:"open_warning"`
	OpenInfo       uint64 `json:"open_info"`
	Acknowledged   uint64 `json:"acknowledged"`
	ClosedLast24h  uint64 `json:"closed_last_24h"`
}

// QueryAlerts returns the current alert set, optionally filtered by
// state ('open' returns opened+heartbeat collapsed; 'acknowledged'
// returns ack'd; 'closed' returns last 24h of closed). Empty state
// returns everything in the open + acknowledged buckets.
func QueryAlerts(ctx context.Context, conn driver.Conn, state string) ([]Alert, error) {
	q := `
WITH latest AS (
    SELECT
        rule_id,
        scope,
        group_key,
        argMax(state, ts)    AS state,
        argMax(severity, ts) AS severity,
        argMax(title, ts)    AS title,
        argMax(body, ts)     AS body,
        argMax(runbook, ts)  AS runbook,
        argMax(actor, ts)    AS actor,
        argMax(labels, ts)   AS labels,
        min(ts)              AS opened_at,
        max(ts)              AS last_active_at
    FROM alert_events
    WHERE ts >= now() - INTERVAL 7 DAY
    GROUP BY rule_id, scope, group_key
)
SELECT
    cityHash64(concat(rule_id, '|', scope, '|', group_key)) AS id,
    rule_id, severity, state, scope, group_key,
    title, body, runbook, actor, opened_at, last_active_at, labels
FROM latest
WHERE ` + alertStatePredicate(state) + `
ORDER BY opened_at DESC`
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query alerts: %w", err)
	}
	defer rows.Close()
	out := make([]Alert, 0, 32)
	for rows.Next() {
		var (
			a   Alert
			id  uint64
		)
		if err := rows.Scan(
			&id, &a.RuleID, &a.Severity, &a.State, &a.Scope, &a.GroupKey,
			&a.Title, &a.Body, &a.Runbook, &a.Actor,
			&a.OpenedAt, &a.LastActiveAt, &a.Labels,
		); err != nil {
			return nil, fmt.Errorf("store: scan alert: %w", err)
		}
		// Render the id as 16-char hex for stable URLs and JSON.
		a.ID = fmt.Sprintf("%016x", id)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enrich scope_display when an alert's labels carry an exporter
	// IP and SNMP has resolved a sys_name for it.
	exporters := make(map[string]struct{}, len(out))
	for _, a := range out {
		if ip := a.Labels["exporter"]; ip != "" {
			exporters[ip] = struct{}{}
		}
	}
	if len(exporters) > 0 {
		names := make(map[string]string, len(exporters))
		for ip := range exporters {
			addr, err := netip.ParseAddr(ip)
			if err != nil {
				continue
			}
			const qn = `
SELECT argMax(sys_name, polled_at)
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 7 DAY AND exporter = ?
GROUP BY exporter`
			var n string
			if err := conn.QueryRow(ctx, qn, toIPv6(addr)).Scan(&n); err == nil && n != "" {
				names[ip] = n
			}
		}
		for i := range out {
			if ip := out[i].Labels["exporter"]; ip != "" {
				if n := names[ip]; n != "" {
					// Replace the IP in the scope text with "name · ip"
					// when the scope contained the IP literally; else
					// just append the name as a hint.
					if out[i].Scope == ip {
						out[i].ScopeDisplay = n + " · " + ip
					} else {
						out[i].ScopeDisplay = out[i].Scope + " · " + n
					}
				}
			}
		}
	}
	return out, nil
}

// alertStatePredicate maps the api query parameter to a SQL WHERE.
// Treats 'opened' and 'heartbeat' as the same operator-facing state.
func alertStatePredicate(state string) string {
	switch state {
	case "open":
		return "state IN ('opened', 'heartbeat')"
	case "acknowledged":
		return "state = 'acknowledged'"
	case "closed":
		return "state = 'closed' AND last_active_at >= now() - INTERVAL 24 HOUR"
	default:
		return "state IN ('opened', 'heartbeat', 'acknowledged')"
	}
}

// QueryAlertSummary returns the bucket counts used by the Alerts tab
// summary stats. Single round-trip; uses the same `latest` CTE shape
// as QueryAlerts.
func QueryAlertSummary(ctx context.Context, conn driver.Conn) (*AlertSummary, error) {
	const q = `
WITH latest AS (
    SELECT
        rule_id, scope, group_key,
        argMax(state, ts)    AS state,
        argMax(severity, ts) AS severity,
        max(ts)              AS last_active_at
    FROM alert_events
    WHERE ts >= now() - INTERVAL 7 DAY
    GROUP BY rule_id, scope, group_key
)
SELECT
    countIf(state IN ('opened','heartbeat') AND severity = 'critical') AS open_critical,
    countIf(state IN ('opened','heartbeat') AND severity = 'warning')  AS open_warning,
    countIf(state IN ('opened','heartbeat') AND severity = 'info')     AS open_info,
    countIf(state = 'acknowledged') AS acked,
    countIf(state = 'closed' AND last_active_at >= now() - INTERVAL 24 HOUR) AS closed_24h
FROM latest`
	row := conn.QueryRow(ctx, q)
	var s AlertSummary
	if err := row.Scan(
		&s.OpenCritical, &s.OpenWarning, &s.OpenInfo, &s.Acknowledged, &s.ClosedLast24h,
	); err != nil {
		return nil, fmt.Errorf("store: query alert summary: %w", err)
	}
	return &s, nil
}

// AlertEvent is one row from alert_events — the audit trail behind a
// single (rule, scope, group_key). The detail modal renders a list
// of these as a timeline of "samples that triggered" plus the
// state-transition events (opened, acknowledged, closed).
type AlertEvent struct {
	Ts       time.Time         `json:"ts"`
	State    string            `json:"state"`
	Severity string            `json:"severity"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Actor    string            `json:"actor"`
	Labels   map[string]string `json:"labels"`
}

// AlertDetail bundles everything the alert detail modal needs in one
// API response: the current alert summary (same shape as QueryAlerts),
// the full event timeline for that (rule, scope, group_key), and a
// short list of linked flows derived from the labels recorded at
// open / heartbeat time.
type AlertDetail struct {
	Alert    Alert        `json:"alert"`
	Timeline []AlertEvent `json:"timeline"`
	Flows    []RecentFlow `json:"flows"`
	// FlowsSource explains how the linked-flow list was derived so the
	// UI can show a one-line provenance hint. Possible values:
	//   "labels"     — labels carried enough specificity to filter
	//   "exporter"   — only the exporter IP was usable
	//   "none"       — labels did not include any field we could filter on
	FlowsSource string `json:"flows_source"`
}

// QueryAlertDetail returns the full detail bundle for one alert by id.
// id is the cityHash64-hex returned by QueryAlerts. ErrNotFound is
// returned when no event has ever been written for that hash.
func QueryAlertDetail(ctx context.Context, conn driver.Conn, id string) (*AlertDetail, error) {
	// Resolve the (rule_id, scope, group_key) tuple from the hash.
	// Use the same WHERE the ack/close path uses so the lookups stay
	// consistent across endpoints.
	const lookup = `
SELECT rule_id, scope, group_key
FROM alert_events
WHERE cityHash64(concat(rule_id, '|', scope, '|', group_key)) = reinterpretAsUInt64(reverse(unhex(?)))
GROUP BY rule_id, scope, group_key
LIMIT 1`
	var ruleID, scope, groupKey string
	if err := conn.QueryRow(ctx, lookup, id).Scan(&ruleID, &scope, &groupKey); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: lookup alert %s: %w", id, err)
	}

	// Build the alert summary (same argMax shape as QueryAlerts) for
	// just this one (rule, scope, group_key). Bound to 7d like the
	// list endpoint so closed-but-still-in-ledger alerts still resolve.
	const summary = `
SELECT
    argMax(state, ts)    AS state,
    argMax(severity, ts) AS severity,
    argMax(title, ts)    AS title,
    argMax(body, ts)     AS body,
    argMax(runbook, ts)  AS runbook,
    argMax(actor, ts)    AS actor,
    argMax(labels, ts)   AS labels,
    min(ts)              AS opened_at,
    max(ts)              AS last_active_at
FROM alert_events
WHERE ts >= now() - INTERVAL 7 DAY
  AND rule_id   = ?
  AND scope     = ?
  AND group_key = ?`
	a := Alert{ID: id, RuleID: ruleID, Scope: scope, GroupKey: groupKey}
	if err := conn.QueryRow(ctx, summary, ruleID, scope, groupKey).Scan(
		&a.State, &a.Severity, &a.Title, &a.Body, &a.Runbook, &a.Actor,
		&a.Labels, &a.OpenedAt, &a.LastActiveAt,
	); err != nil {
		return nil, fmt.Errorf("store: alert detail summary: %w", err)
	}

	// Best-effort SNMP enrichment for scope_display, mirroring the
	// behavior in QueryAlerts so the modal header reads the same as
	// the row that opened it.
	if ip := a.Labels["exporter"]; ip != "" {
		if addr, err := netip.ParseAddr(ip); err == nil {
			const qn = `
SELECT argMax(sys_name, polled_at)
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 7 DAY AND exporter = ?
GROUP BY exporter`
			var n string
			if err := conn.QueryRow(ctx, qn, toIPv6(addr)).Scan(&n); err == nil && n != "" {
				if a.Scope == ip {
					a.ScopeDisplay = n + " · " + ip
				} else {
					a.ScopeDisplay = a.Scope + " · " + n
				}
			}
		}
	}

	// Timeline: every event row for this key in the last 7 days,
	// oldest first so the UI can render top-to-bottom chronologically.
	// Capped at 200 events so a flapping alert with thousands of
	// heartbeats doesn't blow up the response payload — the cap is
	// communicated to the UI by the row count alone (no explicit
	// "truncated" flag yet; this is a follow-up if we see it in the wild).
	const tl = `
SELECT ts, state, severity, title, body, actor, labels
FROM alert_events
WHERE ts >= now() - INTERVAL 7 DAY
  AND rule_id   = ?
  AND scope     = ?
  AND group_key = ?
ORDER BY ts ASC
LIMIT 200`
	timeline := make([]AlertEvent, 0, 32)
	rows, err := conn.Query(ctx, tl, ruleID, scope, groupKey)
	if err != nil {
		return nil, fmt.Errorf("store: alert detail timeline: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ev AlertEvent
		if err := rows.Scan(&ev.Ts, &ev.State, &ev.Severity, &ev.Title, &ev.Body, &ev.Actor, &ev.Labels); err != nil {
			return nil, fmt.Errorf("store: scan alert event: %w", err)
		}
		timeline = append(timeline, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Linked flows: derive a FlowFilter from the labels recorded by
	// the rule. Today's two built-ins write:
	//   exporter_silent → labels{exporter}
	//   heavy_talker    → labels{src_addr, dst_addr, bytes}
	// The window is the alert's lifetime, clamped to [60s, 30m].
	flows, source, err := queryAlertLinkedFlows(ctx, conn, &a)
	if err != nil {
		// Don't fail the whole response just because the flow lookup
		// hiccupped — the timeline is still useful on its own.
		flows = nil
		source = "error: " + err.Error()
	}

	return &AlertDetail{
		Alert:       a,
		Timeline:    timeline,
		Flows:       flows,
		FlowsSource: source,
	}, nil
}

// queryAlertLinkedFlows turns alert labels into a flow filter and
// returns up to 50 recent flows that match. Returns ([], "none", nil)
// when no label is specific enough to filter on.
func queryAlertLinkedFlows(ctx context.Context, conn driver.Conn, a *Alert) ([]RecentFlow, string, error) {
	f := FlowFilter{
		Exporter: a.Labels["exporter"],
		SrcAddr:  a.Labels["src_addr"],
		DstAddr:  a.Labels["dst_addr"],
	}
	source := ""
	switch {
	case f.SrcAddr != "" || f.DstAddr != "":
		source = "labels"
	case f.Exporter != "":
		source = "exporter"
	default:
		return nil, "none", nil
	}

	// Window = alert lifetime, but clamp so a 7-day-old alert doesn't
	// scan a week of flows just to populate a modal.
	from := a.OpenedAt
	to := a.LastActiveAt
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if to.Before(from) {
		from, to = to, from
	}
	span := to.Sub(from)
	const minSpan = 60 * time.Second
	const maxSpan = 30 * time.Minute
	if span < minSpan {
		// Pad symmetrically around openedAt so a brand-new alert still
		// shows the flows immediately surrounding the open event.
		half := (minSpan - span) / 2
		from = from.Add(-half)
		to = to.Add(half)
	}
	if to.Sub(from) > maxSpan {
		from = to.Add(-maxSpan)
	}
	tr := AbsoluteRange(from, to)

	// Use the existing list query so SNMP enrichment, IP unmap, and
	// the WHERE-builder edge cases are all reused. Sort newest first.
	flows, err := QueryFlowsList(ctx, conn, tr, 50, 0, FlowsListSortObserved, FlowsListDirDesc, f)
	if err != nil {
		return nil, "", err
	}
	return flows, source, nil
}

// AckAlert appends an acknowledged event for an alert. The alert id
// is the cityHash64-hex returned by QueryAlerts; we look up the
// (rule_id, scope, group_key) tuple via the same hash and write an
// 'acknowledged' row carrying the operator's name.
func AckAlert(ctx context.Context, conn driver.Conn, id string, actor string) error {
	return appendStateTransition(ctx, conn, id, "acknowledged", actor, "alert acknowledged by operator")
}

// CloseAlert appends a closed event. Operators may close manually
// even before the engine auto-closes (e.g. silenced or false positive).
func CloseAlert(ctx context.Context, conn driver.Conn, id string, actor string) error {
	return appendStateTransition(ctx, conn, id, "closed", actor, "manually closed by operator")
}

func appendStateTransition(ctx context.Context, conn driver.Conn, id, state, actor, body string) error {
	const lookup = `
SELECT rule_id, scope, group_key, argMax(severity, ts), argMax(title, ts), argMax(runbook, ts), argMax(labels, ts)
FROM alert_events
WHERE cityHash64(concat(rule_id, '|', scope, '|', group_key)) = reinterpretAsUInt64(reverse(unhex(?)))
GROUP BY rule_id, scope, group_key
LIMIT 1`
	row := conn.QueryRow(ctx, lookup, id)
	var (
		ruleID, scope, groupKey, severity, title, runbook string
		labels                                            map[string]string
	)
	if err := row.Scan(&ruleID, &scope, &groupKey, &severity, &title, &runbook, &labels); err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: lookup alert %s: %w", id, err)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	const ins = `INSERT INTO alert_events
		(ts, rule_id, severity, state, scope, group_key, title, body, runbook, actor, labels)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := conn.Exec(ctx, ins,
		time.Now().UTC(), ruleID, severity, state, scope, groupKey, title, body, runbook, actor, labels,
	); err != nil {
		return fmt.Errorf("store: append %s: %w", state, err)
	}
	return nil
}

// FlowFilter narrows top-N queries by exporter, 5-tuple, or protocol.
// Empty / zero fields mean "no filter on this dimension". Parameters
// are bound through the driver — never interpolated into SQL — so
// untrusted user input cannot escape into the WHERE clause.
type FlowFilter struct {
	Exporter      string // IP string ("10.2.0.11" or "2001:db8::1"); validated as netip.Addr
	SrcAddr       string
	DstAddr       string
	SrcPort       uint16 // 0 = unset
	DstPort       uint16
	Proto         uint16 // 16-bit so 0 can mean "unset"; valid values fit in 8 bits
	InputIfIndex  uint32 // 0 = unset; observation interface on the exporter
	OutputIfIndex uint32 // 0 = unset
	SrcAS         uint32 // 0 = unset; per the convention used by the flows table default
	DstAS         uint32
}

// buildWhere returns SQL fragments and bound args for the WHERE clause
// produced by a FlowFilter. The first fragment is always the time-range
// predicate; the rest are filter terms appended only for fields
// the operator actually set.
func buildWhere(tr TimeRange, f FlowFilter) (string, []any, error) {
	pred, args := tr.Predicate("observed")
	where := []string{pred}

	addAddr := func(name, raw string) error {
		if raw == "" {
			return nil
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return fmt.Errorf("filter.%s: %w", name, err)
		}
		where = append(where, name+" = ?")
		args = append(args, toIPv6(addr))
		return nil
	}
	if err := addAddr("exporter", f.Exporter); err != nil {
		return "", nil, err
	}
	if err := addAddr("src_addr", f.SrcAddr); err != nil {
		return "", nil, err
	}
	if err := addAddr("dst_addr", f.DstAddr); err != nil {
		return "", nil, err
	}
	if f.SrcPort != 0 {
		where = append(where, "src_port = ?")
		args = append(args, f.SrcPort)
	}
	if f.DstPort != 0 {
		where = append(where, "dst_port = ?")
		args = append(args, f.DstPort)
	}
	if f.Proto != 0 {
		where = append(where, "proto = ?")
		args = append(args, uint8(f.Proto))
	}
	if f.InputIfIndex != 0 {
		where = append(where, "input_ifindex = ?")
		args = append(args, f.InputIfIndex)
	}
	if f.OutputIfIndex != 0 {
		where = append(where, "output_ifindex = ?")
		args = append(args, f.OutputIfIndex)
	}
	if f.SrcAS != 0 {
		where = append(where, "src_as = ?")
		args = append(args, f.SrcAS)
	}
	if f.DstAS != 0 {
		where = append(where, "dst_as = ?")
		args = append(args, f.DstAS)
	}
	return strings.Join(where, " AND "), args, nil
}

// TopTalker is a (src, dst) pair aggregated over a window. Returned by
// /api/top/talkers.
type TopTalker struct {
	SrcAddr string `json:"src_addr"`
	DstAddr string `json:"dst_addr"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Flows   uint64 `json:"flows"`
}

// TopService is a (dst_port, proto) aggregate. Returned by /api/top/services.
type TopService struct {
	DstPort uint16 `json:"dst_port"`
	Proto   uint8  `json:"proto"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Flows   uint64 `json:"flows"`
}

// TopNSort is the sort dimension for top-N panels: bytes (default),
// packets, or flows. Whitelisted server-side so the column can be
// inlined into the SQL ORDER BY safely.
type TopNSort string

const (
	TopNSortBytes   TopNSort = "bytes"
	TopNSortPackets TopNSort = "packets"
	TopNSortFlows   TopNSort = "flows"
)

// ParseTopNSort accepts the raw query-string value and returns a
// whitelisted TopNSort. Empty or unknown values fall back to bytes.
func ParseTopNSort(s string) TopNSort {
	switch s {
	case string(TopNSortPackets):
		return TopNSortPackets
	case string(TopNSortFlows):
		return TopNSortFlows
	default:
		return TopNSortBytes
	}
}

// orderColumn turns a whitelisted sort enum into the SQL aggregate
// expression. Always returns a safe column name — never user input.
func (s TopNSort) orderColumn() string {
	switch s {
	case TopNSortPackets:
		return "packets"
	case TopNSortFlows:
		return "flows"
	default:
		return "bytes"
	}
}

// TopProtocol is one row per IP protocol number with share-of-total.
// Returned by /api/top/protocols.
type TopProtocol struct {
	Proto   uint8  `json:"proto"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Flows   uint64 `json:"flows"`
}

// TopASN is a (src_as, dst_as) pair aggregated over a window.
// Returned by /api/top/asn. Either side may be 0 — common when
// the exporter doesn't have a BGP table covering the address (the
// IP is in a default route or the exporter wasn't configured
// with NetFlow ASN export).
type TopASN struct {
	SrcAS   uint32 `json:"src_as"`
	DstAS   uint32 `json:"dst_as"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Flows   uint64 `json:"flows"`
}

// QueryTopASN returns the N largest (src_as, dst_as) aggregates
// over the trailing window, narrowed by the FlowFilter. The sort
// dimension (bytes / packets / flows) is whitelisted by the caller.
func QueryTopASN(ctx context.Context, conn driver.Conn, tr TimeRange, limit int, sort TopNSort, f FlowFilter) ([]TopASN, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT src_as, dst_as,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY src_as, dst_as
ORDER BY ` + sort.orderColumn() + ` DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top asn: %w", err)
	}
	defer rows.Close()
	out := make([]TopASN, 0, limit)
	for rows.Next() {
		var a TopASN
		if err := rows.Scan(&a.SrcAS, &a.DstAS, &a.Bytes, &a.Packets, &a.Flows); err != nil {
			return nil, fmt.Errorf("store: scan top asn: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TopConversation is a full 5-tuple aggregate. Returned by
// /api/top/conversations.
type TopConversation struct {
	SrcAddr  string    `json:"src_addr"`
	DstAddr  string    `json:"dst_addr"`
	SrcPort  uint16    `json:"src_port"`
	DstPort  uint16    `json:"dst_port"`
	Proto    uint8     `json:"proto"`
	Bytes    uint64    `json:"bytes"`
	Packets  uint64    `json:"packets"`
	Flows    uint64    `json:"flows"`
	LastSeen time.Time `json:"last_seen"`
}

// QueryTopTalkers returns the N largest src→dst aggregates over the
// trailing window, narrowed by the supplied FlowFilter. The sort
// dimension (bytes / packets / flows) is whitelisted by the caller.
func QueryTopTalkers(ctx context.Context, conn driver.Conn, tr TimeRange, limit int, sort TopNSort, f FlowFilter) ([]TopTalker, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT src_addr, dst_addr,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY src_addr, dst_addr
ORDER BY ` + sort.orderColumn() + ` DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top talkers: %w", err)
	}
	defer rows.Close()
	out := make([]TopTalker, 0, limit)
	for rows.Next() {
		var (
			t   TopTalker
			src netip.Addr
			dst netip.Addr
		)
		if err := rows.Scan(&src, &dst, &t.Bytes, &t.Packets, &t.Flows); err != nil {
			return nil, fmt.Errorf("store: scan top talker: %w", err)
		}
		t.SrcAddr = src.Unmap().String()
		t.DstAddr = dst.Unmap().String()
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryTopServices returns the N largest (dst_port, proto) aggregates
// over the trailing window, narrowed by the FlowFilter. The sort
// dimension (bytes / packets / flows) is whitelisted by the caller.
func QueryTopServices(ctx context.Context, conn driver.Conn, tr TimeRange, limit int, sort TopNSort, f FlowFilter) ([]TopService, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT dst_port, proto,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY dst_port, proto
ORDER BY ` + sort.orderColumn() + ` DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top services: %w", err)
	}
	defer rows.Close()
	out := make([]TopService, 0, limit)
	for rows.Next() {
		var s TopService
		if err := rows.Scan(&s.DstPort, &s.Proto, &s.Bytes, &s.Packets, &s.Flows); err != nil {
			return nil, fmt.Errorf("store: scan top service: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryTopProtocols returns one row per IP protocol number, ordered by
// the chosen sort dimension desc, narrowed by the FlowFilter.
func QueryTopProtocols(ctx context.Context, conn driver.Conn, tr TimeRange, sort TopNSort, f FlowFilter) ([]TopProtocol, error) {
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT proto,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY proto
ORDER BY ` + sort.orderColumn() + ` DESC`
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top protocols: %w", err)
	}
	defer rows.Close()
	out := make([]TopProtocol, 0, 16)
	for rows.Next() {
		var p TopProtocol
		if err := rows.Scan(&p.Proto, &p.Bytes, &p.Packets, &p.Flows); err != nil {
			return nil, fmt.Errorf("store: scan top protocol: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueryTopConversations returns the N largest 5-tuple aggregates over
// the trailing window, narrowed by the FlowFilter. The sort dimension
// (bytes / packets / flows) is whitelisted by the caller.
func QueryTopConversations(ctx context.Context, conn driver.Conn, tr TimeRange, limit int, sort TopNSort, f FlowFilter) ([]TopConversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT src_addr, dst_addr, src_port, dst_port, proto,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows,
       max(observed) AS last_seen
FROM flows
WHERE ` + whereSQL + `
GROUP BY src_addr, dst_addr, src_port, dst_port, proto
ORDER BY ` + sort.orderColumn() + ` DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top conversations: %w", err)
	}
	defer rows.Close()
	out := make([]TopConversation, 0, limit)
	for rows.Next() {
		var (
			c   TopConversation
			src netip.Addr
			dst netip.Addr
		)
		if err := rows.Scan(&src, &dst, &c.SrcPort, &c.DstPort, &c.Proto, &c.Bytes, &c.Packets, &c.Flows, &c.LastSeen); err != nil {
			return nil, fmt.Errorf("store: scan top conversation: %w", err)
		}
		c.SrcAddr = src.Unmap().String()
		c.DstAddr = dst.Unmap().String()
		out = append(out, c)
	}
	return out, rows.Err()
}

// TopInterface is a per-interface aggregate over the trailing window.
// Each flow contributes to both its input and output interface, so
// the response carries separate in_/out_ totals plus combined totals
// used for sorting. SNMP enrichment (sys_name, if_descr, if_alias) is
// joined in when available — empty strings when SNMP has not yet
// walked the device. Returned by /api/top/interfaces.
type TopInterface struct {
	Exporter   string `json:"exporter"`
	SysName    string `json:"sys_name"`
	IfIndex    uint32 `json:"ifindex"`
	IfDescr    string `json:"if_descr"`
	IfAlias    string `json:"if_alias"`
	InBytes    uint64 `json:"in_bytes"`
	OutBytes   uint64 `json:"out_bytes"`
	InPackets  uint64 `json:"in_packets"`
	OutPackets uint64 `json:"out_packets"`
	InFlows    uint64 `json:"in_flows"`
	OutFlows   uint64 `json:"out_flows"`
	Bytes      uint64 `json:"bytes"`
	Packets    uint64 `json:"packets"`
	Flows      uint64 `json:"flows"`
}

// QueryTopInterfaces returns the N busiest interfaces seen in the
// trailing window, narrowed by the FlowFilter. Each flow record is
// fanned out via ARRAY JOIN into one row for its input_ifindex and
// one for its output_ifindex (direction=1/2), so the table is scanned
// once. ifindex=0 rows (unset on this side of the flow) are dropped.
// SNMP inventory + ifTable are LEFT JOINed so the UI can show
// sys_name / if_descr / if_alias when available.
func QueryTopInterfaces(ctx context.Context, conn driver.Conn, tr TimeRange, limit int, sort TopNSort, f FlowFilter) ([]TopInterface, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
WITH
per_iface AS (
    SELECT
        exporter,
        iface_tuple.1 AS ifindex,
        iface_tuple.2 AS direction,
        bytes,
        packets
    FROM flows
    ARRAY JOIN [tuple(input_ifindex, toUInt8(1)), tuple(output_ifindex, toUInt8(2))] AS iface_tuple
    WHERE ` + whereSQL + ` AND iface_tuple.1 != 0
),
agg AS (
    SELECT
        exporter,
        ifindex,
        sumIf(bytes,   direction = 1) AS in_bytes,
        sumIf(bytes,   direction = 2) AS out_bytes,
        sumIf(packets, direction = 1) AS in_packets,
        sumIf(packets, direction = 2) AS out_packets,
        countIf(direction = 1)        AS in_flows,
        countIf(direction = 2)        AS out_flows
    FROM per_iface
    GROUP BY exporter, ifindex
),
totals AS (
    SELECT
        exporter,
        ifindex,
        in_bytes,
        out_bytes,
        in_packets,
        out_packets,
        in_flows,
        out_flows,
        in_bytes + out_bytes     AS bytes,
        in_packets + out_packets AS packets,
        in_flows + out_flows     AS flows
    FROM agg
),
inv AS (` + sqlLatestInventory + `),
sif AS (` + sqlLatestSNMPInterfaces + `)
SELECT
    a.exporter,
    ifNull(inv.sys_name, '') AS sys_name,
    a.ifindex,
    ifNull(sif.if_descr, '') AS if_descr,
    ifNull(sif.if_alias, '') AS if_alias,
    a.in_bytes, a.out_bytes,
    a.in_packets, a.out_packets,
    a.in_flows, a.out_flows,
    a.bytes, a.packets, a.flows
FROM totals AS a
LEFT JOIN inv ON a.exporter = inv.exporter
LEFT JOIN sif ON a.exporter = sif.exporter AND a.ifindex = sif.ifindex
ORDER BY ` + sort.orderColumn() + ` DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query top interfaces: %w", err)
	}
	defer rows.Close()
	out := make([]TopInterface, 0, limit)
	for rows.Next() {
		var (
			t        TopInterface
			exporter netip.Addr
		)
		if err := rows.Scan(
			&exporter, &t.SysName, &t.IfIndex, &t.IfDescr, &t.IfAlias,
			&t.InBytes, &t.OutBytes,
			&t.InPackets, &t.OutPackets,
			&t.InFlows, &t.OutFlows,
			&t.Bytes, &t.Packets, &t.Flows,
		); err != nil {
			return nil, fmt.Errorf("store: scan top interface: %w", err)
		}
		t.Exporter = exporter.Unmap().String()
		out = append(out, t)
	}
	return out, rows.Err()
}

// FlowsListSort is the sort dimension for the paginated flows-list
// endpoint that powers the Flows-tab Investigate panel. Whitelisted
// server-side so the column can be inlined into ORDER BY safely.
type FlowsListSort string

const (
	FlowsListSortObserved FlowsListSort = "observed"
	FlowsListSortBytes    FlowsListSort = "bytes"
	FlowsListSortPackets  FlowsListSort = "packets"
)

func ParseFlowsListSort(s string) FlowsListSort {
	switch s {
	case string(FlowsListSortBytes):
		return FlowsListSortBytes
	case string(FlowsListSortPackets):
		return FlowsListSortPackets
	default:
		return FlowsListSortObserved
	}
}

// FlowsListDir is the sort direction for QueryFlowsList. Whitelisted
// like FlowsListSort.
type FlowsListDir string

const (
	FlowsListDirAsc  FlowsListDir = "asc"
	FlowsListDirDesc FlowsListDir = "desc"
)

func ParseFlowsListDir(s string) FlowsListDir {
	if s == string(FlowsListDirAsc) {
		return FlowsListDirAsc
	}
	return FlowsListDirDesc
}

// QueryFlowsList returns a paginated, filterable, sortable view of
// rows from the flows table. Powers the Investigate panel on the
// Flows tab — distinct from the live tail (QueryRecentFlows) which
// is fixed-size, time-only, and tail-only.
//
// limit is clamped to [1, 500]; offset to [0, 100000]. The caller is
// responsible for tracking pagination state — this function returns
// raw rows without a total count (the cost of computing a total over
// large windows is high on ClickHouse and the caller can infer
// "more pages exist" from len(rows) == limit).
func QueryFlowsList(
	ctx context.Context,
	conn driver.Conn,
	tr TimeRange,
	limit, offset int,
	sort FlowsListSort,
	dir FlowsListDir,
	f FlowFilter,
) ([]RecentFlow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	if offset > 100_000 {
		offset = 100_000
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
WITH inv AS (` + sqlLatestInventory + `)
SELECT
    f.observed, f.exporter, ifNull(inv.sys_name, '') AS exporter_name,
    f.src_addr, f.dst_addr,
    f.src_port, f.dst_port, f.proto, f.bytes, f.packets,
    f.input_ifindex, f.output_ifindex, f.src_as, f.dst_as, f.tcp_flags, f.source
FROM flows AS f
LEFT JOIN inv ON f.exporter = inv.exporter
WHERE ` + whereSQL + `
ORDER BY f.` + string(sort) + ` ` + string(dir) + `
LIMIT ? OFFSET ?`
	args = append(args, uint64(limit), uint64(offset))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query flows list: %w", err)
	}
	defer rows.Close()
	out := make([]RecentFlow, 0, limit)
	for rows.Next() {
		var (
			rf       RecentFlow
			exporter netip.Addr
			src      netip.Addr
			dst      netip.Addr
		)
		if err := rows.Scan(
			&rf.Observed, &exporter, &rf.ExporterName, &src, &dst,
			&rf.SrcPort, &rf.DstPort, &rf.Proto,
			&rf.Bytes, &rf.Packets,
			&rf.InputIfIndex, &rf.OutputIfIndex,
			&rf.SrcAS, &rf.DstAS,
			&rf.TCPFlags,
			&rf.Source,
		); err != nil {
			return nil, fmt.Errorf("store: scan flows list row: %w", err)
		}
		rf.Exporter = exporter.Unmap().String()
		rf.SrcAddr = src.Unmap().String()
		rf.DstAddr = dst.Unmap().String()
		out = append(out, rf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FlowTimeseriesPoint is one bucket on the per-filter flow chart
// rendered in the drill-in drawer. Bytes and packets are the
// summed totals for the bucket window.
type FlowTimeseriesPoint struct {
	Ts      time.Time `json:"ts"`
	Bytes   uint64    `json:"bytes"`
	Packets uint64    `json:"packets"`
	Flows   uint64    `json:"flows"`
}

// QueryFlowsTimeseries returns bucketed bytes/packets/flow-count
// per bucketSeconds over the time range, narrowed by the filter.
// Buckets are aligned via toStartOfInterval so the same window over
// a stable input always returns the same bucket boundaries.
//
// bucketSeconds is clamped to [1, 3600] — narrower than 1s rounds
// down to bursts that aren't statistically interesting; wider than
// an hour produces too few points to read as a chart.
func QueryFlowsTimeseries(
	ctx context.Context,
	conn driver.Conn,
	tr TimeRange,
	bucketSeconds int,
	f FlowFilter,
) ([]FlowTimeseriesPoint, error) {
	if bucketSeconds < 1 {
		bucketSeconds = 1
	}
	if bucketSeconds > 3600 {
		bucketSeconds = 3600
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT toStartOfInterval(observed, INTERVAL ? SECOND) AS ts,
       sum(bytes)   AS bytes,
       sum(packets) AS packets,
       count()      AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY ts
ORDER BY ts ASC`
	args = append([]any{uint64(bucketSeconds)}, args...)
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query flows timeseries: %w", err)
	}
	defer rows.Close()
	out := make([]FlowTimeseriesPoint, 0, 64)
	for rows.Next() {
		var p FlowTimeseriesPoint
		if err := rows.Scan(&p.Ts, &p.Bytes, &p.Packets, &p.Flows); err != nil {
			return nil, fmt.Errorf("store: scan flows timeseries: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FlagsBucket is one bucket on the per-conversation TCP-flag
// timeline rendered in the drawer's Connection state tab. Each
// counter is the number of flow records in this bucket whose
// ORed tcp_flags has the corresponding bit set.
//
// SYN+ACK is counted as a separate dimension (both bits set in
// the same record) because it's the marker for a successful
// handshake — distinct from a bare SYN (initiation, possibly
// unanswered) or a bare ACK (data flow / keepalive). All counts
// are over flow records, not packets — sFlow is sampled, NetFlow
// aggregates over the flow's lifetime.
type FlagsBucket struct {
	Ts      time.Time `json:"ts"`
	SYN     uint64    `json:"syn"`     // any record with SYN set
	SYNACK  uint64    `json:"syn_ack"` // SYN and ACK both set in the same record
	FIN     uint64    `json:"fin"`
	RST     uint64    `json:"rst"`
	ACKOnly uint64    `json:"ack_only"` // ACK set, SYN+FIN+RST clear — data flow
	PSH     uint64    `json:"psh"`
	URG     uint64    `json:"urg"`
	Total   uint64    `json:"total"` // total flow records in this bucket (all protos)
}

// QueryFlagsTimeseries returns bucketed TCP-flag counts over the
// time range, narrowed by the filter. Buckets align via
// toStartOfInterval so the same window over a stable input always
// returns the same boundaries.
//
// We deliberately don't filter to proto=6 server-side — non-TCP
// records have tcp_flags=0 by construction, so they contribute
// 0 to every flag count. The Total field still reflects all
// records so the UI can compute "X% of records carried flag Y"
// even on mixed-protocol filters.
func QueryFlagsTimeseries(
	ctx context.Context,
	conn driver.Conn,
	tr TimeRange,
	bucketSeconds int,
	f FlowFilter,
) ([]FlagsBucket, error) {
	if bucketSeconds < 1 {
		bucketSeconds = 1
	}
	if bucketSeconds > 3600 {
		bucketSeconds = 3600
	}
	whereSQL, args, err := buildWhere(tr, f)
	if err != nil {
		return nil, err
	}
	// TCP flag bits per RFC 793: FIN=0x01 SYN=0x02 RST=0x04
	// PSH=0x08 ACK=0x10 URG=0x20.
	q := `
SELECT
    toStartOfInterval(observed, INTERVAL ? SECOND) AS ts,
    countIf(bitAnd(tcp_flags, 2)  != 0)                                AS syn,
    countIf(bitAnd(tcp_flags, 18) = 18)                                AS syn_ack,
    countIf(bitAnd(tcp_flags, 1)  != 0)                                AS fin,
    countIf(bitAnd(tcp_flags, 4)  != 0)                                AS rst,
    countIf(bitAnd(tcp_flags, 16) != 0 AND bitAnd(tcp_flags, 7)  = 0)  AS ack_only,
    countIf(bitAnd(tcp_flags, 8)  != 0)                                AS psh,
    countIf(bitAnd(tcp_flags, 32) != 0)                                AS urg,
    count()                                                            AS total
FROM flows
WHERE ` + whereSQL + `
GROUP BY ts
ORDER BY ts ASC`
	args = append([]any{uint64(bucketSeconds)}, args...)
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query flags timeseries: %w", err)
	}
	defer rows.Close()
	out := make([]FlagsBucket, 0, 64)
	for rows.Next() {
		var b FlagsBucket
		if err := rows.Scan(
			&b.Ts, &b.SYN, &b.SYNACK, &b.FIN, &b.RST, &b.ACKOnly, &b.PSH, &b.URG, &b.Total,
		); err != nil {
			return nil, fmt.Errorf("store: scan flags timeseries: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QueryRecentFlows returns the most recent N rows from the flows table,
// newest first. If exporter is non-empty, results are filtered to that
// single exporter. Limit is clamped to [1, 1000].
func QueryRecentFlows(ctx context.Context, conn driver.Conn, limit int, exporter string) ([]RecentFlow, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	exporterPredicate := ""
	args := []any{}
	if exporter != "" {
		addr, err := netip.ParseAddr(exporter)
		if err != nil {
			return nil, fmt.Errorf("store: invalid exporter address: %w", err)
		}
		exporterPredicate = " WHERE f.exporter = ?"
		args = append(args, toIPv6(addr))
	}
	q := `
WITH inv AS (` + sqlLatestInventory + `)
SELECT
    f.observed, f.exporter, ifNull(inv.sys_name, '') AS exporter_name,
    f.src_addr, f.dst_addr,
    f.src_port, f.dst_port, f.proto, f.bytes, f.packets,
    f.input_ifindex, f.output_ifindex, f.src_as, f.dst_as, f.tcp_flags, f.source
FROM flows AS f
LEFT JOIN inv ON f.exporter = inv.exporter` + exporterPredicate + `
ORDER BY f.observed DESC
LIMIT ?`
	args = append(args, uint64(limit))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query recent flows: %w", err)
	}
	defer rows.Close()
	out := make([]RecentFlow, 0, limit)
	for rows.Next() {
		var (
			rf       RecentFlow
			exporter netip.Addr
			src      netip.Addr
			dst      netip.Addr
		)
		if err := rows.Scan(
			&rf.Observed, &exporter, &rf.ExporterName, &src, &dst,
			&rf.SrcPort, &rf.DstPort, &rf.Proto,
			&rf.Bytes, &rf.Packets,
			&rf.InputIfIndex, &rf.OutputIfIndex,
			&rf.SrcAS, &rf.DstAS,
			&rf.TCPFlags,
			&rf.Source,
		); err != nil {
			return nil, fmt.Errorf("store: scan recent flow: %w", err)
		}
		rf.Exporter = exporter.Unmap().String()
		rf.SrcAddr = src.Unmap().String()
		rf.DstAddr = dst.Unmap().String()
		out = append(out, rf)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
