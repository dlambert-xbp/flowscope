package alerteng

// rules_extra.go — built-in alert rules added in the P1 push.
//
// These four rules complement the original ExporterSilent / HeavyTalker
// pair. Two depend on SNMP-derived inventory (oper-status transitions,
// utilization), one on counter-sample diffs (errors / discards rate),
// and one on flow-history baselines (top-talker anomaly).
//
// Each rule is a pure function over ClickHouse: Evaluate runs queries
// and returns the set of currently-violating tuples. The engine handles
// dedup, heartbeat, and auto-close; rules never persist state.
//
// Two rules from the original P1 list — bgp_session_state_change and
// device_unreachable — are intentionally absent. They depend on data
// sources (bgpPeerTable walks, gNMI ingest) that don't exist yet; see
// the PR description.

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

/* ----------------------- InterfaceOperStatusChange ----------------------- */

// InterfaceOperStatusChange fires when a polled interface transitions
// between if_oper_status values (typically up<->down). Source:
// device_snmp_interfaces — we look at the latest two snapshots per
// (exporter, ifindex) and emit a violation when they disagree.
//
// DebounceSeconds is the lookback window: only transitions where the
// most recent polled_at is within DebounceSeconds of now() count as
// "currently violating". This makes flap-debouncing fall out of the
// engine's existing stability-window auto-close — once the new state
// has been the steady-state long enough that the prior snapshot ages
// out of the window, the violation clears.
type InterfaceOperStatusChange struct {
	DebounceSeconds int
	LookbackHours   int // history window for picking "previous" status; default 24
}

func (InterfaceOperStatusChange) ID() string              { return "interface_oper_status_change" }
func (InterfaceOperStatusChange) Severity() string        { return SeverityWarning }
func (InterfaceOperStatusChange) DefaultSeverity() string { return SeverityWarning }
func (InterfaceOperStatusChange) Runbook() string {
	return "Fires when an interface's SNMP if_oper_status changes between successive polls. " +
		"Investigate the device's interface for a hardware fault, an admin shutdown, or upstream " +
		"link failure. Auto-clears once the new state has held steady through the engine's stability window."
}
func (r InterfaceOperStatusChange) DefaultParams() map[string]any {
	return map[string]any{"debounce_seconds": r.DebounceSeconds, "lookback_hours": r.LookbackHours}
}

func (r InterfaceOperStatusChange) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

func (r InterfaceOperStatusChange) EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error) {
	debounce := paramInt(params, "debounce_seconds", r.DebounceSeconds)
	if debounce <= 0 {
		debounce = 60
	}
	lookbackHours := paramInt(params, "lookback_hours", r.LookbackHours)
	if lookbackHours <= 0 {
		lookbackHours = 24
	}
	// For each (exporter, ifindex) pull the latest polled_at and the
	// status at the latest two snapshots in the lookback window. We
	// emit a violation when those two statuses differ AND the latest
	// snapshot is within the debounce window — the engine's stability
	// window then auto-closes once the new state is steady.
	//
	// arraySort + arraySlice lets us pull the top-2 polled_at values
	// reliably regardless of merge ordering. The status lookup uses
	// argMax(... , polled_at) for the most recent and a filtered
	// argMax with the prior polled_at for the second-most-recent.
	scopeFrag, scopeArgs := scopeWhere("exporter", "ifindex", scope)
	q := `
SELECT
    IPv6NumToString(exporter) AS exporter_ip,
    ifindex,
    argMax(if_descr, polled_at) AS if_descr,
    argMax(if_oper_status, polled_at) AS curr_status,
    argMaxIf(if_oper_status, polled_at, polled_at < latest) AS prev_status,
    latest
FROM (
    SELECT
        exporter,
        ifindex,
        if_descr,
        if_oper_status,
        polled_at,
        max(polled_at) OVER (PARTITION BY exporter, ifindex) AS latest
    FROM device_snmp_interfaces
    WHERE polled_at >= now() - INTERVAL ? HOUR` + scopeFrag + `
)
GROUP BY exporter, ifindex, latest
HAVING curr_status != ''
   AND prev_status != ''
   AND curr_status != prev_status
   AND latest >= now() - INTERVAL ? SECOND`
	args := []any{uint64(lookbackHours)}
	args = append(args, scopeArgs...)
	args = append(args, uint64(debounce))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interface_oper_status_change: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawExporter, ifDescr, curr, prev string
			ifindex                          uint32
			lastPolled                       any // DateTime64; we don't render it directly
		)
		if err := rows.Scan(&rawExporter, &ifindex, &ifDescr, &curr, &prev, &lastPolled); err != nil {
			return nil, err
		}
		exporter := unmap4in6(rawExporter)
		v := buildOperStatusViolation(exporter, ifindex, ifDescr, prev, curr)
		out = append(out, v)
	}
	return out, rows.Err()
}

// buildOperStatusViolation is split out so unit tests can verify the
// formatting without touching ClickHouse.
func buildOperStatusViolation(exporter string, ifindex uint32, ifDescr, prev, curr string) Violation {
	label := ifDescr
	if label == "" {
		label = fmt.Sprintf("ifindex %d", ifindex)
	}
	scope := fmt.Sprintf("%s · %s", exporter, label)
	severity := SeverityWarning
	if curr == "down" || curr == "lowerLayerDown" {
		severity = SeverityCritical
	}
	return Violation{
		Scope:    scope,
		GroupKey: fmt.Sprintf("operstatus_%s_%d", exporter, ifindex),
		Title:    fmt.Sprintf("Interface %s on %s changed: %s → %s", label, exporter, prev, curr),
		Body: fmt.Sprintf(
			"SNMP if_oper_status for %s (ifindex %d) on %s transitioned from %q to %q. "+
				"This usually indicates a link event — admin shutdown, cable pull, or upstream failure.",
			label, ifindex, exporter, prev, curr,
		),
		Severity: severity,
		Labels: map[string]string{
			"exporter":    exporter,
			"ifindex":     fmt.Sprintf("%d", ifindex),
			"if_descr":    ifDescr,
			"prev_status": prev,
			"curr_status": curr,
		},
	}
}

/* ----------------------- InterfaceUtilizationHigh ----------------------- */

// InterfaceUtilizationHigh fires when the trailing bps utilization on
// an interface exceeds ThresholdPct of if_speed_bps. Bps comes from
// successive iface_counter_samples diffs; speed comes from the latest
// device_snmp_interfaces row.
//
// Severity is dynamic: warning at ThresholdPct, critical at
// ThresholdPct + CriticalBumpPct (default 15). Two thresholds in one
// rule keeps the operator UI simple — one knob controls both.
type InterfaceUtilizationHigh struct {
	ThresholdPct     int
	CriticalBumpPct  int
	WindowSeconds    int // bps measurement window, default 300
}

func (InterfaceUtilizationHigh) ID() string              { return "interface_utilization_high" }
func (InterfaceUtilizationHigh) Severity() string        { return SeverityWarning }
func (InterfaceUtilizationHigh) DefaultSeverity() string { return SeverityWarning }
func (InterfaceUtilizationHigh) Runbook() string {
	return "Fires when an interface's recent throughput exceeds the configured percentage of its " +
		"link speed. Check for a heavy talker, a backup window, or a saturated uplink. Verify " +
		"if_speed_bps in the SNMP snapshot is correct (mis-set ifSpeed on legacy devices makes the " +
		"percentage unreliable)."
}
func (r InterfaceUtilizationHigh) DefaultParams() map[string]any {
	return map[string]any{
		"threshold_pct":     r.ThresholdPct,
		"critical_bump_pct": r.CriticalBumpPct,
		"window_seconds":    r.WindowSeconds,
	}
}

func (r InterfaceUtilizationHigh) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

func (r InterfaceUtilizationHigh) EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error) {
	threshold := paramInt(params, "threshold_pct", r.ThresholdPct)
	if threshold <= 0 {
		threshold = 80
	}
	bump := paramInt(params, "critical_bump_pct", r.CriticalBumpPct)
	if bump <= 0 {
		bump = 15
	}
	window := paramInt(params, "window_seconds", r.WindowSeconds)
	if window <= 0 {
		window = 300
	}
	// We compute bps as (latest_octets - earliest_octets) * 8 / window
	// over the trailing window, then pct = bps * 100 / if_speed_bps.
	// The join uses the latest device_snmp_interfaces snapshot per
	// (exporter, ifindex). Counters can wrap on 32-bit interfaces but
	// most modern gear exports 64-bit ifHC* so we treat negatives as
	// "skip this interface" rather than complicate the SQL.
	scopeFrag, scopeArgs := scopeWhere("exporter", "ifindex", scope)
	q := `
SELECT
    IPv6NumToString(s.exporter) AS exporter_ip,
    s.ifindex,
    iface.if_descr,
    iface.if_speed_bps,
    s.bps,
    intDiv(s.bps * 100, iface.if_speed_bps) AS pct
FROM (
    SELECT
        exporter,
        ifindex,
        intDiv(toInt64(max(in_octets) - min(in_octets) + max(out_octets) - min(out_octets)) * 8,
               greatest(toInt64(?), 1)) AS bps
    FROM iface_counter_samples
    WHERE ts >= now() - INTERVAL ? SECOND` + scopeFrag + `
    GROUP BY exporter, ifindex
    HAVING max(in_octets) >= min(in_octets) AND max(out_octets) >= min(out_octets)
) AS s
INNER JOIN (
    SELECT
        exporter,
        ifindex,
        argMax(if_descr, polled_at)     AS if_descr,
        argMax(if_speed_bps, polled_at) AS if_speed_bps
    FROM device_snmp_interfaces
    WHERE polled_at >= now() - INTERVAL 7 DAY` + scopeFrag + `
    GROUP BY exporter, ifindex
) AS iface USING (exporter, ifindex)
WHERE iface.if_speed_bps > 0
  AND s.bps > 0
HAVING pct >= ?`
	args := []any{uint64(window), uint64(window)}
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)
	args = append(args, uint64(threshold))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interface_utilization_high: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawExporter, ifDescr string
			ifindex              uint32
			ifSpeedBps           uint64
			bps                  int64
			pct                  int64
		)
		if err := rows.Scan(&rawExporter, &ifindex, &ifDescr, &ifSpeedBps, &bps, &pct); err != nil {
			return nil, err
		}
		exporter := unmap4in6(rawExporter)
		out = append(out, buildUtilizationViolation(
			exporter, ifindex, ifDescr, ifSpeedBps, uint64(bps),
			int(pct), threshold, threshold+bump,
		))
	}
	return out, rows.Err()
}

func buildUtilizationViolation(
	exporter string, ifindex uint32, ifDescr string,
	ifSpeedBps, bps uint64, pct, warnAt, critAt int,
) Violation {
	label := ifDescr
	if label == "" {
		label = fmt.Sprintf("ifindex %d", ifindex)
	}
	severity := SeverityWarning
	if pct >= critAt {
		severity = SeverityCritical
	}
	scope := fmt.Sprintf("%s · %s", exporter, label)
	return Violation{
		Scope:    scope,
		GroupKey: fmt.Sprintf("util_%s_%d", exporter, ifindex),
		Title: fmt.Sprintf(
			"Interface %s on %s at %d%% utilization (%s of %s)",
			label, exporter, pct, fmtBitsPerSec(bps), fmtBitsPerSec(ifSpeedBps),
		),
		Body: fmt.Sprintf(
			"Trailing utilization %d%% (%s) exceeds the threshold (%d%% warning / %d%% critical) "+
				"for a link rated at %s. Investigate top talkers or check for unexpected traffic.",
			pct, fmtBitsPerSec(bps), warnAt, critAt, fmtBitsPerSec(ifSpeedBps),
		),
		Severity: severity,
		Labels: map[string]string{
			"exporter":     exporter,
			"ifindex":      fmt.Sprintf("%d", ifindex),
			"if_descr":     ifDescr,
			"bps":          fmt.Sprintf("%d", bps),
			"if_speed_bps": fmt.Sprintf("%d", ifSpeedBps),
			"pct":          fmt.Sprintf("%d", pct),
		},
	}
}

/* ----------------------------- InterfaceErrorsRate ----------------------------- */

// InterfaceErrorsRate fires when the per-minute rate of
// in_errors+out_errors+in_discards+out_discards over the trailing
// WindowSeconds exceeds ErrorsPerMin. Counter-sample driven; a noisy
// interface generates a single sustained alert that auto-closes when
// the rate drops.
type InterfaceErrorsRate struct {
	WindowSeconds int
	ErrorsPerMin  int
}

func (InterfaceErrorsRate) ID() string              { return "interface_errors_rate" }
func (InterfaceErrorsRate) Severity() string        { return SeverityWarning }
func (InterfaceErrorsRate) DefaultSeverity() string { return SeverityWarning }
func (InterfaceErrorsRate) Runbook() string {
	return "Fires when an interface's combined error+discard rate exceeds the threshold. " +
		"Common causes: duplex mismatch, faulty optic, oversubscribed buffer, or a misconfigured " +
		"QoS policy dropping legitimate traffic. Cross-check if_in_errors / if_out_errors trend " +
		"in the Devices view."
}
func (r InterfaceErrorsRate) DefaultParams() map[string]any {
	return map[string]any{"window_seconds": r.WindowSeconds, "errors_per_min": r.ErrorsPerMin}
}

func (r InterfaceErrorsRate) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

func (r InterfaceErrorsRate) EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error) {
	window := paramInt(params, "window_seconds", r.WindowSeconds)
	if window <= 0 {
		window = 300
	}
	threshold := paramInt(params, "errors_per_min", r.ErrorsPerMin)
	if threshold <= 0 {
		threshold = 10
	}
	// Sum the deltas across all four counters, normalize to per-minute.
	scopeFrag, scopeArgs := scopeWhere("exporter", "ifindex", scope)
	q := `
SELECT
    IPv6NumToString(c.exporter) AS exporter_ip,
    c.ifindex,
    iface.if_descr,
    c.errs,
    intDiv(c.errs * 60, greatest(toInt64(?), 1)) AS per_min
FROM (
    SELECT
        exporter,
        ifindex,
        toInt64(
            (max(in_errors)   - min(in_errors))   +
            (max(out_errors)  - min(out_errors))  +
            (max(in_discards) - min(in_discards)) +
            (max(out_discards)- min(out_discards))
        ) AS errs
    FROM iface_counter_samples
    WHERE ts >= now() - INTERVAL ? SECOND` + scopeFrag + `
    GROUP BY exporter, ifindex
    HAVING max(in_errors)   >= min(in_errors)
       AND max(out_errors)  >= min(out_errors)
       AND max(in_discards) >= min(in_discards)
       AND max(out_discards)>= min(out_discards)
) AS c
LEFT JOIN (
    SELECT
        exporter,
        ifindex,
        argMax(if_descr, polled_at) AS if_descr
    FROM device_snmp_interfaces
    WHERE polled_at >= now() - INTERVAL 7 DAY` + scopeFrag + `
    GROUP BY exporter, ifindex
) AS iface USING (exporter, ifindex)
WHERE c.errs > 0
HAVING per_min >= ?`
	args := []any{uint64(window), uint64(window)}
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)
	args = append(args, uint64(threshold))
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("interface_errors_rate: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawExporter, ifDescr string
			ifindex              uint32
			errs, perMin         int64
		)
		if err := rows.Scan(&rawExporter, &ifindex, &ifDescr, &errs, &perMin); err != nil {
			return nil, err
		}
		exporter := unmap4in6(rawExporter)
		out = append(out, buildErrorsRateViolation(
			exporter, ifindex, ifDescr, uint64(errs), uint64(perMin), window, threshold,
		))
	}
	return out, rows.Err()
}

func buildErrorsRateViolation(
	exporter string, ifindex uint32, ifDescr string,
	totalErrs, perMin uint64, windowSeconds, thresholdPerMin int,
) Violation {
	label := ifDescr
	if label == "" {
		label = fmt.Sprintf("ifindex %d", ifindex)
	}
	scope := fmt.Sprintf("%s · %s", exporter, label)
	return Violation{
		Scope:    scope,
		GroupKey: fmt.Sprintf("errs_%s_%d", exporter, ifindex),
		Title: fmt.Sprintf(
			"Interface %s on %s: %d errors+discards/min (%d total in last %ds)",
			label, exporter, perMin, totalErrs, windowSeconds,
		),
		Body: fmt.Sprintf(
			"Combined errors+discards rate (%d/min) on %s ifindex %d exceeds the configured "+
				"threshold of %d/min over the last %d seconds. Likely physical-layer or "+
				"congestion issue.",
			perMin, exporter, ifindex, thresholdPerMin, windowSeconds,
		),
		Severity: SeverityWarning,
		Labels: map[string]string{
			"exporter": exporter,
			"ifindex":  fmt.Sprintf("%d", ifindex),
			"if_descr": ifDescr,
			"errors":   fmt.Sprintf("%d", totalErrs),
			"per_min":  fmt.Sprintf("%d", perMin),
		},
	}
}

/* --------------------------- TopTalkerBaselineAnomaly --------------------------- */

// TopTalkerBaselineAnomaly fires when a (src, dst) pair's bytes in the
// trailing hour exceed Multiplier × the trailing 7-day baseline for the
// same hour-of-day. MinBaselineBytes guards against the "everything
// looks anomalous because the baseline is near zero" failure mode on
// quiet exporters.
type TopTalkerBaselineAnomaly struct {
	Multiplier        float64
	MinBaselineBytes  uint64
}

func (TopTalkerBaselineAnomaly) ID() string              { return "top_talker_baseline_anomaly" }
func (TopTalkerBaselineAnomaly) Severity() string        { return SeverityWarning }
func (TopTalkerBaselineAnomaly) DefaultSeverity() string { return SeverityWarning }
func (TopTalkerBaselineAnomaly) Runbook() string {
	return "Fires when a (src,dst) pair's bytes in the last hour are more than the configured " +
		"multiplier of the same hour-of-day baseline averaged over the last 7 days. Investigate " +
		"the destination — could be a backup window shifting, replication topology change, or " +
		"data exfiltration. Tune min_baseline_bytes upward if quiet exporters dominate the alert " +
		"stream."
}
func (r TopTalkerBaselineAnomaly) DefaultParams() map[string]any {
	return map[string]any{"multiplier": r.Multiplier, "min_baseline_bytes": r.MinBaselineBytes}
}

func (r TopTalkerBaselineAnomaly) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

func (r TopTalkerBaselineAnomaly) EvaluateScoped(ctx context.Context, conn driver.Conn, _ ScopeSelector, params map[string]any) ([]Violation, error) {
	mult := paramFloat64(params, "multiplier", r.Multiplier)
	if mult < 1.0 {
		mult = 3.0
	}
	minBaseline := paramUint64(params, "min_baseline_bytes", r.MinBaselineBytes)
	if minBaseline == 0 {
		minBaseline = 1_000_000_000 // 1 GB
	}
	// Compare last hour's per-pair bytes against the average bytes per
	// (src,dst) per hour-of-day across the trailing 7 days excluding
	// the current hour. Restricting to the same hour-of-day captures
	// diurnal patterns (e.g. nightly backups). We deliberately keep
	// this expression simple — a more sophisticated baseline (median,
	// stddev, exporter-scoped) is a follow-up.
	//
	// Implementation: bucket flows by toStartOfHour(observed), sum
	// bytes per (src,dst,hour_bucket), then split rows into "this hour"
	// (one bucket — the current hour) and "baseline" (prior buckets at
	// the same hour-of-day). avg() over baseline rows yields one number
	// per (src,dst) pair regardless of how many days landed in window.
	const q = `
WITH
    toStartOfHour(now()) AS this_hour,
    toHour(now()) AS hod
SELECT
    IPv6NumToString(src_addr) AS src,
    IPv6NumToString(dst_addr) AS dst,
    sumIf(bucket_bytes, hour_bucket = this_hour)                                      AS bytes_now,
    avgIf(bucket_bytes, hour_bucket <  this_hour AND toHour(hour_bucket) = hod)       AS baseline_avg
FROM (
    SELECT
        src_addr,
        dst_addr,
        toStartOfHour(observed) AS hour_bucket,
        sum(bytes) AS bucket_bytes
    FROM flows
    WHERE observed >= now() - INTERVAL 7 DAY
    GROUP BY src_addr, dst_addr, hour_bucket
)
GROUP BY src_addr, dst_addr
HAVING bytes_now > 0
   AND baseline_avg >= ?
   AND toFloat64(bytes_now) >= toFloat64(baseline_avg) * ?
ORDER BY bytes_now DESC
LIMIT 25`
	rows, err := conn.Query(ctx, q, minBaseline, mult)
	if err != nil {
		return nil, fmt.Errorf("top_talker_baseline_anomaly: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawSrc, rawDst         string
			bytesNow               uint64
			baselineAvg            float64
		)
		if err := rows.Scan(&rawSrc, &rawDst, &bytesNow, &baselineAvg); err != nil {
			return nil, err
		}
		src := unmap4in6(rawSrc)
		dst := unmap4in6(rawDst)
		out = append(out, buildBaselineAnomalyViolation(src, dst, bytesNow, uint64(baselineAvg), mult))
	}
	return out, rows.Err()
}

func buildBaselineAnomalyViolation(src, dst string, bytesNow, baselineBytes uint64, multiplier float64) Violation {
	scope := src + " → " + dst
	ratio := 0.0
	if baselineBytes > 0 {
		ratio = float64(bytesNow) / float64(baselineBytes)
	}
	return Violation{
		Scope:    scope,
		GroupKey: "baseline_" + src + "_" + dst,
		Title: fmt.Sprintf(
			"Baseline anomaly · %s moved %s in last hour (%.1f× the 7-day same-hour baseline)",
			scope, fmtBytes(bytesNow), ratio,
		),
		Body: fmt.Sprintf(
			"This src→dst pair moved %s in the last hour. The trailing 7-day baseline for the "+
				"same hour-of-day is %s — current traffic is %.1f× that baseline (threshold %.1f×). "+
				"Investigate before treating as malicious.",
			fmtBytes(bytesNow), fmtBytes(baselineBytes), ratio, multiplier,
		),
		Severity: SeverityWarning,
		Labels: map[string]string{
			"src_addr":       src,
			"dst_addr":       dst,
			"bytes_now":      fmt.Sprintf("%d", bytesNow),
			"baseline_bytes": fmt.Sprintf("%d", baselineBytes),
			"ratio":          fmt.Sprintf("%.2f", ratio),
		},
	}
}

/* ----------------------------- helpers ----------------------------- */

// fmtBitsPerSec formats a bits-per-second value using SI multiples
// (Mbps, Gbps, …). Local helper paired with fmtBytes from rules.go.
func fmtBitsPerSec(bps uint64) string {
	const unit = 1000
	if bps < unit {
		return fmt.Sprintf("%d bps", bps)
	}
	div, exp := uint64(unit), 0
	for x := bps / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cbps", float64(bps)/float64(div), "kMGTPE"[exp])
}
