package alerteng

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// DefaultRules returns the v0 built-in rule set. The four extra rules
// added in the P1 push (oper-status change, utilization, errors rate,
// baseline anomaly) live in rules_extra.go. The four device-health
// rules (CPU / memory / storage / unreachable) live in rules_device.go;
// they read the snmp service's tables and require no new ingest path.
// BGP transitions and gNMI-driven unreachability remain deferred until
// bgpPeerTable walks and gNMI ingest land.
func DefaultRules() []Rule {
	return []Rule{
		ExporterSilent{
			SilentSeconds: 60,
			ActiveSeconds: 600,
		},
		HeavyTalker{
			WindowSeconds:  300,
			BytesThreshold: 1 << 30, // 1 GiB
		},
		InterfaceOperStatusChange{
			DebounceSeconds: 60,
			LookbackHours:   24,
		},
		InterfaceUtilizationHigh{
			ThresholdPct:    80,
			CriticalBumpPct: 15,
			WindowSeconds:   300,
		},
		InterfaceErrorsRate{
			WindowSeconds: 300,
			ErrorsPerMin:  10,
		},
		TopTalkerBaselineAnomaly{
			Multiplier:       3.0,
			MinBaselineBytes: 1_000_000_000, // 1 GB
		},
		DeviceCPUHigh{
			ThresholdPct:    80,
			CriticalBumpPct: 15,
			LookbackSeconds: 1800,
		},
		DeviceMemoryHigh{
			ThresholdPct:    85,
			CriticalBumpPct: 10,
			LookbackSeconds: 1800,
		},
		DeviceStorageHigh{
			ThresholdPct:    85,
			CriticalBumpPct: 10,
			LookbackSeconds: 3600,
		},
		DeviceUnreachable{
			StaleSeconds:  2700,
			LookbackHours: 24,
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

func (ExporterSilent) ID() string              { return "exporter_silent" }
func (ExporterSilent) Severity() string        { return SeverityCritical }
func (ExporterSilent) DefaultSeverity() string { return SeverityCritical }
func (ExporterSilent) Runbook() string         { return "exporter-silent.md" }
func (r ExporterSilent) DefaultParams() map[string]any {
	return map[string]any{"silent_seconds": r.SilentSeconds, "active_seconds": r.ActiveSeconds}
}

func (r ExporterSilent) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

// EvaluateScoped honors scope.Exporters by intersecting the active
// exporter set with it before checking silence — useful when an
// instance only cares about a subset of devices (e.g. "alert if any
// of these critical edge routers stop reporting").
func (r ExporterSilent) EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error) {
	silent := paramInt(params, "silent_seconds", r.SilentSeconds)
	if silent <= 0 {
		silent = 60
	}
	active := paramInt(params, "active_seconds", r.ActiveSeconds)
	if active <= 0 {
		active = 600
	}
	scopeFrag, scopeArgs := scopeWhere("exporter", "", scope)
	q := `
SELECT IPv6NumToString(exporter)
FROM (
    SELECT exporter
    FROM flows
    WHERE observed >= now() - INTERVAL ? SECOND` + scopeFrag + `
    GROUP BY exporter
) AS active
WHERE exporter NOT IN (
    SELECT exporter FROM flows
    WHERE observed >= now() - INTERVAL ? SECOND` + scopeFrag + `
    GROUP BY exporter
)`
	args := []any{uint64(active)}
	args = append(args, scopeArgs...)
	args = append(args, uint64(silent))
	args = append(args, scopeArgs...)
	rows, err := conn.Query(ctx, q, args...)
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
		ip := unmap4in6(raw)
		out = append(out, Violation{
			Scope:    ip,
			GroupKey: "silent_" + ip,
			Title:    fmt.Sprintf("Exporter %s has been silent for >%ds", ip, silent),
			Body: fmt.Sprintf(
				"This exporter produced flows in the last %ds but no flows in the last %ds. "+
					"Likely device/path failure or upstream networking issue.",
				active, silent,
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

func (HeavyTalker) ID() string              { return "heavy_talker" }
func (HeavyTalker) Severity() string        { return SeverityWarning }
func (HeavyTalker) DefaultSeverity() string { return SeverityWarning }
func (HeavyTalker) Runbook() string         { return "heavy-talker.md" }
func (r HeavyTalker) DefaultParams() map[string]any {
	return map[string]any{"window_seconds": r.WindowSeconds, "bytes_threshold": r.BytesThreshold}
}

func (r HeavyTalker) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

// EvaluateScoped ignores scope — heavy-talker is flow-pair level and
// the scope dimensions (exporter, ifindex) don't apply. The api's
// ScopeKindsFor returns nil for this template so a non-empty scope
// fails ValidateScope before reaching here.
func (r HeavyTalker) EvaluateScoped(ctx context.Context, conn driver.Conn, _ ScopeSelector, params map[string]any) ([]Violation, error) {
	window := paramInt(params, "window_seconds", r.WindowSeconds)
	if window <= 0 {
		window = 300
	}
	threshold := paramUint64(params, "bytes_threshold", r.BytesThreshold)
	if threshold == 0 {
		threshold = 1 << 30
	}
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
	rows, err := conn.Query(ctx, q, uint64(window), threshold)
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
			Title:    fmt.Sprintf("Heavy talker · %s moved %s in last %ds", scope, fmtBytes(total), window),
			Body: fmt.Sprintf(
				"This src→dst pair has moved %s in the trailing %d-second window. "+
					"Investigate before treating as malicious — could be a backup, replication, or video stream.",
				fmtBytes(total), window,
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
