package alerteng

import (
	"strings"
	"testing"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

func TestScopeWhere_EmptyScope_ReturnsEmpty(t *testing.T) {
	frag, args := scopeWhere("exporter", "ifindex", settings.ScopeSelector{})
	if frag != "" {
		t.Errorf("frag = %q; want empty", frag)
	}
	if len(args) != 0 {
		t.Errorf("args = %v; want empty", args)
	}
}

func TestScopeWhere_ExporterOnly(t *testing.T) {
	scope := settings.ScopeSelector{Exporters: []string{"10.0.0.1", "10.0.0.2"}}
	frag, args := scopeWhere("exporter", "ifindex", scope)
	if !strings.Contains(frag, "IPv6NumToString(exporter) IN (?,?)") {
		t.Errorf("frag = %q; want exporter IN clause", frag)
	}
	if !strings.HasPrefix(frag, " AND ") {
		t.Errorf("frag = %q; want leading ' AND '", frag)
	}
	if len(args) != 2 {
		t.Fatalf("args len = %d; want 2", len(args))
	}
}

func TestScopeWhere_IfindexOnly(t *testing.T) {
	scope := settings.ScopeSelector{IfIndex: []uint32{3, 1, 2}}
	frag, args := scopeWhere("exporter", "ifindex", scope)
	if !strings.Contains(frag, "ifindex IN (?,?,?)") {
		t.Errorf("frag = %q; want ifindex IN clause", frag)
	}
	if len(args) != 3 {
		t.Fatalf("args len = %d; want 3", len(args))
	}
	// Args should be sorted ascending so the SQL is deterministic.
	if args[0].(uint64) != 1 || args[1].(uint64) != 2 || args[2].(uint64) != 3 {
		t.Errorf("args = %v; want sorted ascending [1,2,3]", args)
	}
}

func TestScopeWhere_ExporterAndIfindex(t *testing.T) {
	scope := settings.ScopeSelector{
		Exporters: []string{"10.0.0.1"},
		IfIndex:   []uint32{1, 2},
	}
	frag, args := scopeWhere("c.exporter", "c.ifindex", scope)
	if !strings.Contains(frag, "IPv6NumToString(c.exporter)") {
		t.Errorf("frag = %q; want column substitution", frag)
	}
	if !strings.Contains(frag, "c.ifindex IN") {
		t.Errorf("frag = %q; want ifindex on aliased column", frag)
	}
	if len(args) != 3 {
		t.Fatalf("args len = %d; want 3 (1 exporter + 2 ifindex)", len(args))
	}
}

func TestScopeWhere_SkipsEmptyColumnArgs(t *testing.T) {
	scope := settings.ScopeSelector{IfIndex: []uint32{1}}
	frag, args := scopeWhere("", "ifindex", scope)
	if !strings.Contains(frag, "ifindex IN (?)") {
		t.Errorf("frag = %q", frag)
	}
	if len(args) != 1 {
		t.Fatalf("args len = %d; want 1", len(args))
	}
}

func TestValidateScope_RejectsUnsupportedDimensions(t *testing.T) {
	cases := []struct {
		name       string
		templateID string
		scope      settings.ScopeSelector
		wantErr    string
	}{
		{
			name:       "ifindex on cpu template",
			templateID: "device_cpu_high",
			scope:      settings.ScopeSelector{IfIndex: []uint32{1}},
			wantErr:    "interface-level scope",
		},
		{
			name:       "exporter on heavy_talker (no scope kinds)",
			templateID: "heavy_talker",
			scope:      settings.ScopeSelector{Exporters: []string{"10.0.0.1"}},
			wantErr:    "exporter-level scope",
		},
		{
			name:       "labels rejected as phase 3 not yet implemented",
			templateID: "device_cpu_high",
			scope:      settings.ScopeSelector{ExporterLabels: map[string]string{"role": "core"}},
			wantErr:    "exporter_labels not yet supported",
		},
		{
			name:       "cidrs rejected as phase 3 not yet implemented",
			templateID: "device_cpu_high",
			scope:      settings.ScopeSelector{ExporterCIDRs: []string{"10.0.0.0/24"}},
			wantErr:    "exporter_cidrs not yet supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScope(tc.templateID, tc.scope)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateScope_AcceptsSupportedDimensions(t *testing.T) {
	cases := []struct {
		name       string
		templateID string
		scope      settings.ScopeSelector
	}{
		{"empty", "device_cpu_high", settings.ScopeSelector{}},
		{"exporter on cpu", "device_cpu_high", settings.ScopeSelector{Exporters: []string{"10.0.0.1"}}},
		{"interface scope", "interface_utilization_high", settings.ScopeSelector{Exporters: []string{"10.0.0.1"}, IfIndex: []uint32{1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateScope(tc.templateID, tc.scope); err != nil {
				t.Errorf("ValidateScope returned error: %v", err)
			}
		})
	}
}

func TestBuildTemplateRegistry_ContainsAllBuiltins(t *testing.T) {
	registry := BuildTemplateRegistry()
	for _, t2 := range BuiltinTemplates() {
		if _, ok := registry[t2.ID()]; !ok {
			t.Errorf("registry missing %q", t2.ID())
		}
	}
}

func TestScopeKindsFor_KnownTemplates(t *testing.T) {
	cases := map[string][]ScopeKind{
		"exporter_silent":             {ScopeKindExporter},
		"interface_utilization_high":  {ScopeKindExporter, ScopeKindInterface},
		"device_cpu_high":             {ScopeKindExporter},
		"heavy_talker":                nil,
		"top_talker_baseline_anomaly": nil,
	}
	for id, want := range cases {
		got := ScopeKindsFor(id)
		if len(got) != len(want) {
			t.Errorf("ScopeKindsFor(%q) = %v; want %v", id, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ScopeKindsFor(%q)[%d] = %v; want %v", id, i, got[i], want[i])
			}
		}
	}
}
