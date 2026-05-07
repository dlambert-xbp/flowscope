package store

import (
	"context"
	"fmt"
	"net/netip"
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
// Window defaults to 5 minutes when zero or negative.
func QueryInterfaces(ctx context.Context, conn driver.Conn, window time.Duration) ([]InterfaceRow, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	const q = `
WITH diffed AS (
    SELECT
        ts,
        exporter,
        ifindex,
        toFloat64(in_octets - lagInFrame(in_octets) OVER w) AS d_in,
        toFloat64(out_octets - lagInFrame(out_octets) OVER w) AS d_out,
        date_diff('millisecond', lagInFrame(ts) OVER w, ts) AS dt_ms
    FROM iface_counter_samples
    WHERE ts >= now() - INTERVAL ? SECOND
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
	rows, err := conn.Query(ctx, q, uint64(window.Seconds()))
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
	expBytes := exporter.As16()
	rows, err := conn.Query(ctx, q, expBytes[:], ifindex, uint64(window.Seconds()))
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

// QueryRecentFlows returns the most recent N rows from the flows table,
// newest first. Limit is clamped to [1, 1000].
func QueryRecentFlows(ctx context.Context, conn driver.Conn, limit int) ([]RecentFlow, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `
SELECT
    observed, exporter, src_addr, dst_addr,
    src_port, dst_port, proto, bytes, packets,
    input_ifindex, output_ifindex, source
FROM flows
ORDER BY observed DESC
LIMIT ?`
	rows, err := conn.Query(ctx, q, uint64(limit))
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
