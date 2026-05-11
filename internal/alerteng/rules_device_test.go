package alerteng

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// Unit tests for the pure-function violation builders in rules_device.go
// and for loader_test-style override plumbing. End-to-end ClickHouse
// coverage lives in evaluator_device_integration_test.go.

func TestBuildResourceHighViolation_CriticalAtBump(t *testing.T) {
	row := resourceRow{
		exporter:   "10.0.0.5",
		component:  "Processor 1",
		source:     "cisco-process",
		pct:        96,
		bytesUsed:  0,
		bytesTotal: 0,
	}
	v := buildResourceHighViolation("CPU", "cpu_", row, 80, 95)
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q (96%% >= 95%% crit)", v.Severity, SeverityCritical)
	}
	if v.GroupKey != "cpu_10.0.0.5_Processor 1" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
	if !strings.Contains(v.Title, "CPU at 96%") {
		t.Errorf("title %q missing pct", v.Title)
	}
	if v.Labels["pct"] != "96" {
		t.Errorf("pct label = %q", v.Labels["pct"])
	}
	if _, ok := v.Labels["bytes_used"]; ok {
		t.Errorf("CPU row with zero bytesTotal should not emit bytes_used label")
	}
}

func TestBuildResourceHighViolation_WarnBelowBump(t *testing.T) {
	row := resourceRow{exporter: "10.0.0.5", component: "Processor 1", pct: 88}
	v := buildResourceHighViolation("CPU", "cpu_", row, 80, 95)
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want %q (88%% between thresholds)", v.Severity, SeverityWarning)
	}
}

func TestBuildResourceHighViolation_BytesRendered(t *testing.T) {
	row := resourceRow{
		exporter:   "10.0.0.5",
		component:  "Pool: Processor",
		source:     "cisco-mempool",
		pct:        92,
		bytesUsed:  900 * 1024 * 1024,
		bytesTotal: 1024 * 1024 * 1024,
	}
	v := buildResourceHighViolation("Memory", "mem_", row, 85, 95)
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning (92%% < 95%% crit)", v.Severity)
	}
	if !strings.Contains(v.Body, "Bytes used: 900.0 MiB") {
		t.Errorf("body should render bytes used: %s", v.Body)
	}
	if v.Labels["bytes_used"] != "943718400" {
		t.Errorf("bytes_used label = %q", v.Labels["bytes_used"])
	}
	if v.Labels["bytes_total"] != "1073741824" {
		t.Errorf("bytes_total label = %q", v.Labels["bytes_total"])
	}
}

func TestBuildResourceHighViolation_EmptyComponentFallsBack(t *testing.T) {
	row := resourceRow{exporter: "10.0.0.5", component: "", pct: 90}
	v := buildResourceHighViolation("Storage", "stor_", row, 85, 95)
	if !strings.Contains(v.Scope, "default") {
		t.Errorf("scope %q should contain 'default' fallback when component empty", v.Scope)
	}
	if v.GroupKey != "stor_10.0.0.5_" {
		t.Errorf("group_key = %q; want trailing underscore for empty component", v.GroupKey)
	}
}

func TestBuildUnreachableViolation_ErrorStatus(t *testing.T) {
	v := buildUnreachableViolation("10.0.0.7", "core-sw-01", "error", 1200, 2700)
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want critical", v.Severity)
	}
	if !strings.Contains(v.Title, "core-sw-01") {
		t.Errorf("title %q should include sys_name", v.Title)
	}
	if !strings.Contains(v.Title, "poll failing") {
		t.Errorf("title %q should describe the error status", v.Title)
	}
	if v.GroupKey != "unreachable_10.0.0.7" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
	if v.Labels["status"] != "error" {
		t.Errorf("status label = %q", v.Labels["status"])
	}
}

func TestBuildUnreachableViolation_StaleNoSysName(t *testing.T) {
	v := buildUnreachableViolation("10.0.0.7", "", "ok", 3600, 2700)
	if !strings.Contains(v.Title, "10.0.0.7") {
		t.Errorf("title %q should fall back to exporter address", v.Title)
	}
	if !strings.Contains(v.Title, "no successful walk in 3600s") {
		t.Errorf("title %q should describe stale reason", v.Title)
	}
	if v.Labels["age_sec"] != "3600" {
		t.Errorf("age_sec label = %q", v.Labels["age_sec"])
	}
}

func TestDefaultRules_IncludesDeviceRules(t *testing.T) {
	rules := DefaultRules()
	want := map[string]bool{
		"device_cpu_high":     false,
		"device_memory_high":  false,
		"device_storage_high": false,
		"device_unreachable":  false,
	}
	for _, r := range rules {
		if _, ok := want[r.ID()]; ok {
			want[r.ID()] = true
		}
	}
	for id, present := range want {
		if !present {
			t.Errorf("default rule %q missing from DefaultRules()", id)
		}
	}
}

// deviceOverrideSrc returns a stubSource carrying one alert_rule_settings
// row with the given rule_id, params, and a fresh UpdatedAt. Mirrors the
// inline literals in loader_test.go to keep the device-rule overrides
// readable at the call site.
func deviceOverrideSrc(ruleID string, params map[string]any) stubSource {
	return stubSource{rows: []settings.AlertRuleSetting{{
		RuleID:    ruleID,
		Enabled:   true,
		Params:    params,
		UpdatedAt: time.Now().UTC(),
	}}}
}

func TestLoadRules_DeviceCPUOverride(t *testing.T) {
	src := deviceOverrideSrc("device_cpu_high", map[string]any{
		"threshold_pct":     float64(70),
		"critical_bump_pct": float64(20),
		"lookback_seconds":  float64(900),
	})
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if x, ok := r.(DeviceCPUHigh); ok {
			if x.ThresholdPct != 70 || x.CriticalBumpPct != 20 || x.LookbackSeconds != 900 {
				t.Errorf("override not applied: %+v", x)
			}
			return
		}
	}
	t.Fatal("DeviceCPUHigh missing from loaded rule set")
}

func TestLoadRules_DeviceMemoryOverride(t *testing.T) {
	src := deviceOverrideSrc("device_memory_high", map[string]any{
		"threshold_pct":     float64(90),
		"critical_bump_pct": float64(5),
		"lookback_seconds":  float64(1200),
	})
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if x, ok := r.(DeviceMemoryHigh); ok {
			if x.ThresholdPct != 90 || x.CriticalBumpPct != 5 || x.LookbackSeconds != 1200 {
				t.Errorf("override not applied: %+v", x)
			}
			return
		}
	}
	t.Fatal("DeviceMemoryHigh missing from loaded rule set")
}

func TestLoadRules_DeviceStorageOverride(t *testing.T) {
	src := deviceOverrideSrc("device_storage_high", map[string]any{
		"threshold_pct":     float64(75),
		"critical_bump_pct": float64(15),
		"lookback_seconds":  float64(7200),
	})
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if x, ok := r.(DeviceStorageHigh); ok {
			if x.ThresholdPct != 75 || x.CriticalBumpPct != 15 || x.LookbackSeconds != 7200 {
				t.Errorf("override not applied: %+v", x)
			}
			return
		}
	}
	t.Fatal("DeviceStorageHigh missing from loaded rule set")
}

func TestLoadRules_DeviceUnreachableOverride(t *testing.T) {
	src := deviceOverrideSrc("device_unreachable", map[string]any{
		"stale_seconds":  float64(900),
		"lookback_hours": float64(48),
	})
	rules, _, err := LoadRules(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if x, ok := r.(DeviceUnreachable); ok {
			if x.StaleSeconds != 900 || x.LookbackHours != 48 {
				t.Errorf("override not applied: %+v", x)
			}
			return
		}
	}
	t.Fatal("DeviceUnreachable missing from loaded rule set")
}

func TestEffective_DeviceRulesPresent(t *testing.T) {
	eff, err := Effective(context.Background(), stubSource{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"device_cpu_high", "device_memory_high", "device_storage_high", "device_unreachable"}
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
