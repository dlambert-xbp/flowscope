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
