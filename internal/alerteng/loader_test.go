package alerteng

import (
	"context"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

type stubSource struct {
	rows []settings.AlertRuleSetting
	err  error
}

func (s stubSource) List(_ context.Context) ([]settings.AlertRuleSetting, error) {
	return s.rows, s.err
}

func TestLoadRules_DefaultsWhenNoOverrides(t *testing.T) {
	rules, version, err := LoadRules(context.Background(), stubSource{})
	if err != nil {
		t.Fatal(err)
	}
	if !version.IsZero() {
		t.Fatalf("expected zero version with no overrides, got %v", version)
	}
	if len(rules) != len(DefaultRules()) {
		t.Fatalf("expected %d rules, got %d", len(DefaultRules()), len(rules))
	}
	for _, r := range rules {
		if es, ok := r.(ExporterSilent); ok {
			if es.SilentSeconds != 60 {
				t.Errorf("default silent_seconds = %d; want 60", es.SilentSeconds)
			}
		}
		if ht, ok := r.(HeavyTalker); ok {
			if ht.BytesThreshold != 1<<30 {
				t.Errorf("default bytes_threshold = %d; want %d", ht.BytesThreshold, uint64(1<<30))
			}
		}
	}
}

func TestLoadRules_AppliesParamsOverride(t *testing.T) {
	now := time.Now().UTC()
	src := stubSource{rows: []settings.AlertRuleSetting{
		{
			RuleID:    "exporter_silent",
			Enabled:   true,
			Params:    map[string]any{"silent_seconds": float64(120), "active_seconds": float64(900)},
			UpdatedAt: now,
		},
	}}
	rules, version, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if !version.Equal(now) {
		t.Errorf("version = %v; want %v", version, now)
	}
	var got *ExporterSilent
	for _, r := range rules {
		if es, ok := r.(ExporterSilent); ok {
			got = &es
		}
	}
	if got == nil {
		t.Fatal("ExporterSilent rule missing from loaded set")
	}
	if got.SilentSeconds != 120 {
		t.Errorf("silent_seconds = %d; want 120 (from override)", got.SilentSeconds)
	}
	if got.ActiveSeconds != 900 {
		t.Errorf("active_seconds = %d; want 900 (from override)", got.ActiveSeconds)
	}
}

func TestLoadRules_DisabledRuleDropped(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{RuleID: "heavy_talker", Enabled: false, UpdatedAt: time.Now()},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.ID() == "heavy_talker" {
			t.Fatalf("disabled rule heavy_talker should not be loaded")
		}
	}
}

func TestLoadRules_SeverityOverride(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{RuleID: "heavy_talker", Enabled: true, Severity: SeverityCritical},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var got Rule
	for _, r := range rules {
		if r.ID() == "heavy_talker" {
			got = r
		}
	}
	if got == nil {
		t.Fatal("heavy_talker rule missing")
	}
	if got.Severity() != SeverityCritical {
		t.Errorf("severity = %q; want %q (override)", got.Severity(), SeverityCritical)
	}
}

func TestEffective_ReturnsAllRulesIncludingDisabled(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{RuleID: "heavy_talker", Enabled: false},
	}}
	eff, err := Effective(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff) != len(DefaultRules()) {
		t.Fatalf("expected %d effective rows, got %d", len(DefaultRules()), len(eff))
	}
	var ht *EffectiveRule
	for i := range eff {
		if eff[i].RuleID == "heavy_talker" {
			ht = &eff[i]
		}
	}
	if ht == nil {
		t.Fatal("heavy_talker missing from effective view")
	}
	if ht.Enabled {
		t.Error("heavy_talker should report enabled=false")
	}
	if ht.Params["window_seconds"] != 300 {
		t.Errorf("window_seconds = %v; want 300", ht.Params["window_seconds"])
	}
}

func TestLoadRules_InterfaceOperStatusOverride(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{
			RuleID:    "interface_oper_status_change",
			Enabled:   true,
			Params:    map[string]any{"debounce_seconds": float64(120), "lookback_hours": float64(48)},
			UpdatedAt: time.Now(),
		},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var got *InterfaceOperStatusChange
	for _, r := range rules {
		if x, ok := r.(InterfaceOperStatusChange); ok {
			got = &x
		}
	}
	if got == nil {
		t.Fatal("InterfaceOperStatusChange missing")
	}
	if got.DebounceSeconds != 120 {
		t.Errorf("debounce_seconds = %d; want 120", got.DebounceSeconds)
	}
	if got.LookbackHours != 48 {
		t.Errorf("lookback_hours = %d; want 48", got.LookbackHours)
	}
}

func TestLoadRules_InterfaceUtilizationOverride(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{
			RuleID:  "interface_utilization_high",
			Enabled: true,
			Params: map[string]any{
				"threshold_pct":     float64(70),
				"critical_bump_pct": float64(20),
				"window_seconds":    float64(180),
			},
			UpdatedAt: time.Now(),
		},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var got *InterfaceUtilizationHigh
	for _, r := range rules {
		if x, ok := r.(InterfaceUtilizationHigh); ok {
			got = &x
		}
	}
	if got == nil {
		t.Fatal("InterfaceUtilizationHigh missing")
	}
	if got.ThresholdPct != 70 || got.CriticalBumpPct != 20 || got.WindowSeconds != 180 {
		t.Errorf("util override not applied: %+v", got)
	}
}

func TestLoadRules_InterfaceErrorsOverride(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{
			RuleID:    "interface_errors_rate",
			Enabled:   true,
			Params:    map[string]any{"window_seconds": float64(600), "errors_per_min": float64(5)},
			UpdatedAt: time.Now(),
		},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var got *InterfaceErrorsRate
	for _, r := range rules {
		if x, ok := r.(InterfaceErrorsRate); ok {
			got = &x
		}
	}
	if got == nil {
		t.Fatal("InterfaceErrorsRate missing")
	}
	if got.WindowSeconds != 600 || got.ErrorsPerMin != 5 {
		t.Errorf("errors override not applied: %+v", got)
	}
}

func TestLoadRules_BaselineAnomalyOverride(t *testing.T) {
	src := stubSource{rows: []settings.AlertRuleSetting{
		{
			RuleID:    "top_talker_baseline_anomaly",
			Enabled:   true,
			Params:    map[string]any{"multiplier": 5.0, "min_baseline_bytes": float64(2_000_000_000)},
			UpdatedAt: time.Now(),
		},
	}}
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var got *TopTalkerBaselineAnomaly
	for _, r := range rules {
		if x, ok := r.(TopTalkerBaselineAnomaly); ok {
			got = &x
		}
	}
	if got == nil {
		t.Fatal("TopTalkerBaselineAnomaly missing")
	}
	if got.Multiplier != 5.0 {
		t.Errorf("multiplier = %v; want 5.0", got.Multiplier)
	}
	if got.MinBaselineBytes != 2_000_000_000 {
		t.Errorf("min_baseline_bytes = %d; want 2e9", got.MinBaselineBytes)
	}
}

func TestEffective_NewRulesPresent(t *testing.T) {
	eff, err := Effective(context.Background(), stubSource{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"interface_oper_status_change",
		"interface_utilization_high",
		"interface_errors_rate",
		"top_talker_baseline_anomaly",
	}
	have := map[string]EffectiveRule{}
	for _, r := range eff {
		have[r.RuleID] = r
	}
	for _, id := range want {
		r, ok := have[id]
		if !ok {
			t.Errorf("effective view missing %q", id)
			continue
		}
		if !r.Enabled {
			t.Errorf("%q should default to enabled", id)
		}
		if r.Params == nil {
			t.Errorf("%q has nil params; describe() switch needs a case", id)
		}
	}
}

func TestParamFloat64(t *testing.T) {
	p := map[string]any{
		"a": float64(1.5),
		"b": int(2),
		"c": int64(3),
		"d": "nope",
	}
	if v := paramFloat64(p, "a", 0); v != 1.5 {
		t.Errorf("a = %v; want 1.5", v)
	}
	if v := paramFloat64(p, "b", 0); v != 2 {
		t.Errorf("b = %v; want 2", v)
	}
	if v := paramFloat64(p, "c", 0); v != 3 {
		t.Errorf("c = %v; want 3", v)
	}
	if v := paramFloat64(p, "d", 9.9); v != 9.9 {
		t.Errorf("d = %v; want fallback 9.9", v)
	}
	if v := paramFloat64(p, "missing", 4.2); v != 4.2 {
		t.Errorf("missing = %v; want fallback 4.2", v)
	}
}
