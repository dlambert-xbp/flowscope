package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// RetentionTarget describes a TTL'd table FlowScope manages from
// settings. ts_column is the DateTime column the table TTLs against
// (e.g. "observed" for flows, "ts" for iface_counter_samples).
type RetentionTarget struct {
	Table    string
	TSColumn string
	// SettingKey is the app_settings.name that stores the desired
	// retention in days. The settings UI writes integers here.
	SettingKey string
	// DefaultDays is used when the setting is missing or unparseable
	// — keeps the init container working on a fresh database.
	DefaultDays int
}

// RetentionTargets is the closed set of TTL'd tables the init
// container manages. Adding a new TTL'd table to a migration means
// adding it here too — otherwise the operator-controlled retention
// knob will silently not apply.
var RetentionTargets = []RetentionTarget{
	{Table: "flows", TSColumn: "observed", SettingKey: "flow_retention_days", DefaultDays: 7},
	{Table: "iface_counter_samples", TSColumn: "ts", SettingKey: "counter_retention_days", DefaultDays: 30},
}

// BuildAlterTTLStatement renders the ALTER TABLE … MODIFY TTL
// statement for one target. Pure / no DB. Tested in isolation.
//
// ClickHouse accepts integer-valued INTERVAL N DAY expressions; days
// is asserted positive by the caller. The generated SQL is whitespace-
// stable so tests can compare strings.
func BuildAlterTTLStatement(t RetentionTarget, days int) (string, error) {
	if t.Table == "" {
		return "", errors.New("retention: table required")
	}
	if t.TSColumn == "" {
		return "", errors.New("retention: ts column required")
	}
	if days <= 0 {
		return "", fmt.Errorf("retention: days must be > 0, got %d", days)
	}
	return fmt.Sprintf(
		"ALTER TABLE %s MODIFY TTL toDateTime(%s) + INTERVAL %d DAY",
		t.Table, t.TSColumn, days,
	), nil
}

// ttlIntervalRE pulls the day count out of a stored TTL expression
// like "TTL toDateTime(observed) + toIntervalDay(7)" or
// "TTL toDateTime(observed) + INTERVAL 7 DAY". ClickHouse normalises
// the engine_full string to the toIntervalDay form on read; we accept
// both so a fresh migration (INTERVAL N DAY in the SQL file) and a
// post-ALTER state both parse correctly.
var ttlIntervalRE = regexp.MustCompile(
	`(?i)TTL\s+toDateTime\([^)]+\)\s*\+\s*(?:toIntervalDay\(\s*(\d+)\s*\)|INTERVAL\s+(\d+)\s+DAY)`,
)

// CurrentTTLDays inspects system.tables for the table's current TTL
// expression and parses out the day count. Returns (days, true, nil)
// on a successful parse; (0, false, nil) if the table exists but
// no TTL expression matches the canonical shape (someone hand-edited
// it — refuse to silently overwrite, the caller decides what to do);
// error if the lookup itself failed.
func CurrentTTLDays(ctx context.Context, conn driver.Conn, table string) (int, bool, error) {
	const q = `SELECT engine_full FROM system.tables WHERE database = currentDatabase() AND name = ?`
	var engineFull string
	if err := conn.QueryRow(ctx, q, table).Scan(&engineFull); err != nil {
		return 0, false, fmt.Errorf("retention: lookup %s: %w", table, err)
	}
	m := ttlIntervalRE.FindStringSubmatch(engineFull)
	if m == nil {
		return 0, false, nil
	}
	// One of the two capture groups will be non-empty.
	raw := m[1]
	if raw == "" {
		raw = m[2]
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// ReadRetentionDays pulls a numeric retention value from the
// app_settings table. Numbers may be stored as JSON numbers (the UI
// posts them this way) or as JSON strings (operator-edited rows);
// both are accepted. Missing rows return (default, nil) so the init
// container is a no-op on a fresh database.
func ReadRetentionDays(ctx context.Context, conn driver.Conn, key string, def int) (int, error) {
	const q = `SELECT value_json FROM app_settings FINAL WHERE name = ?`
	var raw string
	err := conn.QueryRow(ctx, q, key).Scan(&raw)
	if err != nil {
		// clickhouse-go returns this exact text for empty rowsets in
		// QueryRow.Scan. Mirror what internal/settings does.
		if err.Error() == "sql: no rows in result set" {
			return def, nil
		}
		return def, fmt.Errorf("retention: read %s: %w", key, err)
	}
	return parseDaysJSON(raw, def)
}

// parseDaysJSON decodes "30", "\"30\"", or "30.0" into a positive
// integer day count. Anything else falls back to the default — the
// init container should never wedge because someone hand-edited
// app_settings into junk.
func parseDaysJSON(raw string, def int) (int, error) {
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, "\"")
	if s == "" {
		return def, nil
	}
	// Truncate at decimal point so "30.0" → "30".
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def, nil
	}
	return n, nil
}
