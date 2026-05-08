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
