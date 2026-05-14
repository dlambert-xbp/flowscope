package alerteng

// template.go — the per-device alerting model.
//
// A Template is the existing concept of a Rule lifted one level: it
// owns the SQL and the violation copy, but takes the scope and
// parameters from outside. The engine no longer iterates rules; it
// iterates instances (template + scope + params + instance_id) and
// dispatches each one to its template's EvaluateScoped.
//
// Built-in rule structs (InterfaceUtilizationHigh, DeviceCPUHigh, …)
// satisfy both Rule (legacy) and Template (new). The legacy Evaluate
// stays around so existing tests keep working — it calls
// EvaluateScoped with an empty selector and the rule's own
// parameters, which is the global behavior we had before.

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// ScopeSelector aliases the settings package type so engine code can
// stay in the alerteng package without importing settings everywhere.
// Concrete shape lives in internal/settings to keep the persistence
// model in one place.
type ScopeSelector = settings.ScopeSelector

// Template is the new evaluator interface. Each built-in rule struct
// implements both Rule (legacy) and Template; the engine uses
// Template via instance loaders.
type Template interface {
	// ID returns the stable template identifier. Matches the
	// legacy Rule.ID() so seed rows in alert_rule_instances can be
	// looked up by template_id == old rule_id.
	ID() string

	// DefaultParams returns the parameter set the engine should use
	// when an instance row supplies none (or only some) of the
	// fields. The api also surfaces this in /api/alerts/templates so
	// the wizard can prefill the form.
	DefaultParams() map[string]any

	// DefaultSeverity returns the per-violation severity when the
	// instance row leaves it blank.
	DefaultSeverity() string

	// Runbook returns the human-readable description shown on the
	// alert card and in /api/alerts/templates.
	Runbook() string

	// EvaluateScoped runs the template against ClickHouse and returns
	// the currently-violating tuples. scope filters which devices /
	// interfaces / peers are considered; params provides the operator-
	// configurable knobs.
	EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error)
}

// ScopedTemplate is the small subset of templates that support scope
// filtering in their SQL. Templates that do not yet support a given
// scope dimension should ignore that field — the api validates at
// instance-creation time so an unsupported scope never reaches the
// engine.
type scopedSQL struct {
	WhereExporter  string // empty if no exporter filter
	WhereIfindex   string // empty if no ifindex filter
	Args           []any
}

// ScopeKind enumerates the scope dimensions a template understands.
// The api consults this when validating an instance's scope_json so
// we can reject "ifindex set on a template that has no interface
// concept" before it ever reaches the engine.
type ScopeKind string

const (
	ScopeKindExporter  ScopeKind = "exporter"
	ScopeKindInterface ScopeKind = "interface"
	ScopeKindBGPPeer   ScopeKind = "bgp_peer"
)

// ScopeKindsFor returns the dimensions a template supports. New
// templates declare their kinds here. Wired into the api so the
// scope-builder UI hides fields that don't apply to the chosen
// template.
func ScopeKindsFor(templateID string) []ScopeKind {
	switch templateID {
	case "exporter_silent":
		return []ScopeKind{ScopeKindExporter}
	case "heavy_talker":
		return nil // src/dst pairs are flow-level, not exporter-bound
	case "top_talker_baseline_anomaly":
		return nil
	case "interface_oper_status_change",
		"interface_utilization_high",
		"interface_errors_rate":
		return []ScopeKind{ScopeKindExporter, ScopeKindInterface}
	case "device_cpu_high",
		"device_memory_high",
		"device_storage_high",
		"device_unreachable":
		return []ScopeKind{ScopeKindExporter}
	case "bgp_neighbor_down":
		return []ScopeKind{ScopeKindExporter, ScopeKindBGPPeer}
	}
	return nil
}

// buildExporterWhere produces the SQL fragment + bind args for the
// exporter portion of a scope. The column name varies by table
// (exporter on most, c.exporter / s.exporter on joined queries) so
// the caller passes it explicitly. CIDR matching and label matching
// are no-ops in phase 1 — they accept the fields but do not yet
// translate them into SQL. The api blocks instances that try to use
// them so we don't silently match-everything.
func buildExporterWhere(col string, scope ScopeSelector) (string, []any) {
	if len(scope.Exporters) == 0 {
		return "", nil
	}
	cleaned := make([]any, 0, len(scope.Exporters))
	for _, raw := range scope.Exporters {
		ip := strings.TrimSpace(raw)
		if ip == "" {
			continue
		}
		// Normalize through netip so CH's IPv6NumToString decoder
		// agrees on the v4-mapped form. Bind as the canonical text
		// representation; CH's IPv4StringToNumOrNull / IPv6StringTo
		// equivalents handle the conversion server-side via the
		// implicit IPv6 column type.
		if addr, err := netip.ParseAddr(ip); err == nil {
			cleaned = append(cleaned, addr.String())
		} else {
			cleaned = append(cleaned, ip)
		}
	}
	if len(cleaned) == 0 {
		return "", nil
	}
	placeholders := strings.Repeat("?,", len(cleaned))
	placeholders = placeholders[:len(placeholders)-1]
	frag := fmt.Sprintf("IPv6NumToString(%s) IN (%s)", col, placeholders)
	return frag, cleaned
}

// buildIfindexWhere produces the SQL fragment + bind args for the
// ifindex portion of a scope. col is the ifindex column on the table
// being filtered (typically `ifindex` or `c.ifindex`).
func buildIfindexWhere(col string, scope ScopeSelector) (string, []any) {
	if len(scope.IfIndex) == 0 {
		return "", nil
	}
	// Stable order so the rendered SQL is deterministic — helps
	// when reading explain plans and writing tests.
	idx := append([]uint32{}, scope.IfIndex...)
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	placeholders := strings.Repeat("?,", len(idx))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(idx))
	for _, v := range idx {
		args = append(args, uint64(v))
	}
	return fmt.Sprintf("%s IN (%s)", col, placeholders), args
}

// composeScopeSQL joins zero or more scope fragments into an
// "AND (frag1) AND (frag2)" tail safe to append to an existing
// WHERE clause. Returns "" + nil when scope is empty.
func composeScopeSQL(frags []scopedSQL) (string, []any) {
	var parts []string
	var args []any
	for _, f := range frags {
		if f.WhereExporter != "" {
			parts = append(parts, "("+f.WhereExporter+")")
		}
		if f.WhereIfindex != "" {
			parts = append(parts, "("+f.WhereIfindex+")")
		}
		args = append(args, f.Args...)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(parts, " AND "), args
}

// scopeWhere builds the WHERE-clause tail and bind args for a given
// (exporter column, ifindex column, scope) triple. Either column may
// be empty to skip that dimension. The returned fragment always
// starts with a leading " AND " (or is empty).
//
// Examples:
//
//	" AND (IPv6NumToString(exporter) IN (?,?))"
//	" AND (IPv6NumToString(s.exporter) IN (?)) AND (s.ifindex IN (?,?))"
//	"" (empty scope)
func scopeWhere(exporterCol, ifindexCol string, scope ScopeSelector) (string, []any) {
	if scope.IsEmpty() {
		return "", nil
	}
	frags := make([]scopedSQL, 0, 2)
	if exporterCol != "" {
		w, a := buildExporterWhere(exporterCol, scope)
		if w != "" {
			frags = append(frags, scopedSQL{WhereExporter: w, Args: a})
		}
	}
	if ifindexCol != "" {
		w, a := buildIfindexWhere(ifindexCol, scope)
		if w != "" {
			frags = append(frags, scopedSQL{WhereIfindex: w, Args: a})
		}
	}
	return composeScopeSQL(frags)
}

// ValidateScope checks that the given scope only uses dimensions the
// template understands. Returns nil for valid scopes; a descriptive
// error for invalid ones. The api calls this on POST/PUT.
func ValidateScope(templateID string, scope ScopeSelector) error {
	supported := ScopeKindsFor(templateID)
	supportedSet := map[ScopeKind]bool{}
	for _, k := range supported {
		supportedSet[k] = true
	}
	if (len(scope.IfIndex) > 0 || scope.IfNameGlob != "") && !supportedSet[ScopeKindInterface] {
		return fmt.Errorf("template %q does not support interface-level scope", templateID)
	}
	if (len(scope.Exporters) > 0 || len(scope.ExporterCIDRs) > 0 || len(scope.ExporterLabels) > 0) && !supportedSet[ScopeKindExporter] {
		return fmt.Errorf("template %q does not support exporter-level scope", templateID)
	}
	if (len(scope.BGPPeers) > 0 || len(scope.ASNRemote) > 0) && !supportedSet[ScopeKindBGPPeer] {
		return fmt.Errorf("template %q does not support BGP-peer scope", templateID)
	}
	// Phase 1 rejection: CIDR / label / glob matching not yet
	// translated to SQL anywhere. Accepting them silently would
	// quietly match everything.
	if len(scope.ExporterCIDRs) > 0 {
		return fmt.Errorf("scope.exporter_cidrs not yet supported (phase 3)")
	}
	if len(scope.ExporterLabels) > 0 {
		return fmt.Errorf("scope.exporter_labels not yet supported (phase 3)")
	}
	if scope.IfNameGlob != "" {
		return fmt.Errorf("scope.ifname_glob not yet supported (phase 3)")
	}
	return nil
}

// TemplateRegistry is the in-process catalog of built-in templates.
// Looked up by template_id when materializing instances and when the
// api responds to GET /api/alerts/templates.
type TemplateRegistry map[string]Template

// BuiltinTemplates returns one Template per built-in rule. Order is
// stable (same as DefaultRules) so the api's templates list reads
// predictably.
func BuiltinTemplates() []Template {
	return []Template{
		ExporterSilent{SilentSeconds: 60, ActiveSeconds: 600},
		HeavyTalker{WindowSeconds: 300, BytesThreshold: 1 << 30},
		InterfaceOperStatusChange{DebounceSeconds: 60, LookbackHours: 24},
		InterfaceUtilizationHigh{ThresholdPct: 80, CriticalBumpPct: 15, WindowSeconds: 300},
		InterfaceErrorsRate{WindowSeconds: 300, ErrorsPerMin: 10},
		TopTalkerBaselineAnomaly{Multiplier: 3.0, MinBaselineBytes: 1_000_000_000},
		DeviceCPUHigh{ThresholdPct: 80, CriticalBumpPct: 15, LookbackSeconds: 1800},
		DeviceMemoryHigh{ThresholdPct: 85, CriticalBumpPct: 10, LookbackSeconds: 1800},
		DeviceStorageHigh{ThresholdPct: 85, CriticalBumpPct: 10, LookbackSeconds: 3600},
		DeviceUnreachable{StaleSeconds: 2700, LookbackHours: 24},
		BGPNeighborDown{EstablishedMinSeconds: 60, LookbackSeconds: 3600},
	}
}

// BuildTemplateRegistry returns a lookup keyed by template_id.
func BuildTemplateRegistry() TemplateRegistry {
	out := TemplateRegistry{}
	for _, t := range BuiltinTemplates() {
		out[t.ID()] = t
	}
	return out
}
