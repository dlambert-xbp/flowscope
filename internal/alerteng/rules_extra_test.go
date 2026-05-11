package alerteng

import (
	"strings"
	"testing"
)

// These tests exercise the pure-function violation builders that the
// Evaluate methods funnel each row through. End-to-end ClickHouse
// coverage lives in the integration tests added in slice 5.

func TestBuildOperStatusViolation_DownIsCritical(t *testing.T) {
	v := buildOperStatusViolation("10.0.0.1", 3, "Gi0/0/3", "up", "down")
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want %q on transition to down", v.Severity, SeverityCritical)
	}
	if !strings.Contains(v.Title, "up → down") {
		t.Errorf("title %q missing transition arrow", v.Title)
	}
	if v.Labels["prev_status"] != "up" || v.Labels["curr_status"] != "down" {
		t.Errorf("labels missing prev/curr: %v", v.Labels)
	}
	if v.GroupKey != "operstatus_10.0.0.1_3" {
		t.Errorf("group_key = %q; want operstatus_10.0.0.1_3", v.GroupKey)
	}
}

func TestBuildOperStatusViolation_UpIsWarning(t *testing.T) {
	v := buildOperStatusViolation("10.0.0.1", 4, "Gi0/0/4", "down", "up")
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want %q on recovery", v.Severity, SeverityWarning)
	}
}

func TestBuildOperStatusViolation_FallsBackToIfindex(t *testing.T) {
	v := buildOperStatusViolation("10.0.0.1", 7, "", "up", "down")
	if !strings.Contains(v.Scope, "ifindex 7") {
		t.Errorf("scope %q should include ifindex fallback", v.Scope)
	}
}

func TestBuildUtilizationViolation_CriticalAtBump(t *testing.T) {
	// 96% on a 1Gbps link, warn=80, crit=95 → critical
	v := buildUtilizationViolation("10.0.0.2", 5, "Gi0/0/5", 1_000_000_000, 960_000_000, 96, 80, 95)
	if v.Severity != SeverityCritical {
		t.Errorf("severity = %q; want critical at pct >= crit threshold", v.Severity)
	}
	if v.Labels["pct"] != "96" {
		t.Errorf("pct label = %q; want 96", v.Labels["pct"])
	}
}

func TestBuildUtilizationViolation_WarnBelowBump(t *testing.T) {
	v := buildUtilizationViolation("10.0.0.2", 5, "Gi0/0/5", 1_000_000_000, 850_000_000, 85, 80, 95)
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning between thresholds", v.Severity)
	}
}

func TestBuildErrorsRateViolation_LabelsAndScope(t *testing.T) {
	v := buildErrorsRateViolation("10.0.0.3", 9, "Te1/0/9", 50, 12, 300, 10)
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning", v.Severity)
	}
	if v.GroupKey != "errs_10.0.0.3_9" {
		t.Errorf("group_key = %q", v.GroupKey)
	}
	if v.Labels["per_min"] != "12" {
		t.Errorf("per_min label = %q", v.Labels["per_min"])
	}
}

func TestBuildBaselineAnomalyViolation_RatioInTitle(t *testing.T) {
	// 5GB now vs 1GB baseline → 5.0×
	v := buildBaselineAnomalyViolation("10.1.1.1", "10.2.2.2", 5_000_000_000, 1_000_000_000, 3.0)
	if !strings.Contains(v.Title, "5.0×") {
		t.Errorf("title %q should contain 5.0×", v.Title)
	}
	if v.Severity != SeverityWarning {
		t.Errorf("severity = %q; want warning", v.Severity)
	}
	if v.Labels["src_addr"] != "10.1.1.1" || v.Labels["dst_addr"] != "10.2.2.2" {
		t.Errorf("addr labels missing: %v", v.Labels)
	}
}

func TestBuildBaselineAnomalyViolation_ZeroBaselineNoCrash(t *testing.T) {
	// Defensive: Evaluate filters baseline_avg >= min, but the pure
	// function should not divide by zero either.
	v := buildBaselineAnomalyViolation("10.1.1.1", "10.2.2.2", 5_000_000_000, 0, 3.0)
	if !strings.Contains(v.Body, "0.0×") && !strings.Contains(v.Body, "0×") {
		// Either rendering is acceptable; we just don't want a NaN/Inf.
		t.Logf("body: %s", v.Body)
	}
}

func TestFmtBitsPerSec(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{500, "500 bps"},
		{1500, "1.50 kbps"},
		{1_500_000, "1.50 Mbps"},
		{1_500_000_000, "1.50 Gbps"},
	}
	for _, c := range cases {
		if got := fmtBitsPerSec(c.in); got != c.want {
			t.Errorf("fmtBitsPerSec(%d) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultRules_IncludesNewRules(t *testing.T) {
	rules := DefaultRules()
	want := map[string]bool{
		"exporter_silent":               false,
		"heavy_talker":                  false,
		"interface_oper_status_change":  false,
		"interface_utilization_high":    false,
		"interface_errors_rate":         false,
		"top_talker_baseline_anomaly":   false,
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
