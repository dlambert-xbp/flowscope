package alerteng

import (
	"context"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// RuleSettingsSource is the small slice of settings.AlertRulesStore
// that the alert engine actually needs. Defining it here keeps
// internal/alerteng independent of the full settings store
// constructor — useful for tests and avoids importing the whole
// settings DI graph.
type RuleSettingsSource interface {
	List(ctx context.Context) ([]settings.AlertRuleSetting, error)
}

// LoadRules returns the rule set with operator overrides from
// alert_rule_settings applied on top of DefaultRules(). version is
// max(updated_at) across the override rows; the engine uses it to
// decide whether to rebuild on the next refresh tick. Rules whose
// override row has enabled=false are dropped from the returned
// slice — disabled means "do not evaluate".
//
// Unknown rule_ids in the table are ignored. Missing overrides leave
// the corresponding default rule in place, unmodified.
func LoadRules(ctx context.Context, src RuleSettingsSource) ([]Rule, time.Time, error) {
	if src == nil {
		return DefaultRules(), time.Time{}, nil
	}
	overrides, err := src.List(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	byID := make(map[string]settings.AlertRuleSetting, len(overrides))
	var version time.Time
	for _, o := range overrides {
		byID[o.RuleID] = o
		if o.UpdatedAt.After(version) {
			version = o.UpdatedAt
		}
	}

	defaults := DefaultRules()
	out := make([]Rule, 0, len(defaults))
	for _, base := range defaults {
		o, hasOverride := byID[base.ID()]
		if hasOverride && !o.Enabled {
			continue
		}
		out = append(out, applyOverride(base, o, hasOverride))
	}
	return out, version, nil
}

// applyOverride returns the rule with the operator override merged in.
// When hasOverride is false the original rule is returned unchanged.
func applyOverride(base Rule, o settings.AlertRuleSetting, hasOverride bool) Rule {
	if !hasOverride {
		return base
	}
	params := paramsMap(o.Params)
	switch r := base.(type) {
	case ExporterSilent:
		r.SilentSeconds = paramInt(params, "silent_seconds", r.SilentSeconds)
		r.ActiveSeconds = paramInt(params, "active_seconds", r.ActiveSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case HeavyTalker:
		r.WindowSeconds = paramInt(params, "window_seconds", r.WindowSeconds)
		r.BytesThreshold = paramUint64(params, "bytes_threshold", r.BytesThreshold)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case InterfaceOperStatusChange:
		r.DebounceSeconds = paramInt(params, "debounce_seconds", r.DebounceSeconds)
		r.LookbackHours = paramInt(params, "lookback_hours", r.LookbackHours)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case InterfaceUtilizationHigh:
		r.ThresholdPct = paramInt(params, "threshold_pct", r.ThresholdPct)
		r.CriticalBumpPct = paramInt(params, "critical_bump_pct", r.CriticalBumpPct)
		r.WindowSeconds = paramInt(params, "window_seconds", r.WindowSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case InterfaceErrorsRate:
		r.WindowSeconds = paramInt(params, "window_seconds", r.WindowSeconds)
		r.ErrorsPerMin = paramInt(params, "errors_per_min", r.ErrorsPerMin)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case TopTalkerBaselineAnomaly:
		r.Multiplier = paramFloat64(params, "multiplier", r.Multiplier)
		r.MinBaselineBytes = paramUint64(params, "min_baseline_bytes", r.MinBaselineBytes)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case DeviceCPUHigh:
		r.ThresholdPct = paramInt(params, "threshold_pct", r.ThresholdPct)
		r.CriticalBumpPct = paramInt(params, "critical_bump_pct", r.CriticalBumpPct)
		r.LookbackSeconds = paramInt(params, "lookback_seconds", r.LookbackSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case DeviceMemoryHigh:
		r.ThresholdPct = paramInt(params, "threshold_pct", r.ThresholdPct)
		r.CriticalBumpPct = paramInt(params, "critical_bump_pct", r.CriticalBumpPct)
		r.LookbackSeconds = paramInt(params, "lookback_seconds", r.LookbackSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case DeviceStorageHigh:
		r.ThresholdPct = paramInt(params, "threshold_pct", r.ThresholdPct)
		r.CriticalBumpPct = paramInt(params, "critical_bump_pct", r.CriticalBumpPct)
		r.LookbackSeconds = paramInt(params, "lookback_seconds", r.LookbackSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case DeviceUnreachable:
		r.StaleSeconds = paramInt(params, "stale_seconds", r.StaleSeconds)
		r.LookbackHours = paramInt(params, "lookback_hours", r.LookbackHours)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	case BGPNeighborDown:
		r.EstablishedMinSeconds = paramInt(params, "established_min_seconds", r.EstablishedMinSeconds)
		r.LookbackSeconds = paramInt(params, "lookback_seconds", r.LookbackSeconds)
		if o.Severity != "" {
			return severityWrap{Rule: r, severity: o.Severity}
		}
		return r
	default:
		if o.Severity != "" {
			return severityWrap{Rule: base, severity: o.Severity}
		}
		return base
	}
}

// severityWrap overrides a rule's reported severity. Violations the
// underlying rule emits without an explicit severity inherit this
// value through Rule.Severity().
type severityWrap struct {
	Rule
	severity string
}

func (w severityWrap) Severity() string { return w.severity }

// EffectiveRule is the JSON-friendly view of an applied rule. It
// reflects what the engine is actually running after defaults +
// operator overrides are merged.
type EffectiveRule struct {
	RuleID   string         `json:"rule_id"`
	Enabled  bool           `json:"enabled"`
	Severity string         `json:"severity"`
	Params   map[string]any `json:"params"`
}

// Effective returns one row per built-in rule with the effective
// settings the engine is running. Disabled rules are returned with
// enabled=false rather than omitted so the UI can render every
// available rule alongside its current state.
func Effective(ctx context.Context, src RuleSettingsSource) ([]EffectiveRule, error) {
	overrides := map[string]settings.AlertRuleSetting{}
	if src != nil {
		rows, err := src.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range rows {
			overrides[o.RuleID] = o
		}
	}
	defaults := DefaultRules()
	out := make([]EffectiveRule, 0, len(defaults))
	for _, base := range defaults {
		o, hasOverride := overrides[base.ID()]
		applied := applyOverride(base, o, hasOverride)
		eff := EffectiveRule{
			RuleID:   base.ID(),
			Enabled:  !hasOverride || o.Enabled,
			Severity: applied.Severity(),
			Params:   describe(applied),
		}
		out = append(out, eff)
	}
	return out, nil
}

// describe extracts the active parameter values from a rule. The
// switch must stay aligned with the cases handled in applyOverride.
func describe(r Rule) map[string]any {
	switch x := r.(type) {
	case ExporterSilent:
		return map[string]any{"silent_seconds": x.SilentSeconds, "active_seconds": x.ActiveSeconds}
	case HeavyTalker:
		return map[string]any{"window_seconds": x.WindowSeconds, "bytes_threshold": x.BytesThreshold}
	case InterfaceOperStatusChange:
		return map[string]any{"debounce_seconds": x.DebounceSeconds, "lookback_hours": x.LookbackHours}
	case InterfaceUtilizationHigh:
		return map[string]any{
			"threshold_pct":     x.ThresholdPct,
			"critical_bump_pct": x.CriticalBumpPct,
			"window_seconds":    x.WindowSeconds,
		}
	case InterfaceErrorsRate:
		return map[string]any{"window_seconds": x.WindowSeconds, "errors_per_min": x.ErrorsPerMin}
	case TopTalkerBaselineAnomaly:
		return map[string]any{"multiplier": x.Multiplier, "min_baseline_bytes": x.MinBaselineBytes}
	case DeviceCPUHigh:
		return map[string]any{
			"threshold_pct":     x.ThresholdPct,
			"critical_bump_pct": x.CriticalBumpPct,
			"lookback_seconds":  x.LookbackSeconds,
		}
	case DeviceMemoryHigh:
		return map[string]any{
			"threshold_pct":     x.ThresholdPct,
			"critical_bump_pct": x.CriticalBumpPct,
			"lookback_seconds":  x.LookbackSeconds,
		}
	case DeviceStorageHigh:
		return map[string]any{
			"threshold_pct":     x.ThresholdPct,
			"critical_bump_pct": x.CriticalBumpPct,
			"lookback_seconds":  x.LookbackSeconds,
		}
	case DeviceUnreachable:
		return map[string]any{
			"stale_seconds":  x.StaleSeconds,
			"lookback_hours": x.LookbackHours,
		}
	case BGPNeighborDown:
		return map[string]any{
			"established_min_seconds": x.EstablishedMinSeconds,
			"lookback_seconds":        x.LookbackSeconds,
		}
	case severityWrap:
		return describe(x.Rule)
	}
	return nil
}

func paramsMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func paramInt(p map[string]any, key string, fallback int) int {
	v, ok := p[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return fallback
}

func paramFloat64(p map[string]any, key string, fallback float64) float64 {
	v, ok := p[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return fallback
}

func paramUint64(p map[string]any, key string, fallback uint64) uint64 {
	v, ok := p[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return fallback
		}
		return uint64(n)
	case int:
		if n < 0 {
			return fallback
		}
		return uint64(n)
	case int64:
		if n < 0 {
			return fallback
		}
		return uint64(n)
	case uint64:
		return n
	}
	return fallback
}
