package alerteng

import (
	"strings"
	"testing"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

func TestBuildBGPDownViolation_eBGPLabel(t *testing.T) {
	v := buildBGPDownViolation(
		"10.1.1.1", "default", "192.0.2.1",
		65001, 64512,
		"idle", "Transit to AS65001", "bgp4",
	)
	if !strings.Contains(v.Title, "eBGP session") {
		t.Errorf("title should label eBGP for differing ASNs; got %q", v.Title)
	}
	if v.GroupKey != "bgp_10.1.1.1_default_192.0.2.1" {
		t.Errorf("GroupKey = %q; want bgp_<exporter>_<vrf>_<peer>", v.GroupKey)
	}
	if v.Labels["state"] != "idle" {
		t.Errorf("Labels[state] = %q; want idle", v.Labels["state"])
	}
	if !strings.Contains(v.Title, "Transit to AS65001") {
		t.Errorf("title should embed peer description; got %q", v.Title)
	}
	if strings.Contains(v.Title, "[vrf default]") {
		t.Errorf("title should not mention VRF for the default routing instance; got %q", v.Title)
	}
}

func TestBuildBGPDownViolation_iBGPLabel(t *testing.T) {
	v := buildBGPDownViolation(
		"10.1.1.1", "default", "10.1.1.2",
		64512, 64512,
		"active", "", "bgp4",
	)
	if !strings.Contains(v.Title, "iBGP session") {
		t.Errorf("title should label iBGP for matching ASNs; got %q", v.Title)
	}
}

func TestBuildBGPDownViolation_NonDefaultVRFInTitle(t *testing.T) {
	v := buildBGPDownViolation(
		"10.1.1.1", "CUSTOMER-A", "10.200.0.1",
		65100, 64512,
		"idle", "L3VPN PE-CE", "cbgp",
	)
	if !strings.Contains(v.Title, "[vrf CUSTOMER-A]") {
		t.Errorf("title should mention non-default VRF; got %q", v.Title)
	}
	if v.GroupKey != "bgp_10.1.1.1_CUSTOMER-A_10.200.0.1" {
		t.Errorf("GroupKey = %q; want vrf-namespaced", v.GroupKey)
	}
	if v.Labels["vrf"] != "CUSTOMER-A" {
		t.Errorf("Labels[vrf] = %q; want CUSTOMER-A", v.Labels["vrf"])
	}
}

func TestBuildVRFWhere(t *testing.T) {
	frag, args := buildVRFWhere("vrf", settings.ScopeSelector{VRFs: []string{"default", "CUSTOMER-A"}})
	if !strings.Contains(frag, "vrf IN (?,?)") {
		t.Errorf("frag = %q", frag)
	}
	if len(args) != 2 {
		t.Errorf("args len = %d; want 2", len(args))
	}
}

func TestBuildVRFWhere_Empty(t *testing.T) {
	frag, args := buildVRFWhere("vrf", settings.ScopeSelector{})
	if frag != "" {
		t.Errorf("frag = %q; want empty", frag)
	}
	if len(args) != 0 {
		t.Errorf("args = %v; want empty", args)
	}
}

func TestBuildBGPPeerWhere(t *testing.T) {
	frag, args := buildBGPPeerWhere("peer_addr", settings.ScopeSelector{
		BGPPeers: []string{"192.0.2.1", "192.0.2.2"},
	})
	if !strings.Contains(frag, "IPv6NumToString(peer_addr) IN (?,?)") {
		t.Errorf("frag = %q", frag)
	}
	if len(args) != 2 {
		t.Errorf("args len = %d; want 2", len(args))
	}
}

func TestBuildBGPPeerWhere_Empty(t *testing.T) {
	frag, args := buildBGPPeerWhere("peer_addr", settings.ScopeSelector{})
	if frag != "" {
		t.Errorf("frag = %q; want empty", frag)
	}
	if len(args) != 0 {
		t.Errorf("args = %v; want empty", args)
	}
}

func TestBuildASNHaving(t *testing.T) {
	frag, args := buildASNHaving(settings.ScopeSelector{ASNRemote: []uint32{65001, 65002}})
	if !strings.Contains(frag, "argMax(peer_asn, polled_at) IN (?,?)") {
		t.Errorf("frag = %q", frag)
	}
	if len(args) != 2 {
		t.Errorf("args len = %d", len(args))
	}
}

func TestValidateScope_AcceptsBGPScope(t *testing.T) {
	err := ValidateScope("bgp_neighbor_down", settings.ScopeSelector{
		Exporters: []string{"10.1.1.1"},
		BGPPeers:  []string{"192.0.2.1"},
		ASNRemote: []uint32{65001},
	})
	if err != nil {
		t.Errorf("ValidateScope rejected valid BGP scope: %v", err)
	}
}

func TestValidateScope_RejectsBGPOnNonBGPTemplate(t *testing.T) {
	err := ValidateScope("device_cpu_high", settings.ScopeSelector{
		BGPPeers: []string{"192.0.2.1"},
	})
	if err == nil {
		t.Error("ValidateScope should reject BGP scope on non-BGP template")
	}
}

func TestBGPNeighborDown_DefaultParams(t *testing.T) {
	r := BGPNeighborDown{EstablishedMinSeconds: 60, LookbackSeconds: 3600}
	p := r.DefaultParams()
	if p["established_min_seconds"] != 60 {
		t.Errorf("default established_min_seconds = %v; want 60", p["established_min_seconds"])
	}
	if p["lookback_seconds"] != 3600 {
		t.Errorf("default lookback_seconds = %v; want 3600", p["lookback_seconds"])
	}
	if r.DefaultSeverity() != SeverityCritical {
		t.Errorf("default severity = %q; want critical", r.DefaultSeverity())
	}
}

func TestBGPNeighborDown_RegisteredInBuiltinTemplates(t *testing.T) {
	registry := BuildTemplateRegistry()
	tpl, ok := registry["bgp_neighbor_down"]
	if !ok {
		t.Fatal("bgp_neighbor_down missing from registry")
	}
	if tpl.ID() != "bgp_neighbor_down" {
		t.Errorf("template ID = %q; want bgp_neighbor_down", tpl.ID())
	}
}

func TestScopeKindsFor_BGP(t *testing.T) {
	kinds := ScopeKindsFor("bgp_neighbor_down")
	want := map[ScopeKind]bool{ScopeKindExporter: true, ScopeKindBGPPeer: true}
	if len(kinds) != len(want) {
		t.Fatalf("scope kinds = %v; want %v", kinds, want)
	}
	for _, k := range kinds {
		if !want[k] {
			t.Errorf("unexpected scope kind %q", k)
		}
	}
}
