package services

import (
	"strings"
	"sync"
	"testing"
)

func TestLookupVXLAN(t *testing.T) {
	r := Lookup("udp", 4789)
	if !r.Found {
		t.Fatalf("vxlan udp/4789 not found")
	}
	if !strings.EqualFold(r.Primary.Name, "vxlan") {
		t.Errorf("vxlan primary = %q, want vxlan", r.Primary.Name)
	}
}

func TestLookupHTTPS(t *testing.T) {
	r := Lookup("tcp", 443)
	if !r.Found || !strings.EqualFold(r.Primary.Name, "https") {
		t.Errorf("https tcp/443 = %+v, want primary=https", r)
	}
}

func TestLookupSSH(t *testing.T) {
	r := Lookup("tcp", 22)
	if !r.Found || !strings.EqualFold(r.Primary.Name, "ssh") {
		t.Errorf("ssh tcp/22 = %+v", r)
	}
}

func TestLookupCaseInsensitiveProto(t *testing.T) {
	a := Lookup("TCP", 443)
	b := Lookup("tcp", 443)
	if a.Primary.Name != b.Primary.Name {
		t.Errorf("case-insensitive proto mismatch: %q vs %q", a.Primary.Name, b.Primary.Name)
	}
}

func TestLookupInvalidProto(t *testing.T) {
	r := Lookup("icmp", 80)
	if r.Found {
		t.Errorf("icmp lookup should not find anything, got %+v", r)
	}
}

func TestLookupUnknownPort(t *testing.T) {
	r := Lookup("tcp", 65530)
	// Unknown is fine — Found should be false rather than crashing.
	if r.Found && r.Primary.Name == "" {
		t.Errorf("found = true but Primary.Name empty: %+v", r)
	}
}

func TestBuiltInCount(t *testing.T) {
	n := BuiltInCount()
	// nmap-services + IANA together should comfortably exceed
	// 5,000 distinct entries. Sanity-check, not a tight bound.
	if n < 5000 {
		t.Errorf("BuiltInCount = %d, expected > 5000", n)
	}
}

func TestResolverCustomOverridesBuiltIn(t *testing.T) {
	r := NewResolver()
	r.SetCustoms([]CustomEntry{
		{Proto: "tcp", PortLo: 443, PortHi: 443, Name: "internal-https", Group: "DC-internal"},
	})
	res := r.Resolve("tcp", 443)
	if !res.Found {
		t.Fatalf("expected resolved entry, got nothing")
	}
	if res.Primary.Name != "internal-https" {
		t.Errorf("primary = %q, want internal-https (custom must outrank built-in)", res.Primary.Name)
	}
	if res.Primary.Source != SourceCustom {
		t.Errorf("primary source = %q, want custom", res.Primary.Source)
	}
	if res.Primary.Group != "DC-internal" {
		t.Errorf("primary group = %q, want DC-internal", res.Primary.Group)
	}
	// The previously-primary built-in must appear as an alternative.
	if len(res.Alternatives) == 0 || res.Alternatives[0].Name != "https" {
		t.Errorf("built-in https should remain as an alternative, got %+v", res.Alternatives)
	}
}

func TestResolverNarrowestRangeWins(t *testing.T) {
	r := NewResolver()
	r.SetCustoms([]CustomEntry{
		{Proto: "tcp", PortLo: 30000, PortHi: 32767, Name: "k8s-NodePort"},
		{Proto: "tcp", PortLo: 30100, PortHi: 30100, Name: "exact-match"},
	})
	if got := r.Resolve("tcp", 30100).Primary.Name; got != "exact-match" {
		t.Errorf("narrowest-range custom must win: got %q, want exact-match", got)
	}
	if got := r.Resolve("tcp", 30200).Primary.Name; got != "k8s-NodePort" {
		t.Errorf("wider-range custom should still apply: got %q", got)
	}
}

func TestResolverFallsBackToBuiltIn(t *testing.T) {
	r := NewResolver()
	r.SetCustoms([]CustomEntry{
		{Proto: "tcp", PortLo: 8080, PortHi: 8080, Name: "internal-app"},
	})
	res := r.Resolve("tcp", 443)
	if res.Primary.Source == SourceCustom {
		t.Errorf("443 should fall back to built-in, got custom")
	}
	if !strings.EqualFold(res.Primary.Name, "https") {
		t.Errorf("expected https fallback, got %q", res.Primary.Name)
	}
}

// SetCustoms must be safe to call concurrently with Resolve. The race
// detector will catch any unsynchronised access in CI; this test
// ensures the path is actually exercised under contention.
func TestResolverConcurrent(t *testing.T) {
	r := NewResolver()
	r.SetCustoms([]CustomEntry{
		{Proto: "udp", PortLo: 4789, PortHi: 4789, Name: "my-vxlan"},
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = r.Resolve("udp", 4789)
					_ = r.Resolve("tcp", 443)
				}
			}
		}()
	}

	for i := 0; i < 50; i++ {
		r.SetCustoms([]CustomEntry{
			{Proto: "udp", PortLo: 4789, PortHi: 4789, Name: "vxlan-rev"},
		})
	}
	close(stop)
	wg.Wait()
}
