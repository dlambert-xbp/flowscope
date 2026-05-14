package alerteng

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// stubInstanceSource implements InstanceSettingsSource without
// ClickHouse so the loader can be unit-tested.
type stubInstanceSource struct {
	mu      sync.Mutex
	rows    []settings.AlertRuleInstance
	listErr error
	seeded  map[string]bool
}

func (s *stubInstanceSource) List(_ context.Context) ([]settings.AlertRuleInstance, error) {
	return s.rows, s.listErr
}

func (s *stubInstanceSource) EnsureSeed(_ context.Context, templateID, name string, defaultParams map[string]any, defaultSeverity string) (*settings.AlertRuleInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seeded == nil {
		s.seeded = map[string]bool{}
	}
	if s.seeded[templateID] {
		return &settings.AlertRuleInstance{
			InstanceID: settings.SeedInstanceID(templateID),
			TemplateID: templateID,
		}, nil
	}
	s.seeded[templateID] = true
	inst := settings.AlertRuleInstance{
		InstanceID: settings.SeedInstanceID(templateID),
		TemplateID: templateID,
		Name:       name,
		Enabled:    true,
		Severity:   defaultSeverity,
		Params:     defaultParams,
		IsSeed:     true,
		UpdatedAt:  time.Now().UTC(),
	}
	s.rows = append(s.rows, inst)
	return &inst, nil
}

func TestLoadInstanceRules_DropsDisabledInstances(t *testing.T) {
	now := time.Now().UTC()
	src := &stubInstanceSource{
		rows: []settings.AlertRuleInstance{
			{
				InstanceID: "inst_1",
				TemplateID: "device_cpu_high",
				Name:       "Disabled CPU",
				Enabled:    false,
				UpdatedAt:  now,
			},
			{
				InstanceID: "inst_2",
				TemplateID: "device_cpu_high",
				Name:       "Active CPU",
				Enabled:    true,
				UpdatedAt:  now,
			},
		},
	}
	rules, version, err := LoadInstanceRules(context.Background(), src, BuildTemplateRegistry())
	if err != nil {
		t.Fatal(err)
	}
	// Seeded instances are auto-created by EnsureSeed during load, so
	// the rule slice should include the active operator instance plus
	// seeds for every built-in template (10 of them).
	if len(rules) < 1 {
		t.Fatalf("expected at least 1 rule, got %d", len(rules))
	}
	// The disabled instance must NOT appear.
	for _, r := range rules {
		if ir, ok := r.(InstanceRule); ok && ir.InstanceID() == "inst_1" {
			t.Errorf("disabled instance was loaded")
		}
	}
	if version.IsZero() {
		t.Errorf("version is zero; want most-recent UpdatedAt")
	}
}

func TestLoadInstanceRules_SkipsUnknownTemplateID(t *testing.T) {
	src := &stubInstanceSource{
		rows: []settings.AlertRuleInstance{
			{
				InstanceID: "inst_unknown",
				TemplateID: "does_not_exist",
				Enabled:    true,
				UpdatedAt:  time.Now().UTC(),
			},
		},
	}
	rules, _, err := LoadInstanceRules(context.Background(), src, BuildTemplateRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if ir, ok := r.(InstanceRule); ok && ir.InstanceID() == "inst_unknown" {
			t.Errorf("unknown-template instance leaked into rule slice")
		}
	}
}

func TestLoadInstanceRules_NilSourceFallsBackToDefaults(t *testing.T) {
	rules, version, err := LoadInstanceRules(context.Background(), nil, BuildTemplateRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Fatal("expected default rules slice, got empty")
	}
	if !version.IsZero() {
		t.Errorf("version = %v; want zero for nil source fallback", version)
	}
}

func TestInstanceRule_AppliesSeverityOverride(t *testing.T) {
	tpl := DeviceCPUHigh{ThresholdPct: 80, CriticalBumpPct: 15, LookbackSeconds: 1800}
	inst := settings.AlertRuleInstance{
		InstanceID: "inst_x",
		TemplateID: tpl.ID(),
		Name:       "test",
		Enabled:    true,
		Severity:   "critical",
		Params:     tpl.DefaultParams(),
	}
	r := NewInstanceRule(tpl, inst)
	if r.Severity() != "critical" {
		t.Errorf("Severity() = %q; want critical", r.Severity())
	}
	if r.InstanceID() != "inst_x" {
		t.Errorf("InstanceID() = %q; want inst_x", r.InstanceID())
	}
	if r.TemplateID() != tpl.ID() {
		t.Errorf("TemplateID() = %q; want %q", r.TemplateID(), tpl.ID())
	}
}

func TestInstanceRule_FallsBackToTemplateSeverity(t *testing.T) {
	tpl := DeviceCPUHigh{ThresholdPct: 80}
	inst := settings.AlertRuleInstance{
		InstanceID: "inst_y",
		TemplateID: tpl.ID(),
		Enabled:    true,
		// no Severity — inherit
	}
	r := NewInstanceRule(tpl, inst)
	if r.Severity() != tpl.DefaultSeverity() {
		t.Errorf("Severity() = %q; want template default %q", r.Severity(), tpl.DefaultSeverity())
	}
}

func TestSeedInstanceID_RoundTrip(t *testing.T) {
	id := settings.SeedInstanceID("device_cpu_high")
	if id != "seed_device_cpu_high" {
		t.Errorf("SeedInstanceID = %q; want seed_device_cpu_high", id)
	}
	if !settings.IsSeedID(id) {
		t.Errorf("IsSeedID(%q) = false; want true", id)
	}
	if settings.IsSeedID("inst_abc") {
		t.Errorf("IsSeedID(inst_abc) = true; want false")
	}
}
