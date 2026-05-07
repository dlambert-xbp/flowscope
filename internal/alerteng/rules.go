package alerteng

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// DefaultRules returns the v0 built-in rule set. More rules
// (interface utilization, BGP transitions, top-talker delta vs
// baseline) are follow-up slices once SNMP enrichment and gNMI BGP
// state are in place.
func DefaultRules() []Rule {
	return []Rule{
		ExporterSilent{
			SilentSeconds: 60,
			ActiveSeconds: 600,
		},
		HeavyTalker{
			WindowSeconds: 300,
			BytesThreshold: 1 << 30, // 1 GiB
		},
	}
}

/* ----------------------------- ExporterSilent ----------------------------- */

// ExporterSilent fires when an exporter that produced flows in the
// last ActiveSeconds is currently silent for SilentSeconds. Auto-
// clears when the exporter resumes.
type ExporterSilent struct {
	SilentSeconds int
	ActiveSeconds int
}

func (ExporterSilent) ID() string       { return "exporter_silent" }
func (ExporterSilent) Severity() string { return SeverityCritical }
func (ExporterSilent) Runbook() string  { return "exporter-silent.md" }

func (r ExporterSilent) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	const q = `
SELECT IPv6NumToString(exporter)
FROM (
    SELECT exporter
    FROM flows
    WHERE observed >= now() - INTERVAL ? SECOND
    GROUP BY exporter
) AS active
WHERE exporter NOT IN (
    SELECT exporter FROM flows
    WHERE observed >= now() - INTERVAL ? SECOND
    GROUP BY exporter
)`
	rows, err := conn.Query(ctx, q, uint64(r.ActiveSeconds), uint64(r.SilentSeconds))
	if err != nil {
		return nil, fmt.Errorf("exporter_silent: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		// IPv6NumToString returns "::ffff:10.2.0.11" for v4-mapped form;
		// trim the prefix so the chip / scope reads as "10.2.0.11".
		ip := unmap4in6(raw)
		out = append(out, Violation{
			Scope:    ip,
			GroupKey: "silent_" + ip,
			Title:    fmt.Sprintf("Exporter %s has been silent for >%ds", ip, r.SilentSeconds),
			Body: fmt.Sprintf(
				"This exporter produced flows in the last %ds but no flows in the last %ds. "+
					"Likely device/path failure or upstream networking issue.",
				r.ActiveSeconds, r.SilentSeconds,
			),
			Severity: SeverityCritical,
			Labels:   map[string]string{"exporter": ip},
		})
	}
	return out, rows.Err()
}

/* ----------------------------- HeavyTalker ----------------------------- */

// HeavyTalker fires when one (src_addr, dst_addr) pair has moved more
// than BytesThreshold bytes in the trailing window. A simple but
// useful "what's the elephant" alarm. Real-world tuning replaces the
// fixed threshold with a baseline comparison once we have history.
type HeavyTalker struct {
	WindowSeconds  int
	BytesThreshold uint64
}

func (HeavyTalker) ID() string       { return "heavy_talker" }
func (HeavyTalker) Severity() string { return SeverityWarning }
func (HeavyTalker) Runbook() string  { return "heavy-talker.md" }

func (r HeavyTalker) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	const q = `
SELECT IPv6NumToString(src_addr) AS src,
       IPv6NumToString(dst_addr) AS dst,
       sum(bytes) AS total
FROM flows
WHERE observed >= now() - INTERVAL ? SECOND
GROUP BY src_addr, dst_addr
HAVING total >= ?
ORDER BY total DESC
LIMIT 20`
	rows, err := conn.Query(ctx, q, uint64(r.WindowSeconds), r.BytesThreshold)
	if err != nil {
		return nil, fmt.Errorf("heavy_talker: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawSrc, rawDst string
			total          uint64
		)
		if err := rows.Scan(&rawSrc, &rawDst, &total); err != nil {
			return nil, err
		}
		src := unmap4in6(rawSrc)
		dst := unmap4in6(rawDst)
		scope := src + " → " + dst
		out = append(out, Violation{
			Scope:    scope,
			GroupKey: "talker_" + src + "_" + dst,
			Title:    fmt.Sprintf("Heavy talker · %s moved %s in last %ds", scope, fmtBytes(total), r.WindowSeconds),
			Body: fmt.Sprintf(
				"This src→dst pair has moved %s in the trailing %d-second window. "+
					"Investigate before treating as malicious — could be a backup, replication, or video stream.",
				fmtBytes(total), r.WindowSeconds,
			),
			Severity: SeverityWarning,
			Labels: map[string]string{
				"src_addr": src,
				"dst_addr": dst,
				"bytes":    fmt.Sprintf("%d", total),
			},
		})
	}
	return out, rows.Err()
}

/* ----------------------------- helpers ----------------------------- */

// unmap4in6 strips ClickHouse's "::ffff:" prefix that IPv6NumToString
// emits for IPv4-mapped IPv6 values, leaving the v4 dotted form
// alone. Pure IPv6 addresses pass through unchanged.
func unmap4in6(s string) string {
	const pfx = "::ffff:"
	if len(s) > len(pfx) && s[:len(pfx)] == pfx {
		return s[len(pfx):]
	}
	return s
}

// fmtBytes formats a byte count with SI-binary units. Local copy to
// avoid pulling in the api layer here.
func fmtBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
