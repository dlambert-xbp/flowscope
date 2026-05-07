package store

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

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
	IfIndex       uint32    `json:"ifindex"`
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
	IfIndex       uint32                  `json:"ifindex"`
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
// the canonical record.Flow but with JSON-friendly types.
type RecentFlow struct {
	Observed       time.Time `json:"observed"`
	Exporter       string    `json:"exporter"`
	SrcAddr        string    `json:"src_addr"`
	DstAddr        string    `json:"dst_addr"`
	SrcPort        uint16    `json:"src_port"`
	DstPort        uint16    `json:"dst_port"`
	Proto          uint8     `json:"proto"`
	Bytes          uint64    `json:"bytes"`
	Packets        uint64    `json:"packets"`
	InputIfIndex   uint32    `json:"input_ifindex"`
	OutputIfIndex  uint32    `json:"output_ifindex"`
	Source         string    `json:"source"`
}

// QuerySummary returns aggregate stats over the trailing window.
// A zero or negative window defaults to 5 minutes.
func QuerySummary(ctx context.Context, conn driver.Conn, window time.Duration) (Summary, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	const q = `
SELECT
    count() AS flows,
    sum(bytes) AS bytes,
    sum(packets) AS packets,
    uniq(exporter) AS exporters,
    max(observed) AS newest,
    min(observed) AS oldest
FROM flows
WHERE observed >= now() - INTERVAL ? SECOND`
	row := conn.QueryRow(ctx, q, uint64(window.Seconds()))
	var s Summary
	s.Window = window
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
// exporter. Window defaults to 5 minutes when zero or negative.
func QueryInterfaces(ctx context.Context, conn driver.Conn, window time.Duration, exporter string) ([]InterfaceRow, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	exporterPredicate := ""
	args := []any{uint64(window.Seconds())}
	if exporter != "" {
		addr, err := netip.ParseAddr(exporter)
		if err != nil {
			return nil, fmt.Errorf("store: invalid exporter address: %w", err)
		}
		exporterPredicate = " AND exporter = ?"
		args = append(args, toIPv6(addr))
	}
	q := `
WITH diffed AS (
    SELECT
        ts,
        exporter,
        ifindex,
        toFloat64(in_octets - lagInFrame(in_octets) OVER w) AS d_in,
        toFloat64(out_octets - lagInFrame(out_octets) OVER w) AS d_out,
        date_diff('millisecond', lagInFrame(ts) OVER w, ts) AS dt_ms
    FROM iface_counter_samples
    WHERE ts >= now() - INTERVAL ? SECOND` + exporterPredicate + `
    WINDOW w AS (PARTITION BY exporter, ifindex ORDER BY ts)
)
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
ORDER BY (in_peak + out_peak) DESC
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
			&exporter, &r.IfIndex, &r.LastSeen,
			&r.InBpsLatest, &r.OutBpsLatest,
			&r.InBpsPeak, &r.OutBpsPeak,
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
func QueryInterfaceTimeseries(ctx context.Context, conn driver.Conn, exporter netip.Addr, ifindex uint32, window time.Duration) (*InterfaceTimeseries, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	const q = `
WITH diffed AS (
    SELECT
        ts,
        toFloat64(in_octets - lagInFrame(in_octets) OVER w) AS d_in,
        toFloat64(out_octets - lagInFrame(out_octets) OVER w) AS d_out,
        date_diff('millisecond', lagInFrame(ts) OVER w, ts) AS dt_ms
    FROM iface_counter_samples
    WHERE exporter = ? AND ifindex = ?
      AND ts >= now() - INTERVAL ? SECOND
    WINDOW w AS (ORDER BY ts)
)
SELECT
    ts,
    toUInt64(if(d_in  >= 0 AND dt_ms > 0, d_in  * 8000 / dt_ms, 0)) AS in_bps,
    toUInt64(if(d_out >= 0 AND dt_ms > 0, d_out * 8000 / dt_ms, 0)) AS out_bps
FROM diffed
WHERE dt_ms > 0
ORDER BY ts`
	rows, err := conn.Query(ctx, q, toIPv6(exporter), ifindex, uint64(window.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("store: query interface timeseries: %w", err)
	}
	defer rows.Close()
	out := &InterfaceTimeseries{
		Exporter:      exporter.Unmap().String(),
		IfIndex:       ifindex,
		WindowSeconds: int(window.Seconds()),
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
	return out, rows.Err()
}

// Device is one exporter's traffic summary over a window. Returned by
// /api/devices. The platform infers exporters from observed flows;
// SNMP-driven inventory enrichment (model, OS, uptime, location)
// arrives in a later slice.
type Device struct {
	Exporter   string    `json:"exporter"`
	Flows      uint64    `json:"flows"`
	Bytes      uint64    `json:"bytes"`
	Packets    uint64    `json:"packets"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	IfaceCount uint64    `json:"iface_count"`
}

// QueryDevices lists every exporter that produced flow records in the
// trailing window, ranked by total bytes. iface_count is the number
// of unique ifindex values that produced counter samples in the same
// window — populated only for sFlow / gNMI-capable exporters.
func QueryDevices(ctx context.Context, conn driver.Conn, window time.Duration) ([]Device, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	const q = `
SELECT
    f.exporter   AS exporter,
    f.flows      AS flows,
    f.bytes      AS bytes,
    f.packets    AS packets,
    f.first_seen AS first_seen,
    f.last_seen  AS last_seen,
    ifNull(i.iface_count, 0) AS iface_count
FROM (
    SELECT
        exporter,
        count() AS flows,
        sum(bytes)   AS bytes,
        sum(packets) AS packets,
        min(observed) AS first_seen,
        max(observed) AS last_seen
    FROM flows
    WHERE observed >= now() - INTERVAL ? SECOND
    GROUP BY exporter
) AS f
LEFT JOIN (
    SELECT exporter, uniq(ifindex) AS iface_count
    FROM iface_counter_samples
    WHERE ts >= now() - INTERVAL ? SECOND
    GROUP BY exporter
) AS i ON f.exporter = i.exporter
ORDER BY f.bytes DESC`
	w := uint64(window.Seconds())
	rows, err := conn.Query(ctx, q, w, w)
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
		if err := rows.Scan(&exporter, &d.Flows, &d.Bytes, &d.Packets, &d.FirstSeen, &d.LastSeen, &d.IfaceCount); err != nil {
			return nil, fmt.Errorf("store: scan device: %w", err)
		}
		d.Exporter = exporter.Unmap().String()
		out = append(out, d)
	}
	return out, rows.Err()
}

// QueryDevice returns the same shape as one row of QueryDevices,
// scoped to the supplied exporter address. Empty result (no flows in
// window) returns ErrNotFound — the api maps this to 404.
func QueryDevice(ctx context.Context, conn driver.Conn, exporter netip.Addr, window time.Duration) (*Device, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	const q = `
SELECT
    count() AS flows,
    sum(bytes)   AS bytes,
    sum(packets) AS packets,
    min(observed) AS first_seen,
    max(observed) AS last_seen
FROM flows
WHERE observed >= now() - INTERVAL ? SECOND AND exporter = ?
GROUP BY exporter`
	expIP := toIPv6(exporter)
	row := conn.QueryRow(ctx, q, uint64(window.Seconds()), expIP)
	var d Device
	if err := row.Scan(&d.Flows, &d.Bytes, &d.Packets, &d.FirstSeen, &d.LastSeen); err != nil {
		// clickhouse-go returns sql.ErrNoRows wrapped on empty groups
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: query device: %w", err)
	}
	d.Exporter = exporter.Unmap().String()

	// Interface count for this exporter.
	const qi = `
SELECT uniq(ifindex)
FROM iface_counter_samples
WHERE ts >= now() - INTERVAL ? SECOND AND exporter = ?`
	if err := conn.QueryRow(ctx, qi, uint64(window.Seconds()), expIP).Scan(&d.IfaceCount); err != nil {
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
    poll_duration_ms, poll_status
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

// Alert is the current state of one alert as derived from the
// append-only alert_events ledger via argMax aggregation. Fields
// match the JSON the api returns to the React Alerts tab.
type Alert struct {
	ID           string            `json:"id"` // hash of (rule_id, scope, group_key)
	RuleID       string            `json:"rule_id"`
	Severity     string            `json:"severity"`
	State        string            `json:"state"`
	Scope        string            `json:"scope"`
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
	return out, rows.Err()
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
	Exporter string // IP string ("10.2.0.11" or "2001:db8::1"); validated as netip.Addr
	SrcAddr  string
	DstAddr  string
	SrcPort  uint16 // 0 = unset
	DstPort  uint16
	Proto    uint16 // 16-bit so 0 can mean "unset"; valid values fit in 8 bits
}

// buildWhere returns SQL fragments and bound args for the WHERE clause
// produced by a FlowFilter. The first fragment is always the trailing
// window predicate; the rest are filter terms appended only for fields
// the operator actually set.
func buildWhere(window time.Duration, f FlowFilter) (string, []any, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	where := []string{"observed >= now() - INTERVAL ? SECOND"}
	args := []any{uint64(window.Seconds())}

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
	Flows   uint64 `json:"flows"`
}

// TopProtocol is one row per IP protocol number with share-of-total.
// Returned by /api/top/protocols.
type TopProtocol struct {
	Proto   uint8  `json:"proto"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Flows   uint64 `json:"flows"`
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

// QueryTopTalkers returns the N largest src→dst byte aggregates over
// the trailing window, narrowed by the supplied FlowFilter.
func QueryTopTalkers(ctx context.Context, conn driver.Conn, window time.Duration, limit int, f FlowFilter) ([]TopTalker, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(window, f)
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
ORDER BY bytes DESC
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

// QueryTopServices returns the N largest (dst_port, proto) byte
// aggregates over the trailing window, narrowed by the FlowFilter.
func QueryTopServices(ctx context.Context, conn driver.Conn, window time.Duration, limit int, f FlowFilter) ([]TopService, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(window, f)
	if err != nil {
		return nil, err
	}
	q := `
SELECT dst_port, proto,
       sum(bytes) AS bytes,
       count()    AS flows
FROM flows
WHERE ` + whereSQL + `
GROUP BY dst_port, proto
ORDER BY bytes DESC
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
		if err := rows.Scan(&s.DstPort, &s.Proto, &s.Bytes, &s.Flows); err != nil {
			return nil, fmt.Errorf("store: scan top service: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryTopProtocols returns one row per IP protocol number, ordered by
// total bytes desc, narrowed by the FlowFilter.
func QueryTopProtocols(ctx context.Context, conn driver.Conn, window time.Duration, f FlowFilter) ([]TopProtocol, error) {
	whereSQL, args, err := buildWhere(window, f)
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
ORDER BY bytes DESC`
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
// the trailing window, narrowed by the FlowFilter.
func QueryTopConversations(ctx context.Context, conn driver.Conn, window time.Duration, limit int, f FlowFilter) ([]TopConversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	whereSQL, args, err := buildWhere(window, f)
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
ORDER BY bytes DESC
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
		exporterPredicate = " WHERE exporter = ?"
		args = append(args, toIPv6(addr))
	}
	q := `
SELECT
    observed, exporter, src_addr, dst_addr,
    src_port, dst_port, proto, bytes, packets,
    input_ifindex, output_ifindex, source
FROM flows` + exporterPredicate + `
ORDER BY observed DESC
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
			&rf.Observed, &exporter, &src, &dst,
			&rf.SrcPort, &rf.DstPort, &rf.Proto,
			&rf.Bytes, &rf.Packets,
			&rf.InputIfIndex, &rf.OutputIfIndex,
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
