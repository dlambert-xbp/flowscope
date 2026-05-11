package alerteng

// rules_device.go — chassis-level alert rules driven by SNMP.
//
// These four rules complement the interface-level SNMP rules in
// rules_extra.go. Where those fire on link events (oper-status
// transitions, errors, link saturation), these fire on device events
// (CPU saturation, memory exhaustion, full filesystem, SNMP polling
// failure). All read the snmp service's tables — no new ingest path,
// no new schema.
//
// Source tables:
//   - device_resource_samples (kind = cpu / memory / storage / …)
//   - device_inventory        (poll_status = ok | partial | error)
//
// Shape, mirroring rules_extra.go:
//   - argMax(<col>, polled_at) for the latest per-(exporter, component)
//     reading
//   - WHERE polled_at >= now() - INTERVAL <lookback> SECOND so a long
//     dead device cannot keep firing
//   - HAVING <pct or unreachable predicate>
//
// Severity escalates from warning to critical at ThresholdPct +
// CriticalBumpPct, same single-knob pattern InterfaceUtilizationHigh
// uses.

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

/* ----------------------------- DeviceCPUHigh ----------------------------- */

// DeviceCPUHigh fires when the most recent value_percent on a
// (exporter, cpu component) row within LookbackSeconds is at or above
// ThresholdPct. The CPU metric is overloaded across MIBs (cisco-process
// reports a 5-min average, hrmib a 1-min average) — the rule does not
// try to normalize across sources, it just reads what the device gave
// us. The `source` label on the violation lets operators tell which
// MIB drove the alert.
type DeviceCPUHigh struct {
	ThresholdPct    int
	CriticalBumpPct int
	LookbackSeconds int
}

func (DeviceCPUHigh) ID() string       { return "device_cpu_high" }
func (DeviceCPUHigh) Severity() string { return SeverityWarning }
func (DeviceCPUHigh) Runbook() string {
	return "Fires when an SNMP-polled CPU is at or above the configured percentage of its " +
		"capacity. Common causes: runaway management process, hot path through the data " +
		"plane, control-plane policer event. Cross-check the Devices summary for the CPU " +
		"sparkline and inspect the `source` label to see which MIB drove the reading."
}

func (r DeviceCPUHigh) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	threshold := r.ThresholdPct
	if threshold <= 0 {
		threshold = 80
	}
	bump := r.CriticalBumpPct
	if bump <= 0 {
		bump = 15
	}
	lookback := r.LookbackSeconds
	if lookback <= 0 {
		lookback = 1800
	}
	rows, err := queryResourcePct(ctx, conn, "cpu", lookback, threshold)
	if err != nil {
		return nil, fmt.Errorf("device_cpu_high: %w", err)
	}
	out := make([]Violation, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildResourceHighViolation(
			"CPU", "cpu_", row, threshold, threshold+bump,
		))
	}
	return out, nil
}

/* ----------------------------- DeviceMemoryHigh ----------------------------- */

// DeviceMemoryHigh fires when a memory pool's used/total ratio (or
// reported value_percent when the bytes columns are zero) crosses
// ThresholdPct on its latest reading. Pool granularity is the device's
// — Cisco gear typically exposes "Processor" / "I/O" pools separately,
// hrmib lumps everything as one "Physical memory" row. Either way we
// alert per-component so a tight I/O pool doesn't bury a healthy
// Processor pool.
type DeviceMemoryHigh struct {
	ThresholdPct    int
	CriticalBumpPct int
	LookbackSeconds int
}

func (DeviceMemoryHigh) ID() string       { return "device_memory_high" }
func (DeviceMemoryHigh) Severity() string { return SeverityWarning }
func (DeviceMemoryHigh) Runbook() string {
	return "Fires when an SNMP-polled memory pool is at or above the configured percentage " +
		"of its capacity. Percent is computed from value_bytes / max_bytes when both are " +
		"present; otherwise the device's reported value_percent is used. Common causes: " +
		"memory leak in a daemon, stuck buffer queue, under-provisioned device."
}

func (r DeviceMemoryHigh) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	threshold := r.ThresholdPct
	if threshold <= 0 {
		threshold = 85
	}
	bump := r.CriticalBumpPct
	if bump <= 0 {
		bump = 10
	}
	lookback := r.LookbackSeconds
	if lookback <= 0 {
		lookback = 1800
	}
	rows, err := queryResourcePct(ctx, conn, "memory", lookback, threshold)
	if err != nil {
		return nil, fmt.Errorf("device_memory_high: %w", err)
	}
	out := make([]Violation, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildResourceHighViolation(
			"Memory", "mem_", row, threshold, threshold+bump,
		))
	}
	return out, nil
}

/* ----------------------------- DeviceStorageHigh ----------------------------- */

// DeviceStorageHigh fires per-filesystem when used bytes cross the
// percentage threshold of total bytes. Storage moves slowly, so the
// default LookbackSeconds is longer than the CPU/memory rules — there
// is no need to react in the same window as a CPU spike.
type DeviceStorageHigh struct {
	ThresholdPct    int
	CriticalBumpPct int
	LookbackSeconds int
}

func (DeviceStorageHigh) ID() string       { return "device_storage_high" }
func (DeviceStorageHigh) Severity() string { return SeverityWarning }
func (DeviceStorageHigh) Runbook() string {
	return "Fires when a filesystem on a polled device crosses the configured percentage " +
		"of its capacity. On network gear this is typically bootflash, flash, or harddisk; " +
		"on Linux/BSD targets it is each mounted filesystem from hrStorageTable. Common " +
		"causes: log directory growth, accumulated crashinfo, retained images from a " +
		"prior upgrade."
}

func (r DeviceStorageHigh) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	threshold := r.ThresholdPct
	if threshold <= 0 {
		threshold = 85
	}
	bump := r.CriticalBumpPct
	if bump <= 0 {
		bump = 10
	}
	lookback := r.LookbackSeconds
	if lookback <= 0 {
		lookback = 3600
	}
	rows, err := queryResourcePct(ctx, conn, "storage", lookback, threshold)
	if err != nil {
		return nil, fmt.Errorf("device_storage_high: %w", err)
	}
	out := make([]Violation, 0, len(rows))
	for _, row := range rows {
		out = append(out, buildResourceHighViolation(
			"Storage", "stor_", row, threshold, threshold+bump,
		))
	}
	return out, nil
}

/* ----------------------------- DeviceUnreachable ----------------------------- */

// DeviceUnreachable fires for exporters that the snmp scheduler has
// either walked unsuccessfully (latest poll_status = "error") or has
// not walked at all within StaleSeconds. LookbackHours bounds the
// universe: an exporter that has not been walked in the lookback
// window drops out entirely (we don't want to alert forever on a
// decommissioned device). VISION.md §6 lists "device unreachable"
// alongside gNMI subscription drops; this rule is the SNMP-only slice
// and will pair with a gNMI signal once that ingest path exists.
type DeviceUnreachable struct {
	StaleSeconds  int
	LookbackHours int
}

func (DeviceUnreachable) ID() string       { return "device_unreachable" }
func (DeviceUnreachable) Severity() string { return SeverityCritical }
func (DeviceUnreachable) Runbook() string {
	return "Fires when SNMP polling for a previously-seen device is failing or has gone " +
		"silent. Common causes: device powered off, ACL or firewall change blocking " +
		"161/udp, SNMP daemon crashed, or v3 credentials rotated server-side without " +
		"updating the FlowScope binding. The Devices view shows the most recent walk's " +
		"timestamp and poll_status."
}

func (r DeviceUnreachable) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	stale := r.StaleSeconds
	if stale <= 0 {
		stale = 2700 // 3× the default 15-min walk interval
	}
	lookbackHours := r.LookbackHours
	if lookbackHours <= 0 {
		lookbackHours = 24
	}
	const q = `
SELECT IPv6NumToString(exporter)      AS exporter_ip,
       argMax(poll_status, polled_at) AS status,
       argMax(sys_name, polled_at)    AS sys_name,
       max(polled_at)                 AS latest
FROM device_inventory
WHERE polled_at >= now() - INTERVAL ? HOUR
GROUP BY exporter
HAVING (status = 'error' AND latest >= now() - INTERVAL ? SECOND)
    OR latest < now() - INTERVAL ? SECOND`
	rows, err := conn.Query(ctx, q,
		uint64(lookbackHours), uint64(stale), uint64(stale),
	)
	if err != nil {
		return nil, fmt.Errorf("device_unreachable: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawExporter, status, sysName string
			latest                       time.Time
		)
		if err := rows.Scan(&rawExporter, &status, &sysName, &latest); err != nil {
			return nil, err
		}
		exporter := unmap4in6(rawExporter)
		ageSec := uint64(0)
		if d := time.Since(latest); d > 0 {
			ageSec = uint64(d.Seconds())
		}
		out = append(out, buildUnreachableViolation(exporter, sysName, status, ageSec, stale))
	}
	return out, rows.Err()
}

/* ----------------------------- shared plumbing ----------------------------- */

// resourceRow bundles the columns the three percent-threshold rules
// share. Pulled out so the SQL lives in one place; the Evaluate methods
// only differ by kind / threshold / lookback / violation copy.
type resourceRow struct {
	exporter   string
	component  string
	source     string
	pct        int
	bytesUsed  uint64
	bytesTotal uint64
}

// queryResourcePct returns the latest argMax reading per (exporter,
// component) for `kind`, then filters down to rows whose normalized
// percent is at or above `thresholdPct`. Percent is bytes_used /
// bytes_total when both are non-zero (memory / storage), otherwise the
// device-reported value_percent (cpu, or the bytes-less subset of
// memory/storage rows).
func queryResourcePct(
	ctx context.Context, conn driver.Conn,
	kind string, lookbackSeconds, thresholdPct int,
) ([]resourceRow, error) {
	const q = `
SELECT
    exporter_ip, component, source, bytes_used, bytes_total, pct
FROM (
    SELECT
        IPv6NumToString(exporter)        AS exporter_ip,
        component,
        argMax(value_bytes, polled_at)   AS bytes_used,
        argMax(max_bytes, polled_at)     AS bytes_total,
        argMax(value_percent, polled_at) AS pct_raw,
        argMax(source, polled_at)        AS source,
        toUInt32(if(argMax(max_bytes, polled_at) > 0,
                    toFloat64(argMax(value_bytes, polled_at)) * 100
                        / toFloat64(argMax(max_bytes, polled_at)),
                    argMax(value_percent, polled_at))) AS pct
    FROM device_resource_samples
    WHERE kind = ? AND polled_at >= now() - INTERVAL ? SECOND
    GROUP BY exporter, component
) AS s
WHERE pct >= ?`
	rows, err := conn.Query(ctx, q,
		kind, uint64(lookbackSeconds), uint64(thresholdPct),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resourceRow
	for rows.Next() {
		var (
			rawExporter, component, source string
			bytesUsed, bytesTotal          uint64
			pct                            uint32
		)
		if err := rows.Scan(&rawExporter, &component, &source, &bytesUsed, &bytesTotal, &pct); err != nil {
			return nil, err
		}
		out = append(out, resourceRow{
			exporter:   unmap4in6(rawExporter),
			component:  component,
			source:     source,
			pct:        int(pct),
			bytesUsed:  bytesUsed,
			bytesTotal: bytesTotal,
		})
	}
	return out, rows.Err()
}

// buildResourceHighViolation is the pure-function violation formatter
// the three percent rules share. groupKeyPrefix is the dedup-key
// namespace ("cpu_", "mem_", "stor_") so that simultaneous CPU + memory
// breaches on the same (exporter, component) become two distinct
// alerts rather than collapsing onto one.
func buildResourceHighViolation(
	kindLabel, groupKeyPrefix string,
	row resourceRow, warnAt, critAt int,
) Violation {
	component := row.component
	if component == "" {
		component = "default"
	}
	severity := SeverityWarning
	if row.pct >= critAt {
		severity = SeverityCritical
	}
	scope := fmt.Sprintf("%s · %s", row.exporter, component)
	title := fmt.Sprintf("%s at %d%% on %s · %s", kindLabel, row.pct, row.exporter, component)
	body := fmt.Sprintf(
		"%s utilization on %s component %q is %d%% (threshold %d%% warning / %d%% critical). ",
		kindLabel, row.exporter, component, row.pct, warnAt, critAt,
	)
	if row.bytesTotal > 0 {
		body += fmt.Sprintf("Bytes used: %s of %s. ", fmtBytes(row.bytesUsed), fmtBytes(row.bytesTotal))
	}
	if row.source != "" {
		body += fmt.Sprintf("Source: %s.", row.source)
	}
	labels := map[string]string{
		"exporter":  row.exporter,
		"component": row.component,
		"pct":       fmt.Sprintf("%d", row.pct),
		"source":    row.source,
	}
	if row.bytesTotal > 0 {
		labels["bytes_used"] = fmt.Sprintf("%d", row.bytesUsed)
		labels["bytes_total"] = fmt.Sprintf("%d", row.bytesTotal)
	}
	return Violation{
		Scope:    scope,
		GroupKey: groupKeyPrefix + row.exporter + "_" + row.component,
		Title:    title,
		Body:     body,
		Severity: severity,
		Labels:   labels,
	}
}

// buildUnreachableViolation formats the device_unreachable violation.
// The reason string distinguishes "poll has been failing" from "we
// haven't even attempted a recent walk" — operators investigate those
// two cases very differently.
func buildUnreachableViolation(exporter, sysName, status string, ageSec uint64, staleAt int) Violation {
	label := exporter
	if sysName != "" {
		label = fmt.Sprintf("%s (%s)", sysName, exporter)
	}
	reason := "SNMP poll failing"
	if status != "error" {
		reason = fmt.Sprintf("no successful walk in %ds", ageSec)
	}
	return Violation{
		Scope:    exporter,
		GroupKey: "unreachable_" + exporter,
		Title:    fmt.Sprintf("Device %s unreachable: %s", label, reason),
		Body: fmt.Sprintf(
			"FlowScope has not collected a successful SNMP walk from %s recently. Last "+
				"successful walk was %ds ago; the configured stale threshold is %ds. "+
				"Most recent poll_status: %q. Check the device's reachability, the SNMP "+
				"daemon, and the credential binding in Settings → SNMP.",
			label, ageSec, staleAt, status,
		),
		Severity: SeverityCritical,
		Labels: map[string]string{
			"exporter": exporter,
			"sys_name": sysName,
			"status":   status,
			"age_sec":  fmt.Sprintf("%d", ageSec),
		},
	}
}
