package main

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// fakeAllowlistStore is a minimal AllowlistStore that returns a
// pre-canned slice. Only List is exercised by the gate.
type fakeAllowlistStore struct {
	mu      sync.Mutex
	rows    []settings.ExporterEntry
	listErr error
	calls   int
}

func (f *fakeAllowlistStore) setRows(r []settings.ExporterEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows[:0], r...)
}

func (f *fakeAllowlistStore) List(ctx context.Context) ([]settings.ExporterEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]settings.ExporterEntry, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// The remaining AllowlistStore methods are unused by the gate; they
// exist only to satisfy the interface.
func (f *fakeAllowlistStore) Get(context.Context, string) (*settings.ExporterEntry, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeAllowlistStore) Upsert(context.Context, settings.ExporterEntry, string) error {
	return errors.New("not implemented")
}
func (f *fakeAllowlistStore) Delete(context.Context, string, string) error {
	return errors.New("not implemented")
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a.Unmap()
}

// TestAllowlistGate_DefaultOpen confirms the gate accepts every
// source before any refresh has happened. This protects against
// dropping packets during the priming round-trip.
func TestAllowlistGate_DefaultOpen(t *testing.T) {
	g := newAllowlistGate()
	if !g.Allow(mustAddr(t, "10.0.0.1")) {
		t.Fatal("default-constructed gate must accept every source")
	}
	if !g.Allow(mustAddr(t, "2001:db8::1")) {
		t.Fatal("default-constructed gate must accept IPv6 sources")
	}
}

// TestAllowlistGate_RefreshAndAllow is the table-driven case the
// task brief calls for: empty / non-empty / disabled.
func TestAllowlistGate_RefreshAndAllow(t *testing.T) {
	cases := []struct {
		name string
		rows []settings.ExporterEntry
		// probes is "addr → allow?" expectations after refresh.
		probes map[string]bool
	}{
		{
			name: "empty table accepts all",
			rows: nil,
			probes: map[string]bool{
				"10.0.0.1":    true,
				"192.0.2.7":   true,
				"2001:db8::1": true,
			},
		},
		{
			name: "non-empty enabled accepts listed, drops others",
			rows: []settings.ExporterEntry{
				{Exporter: "10.0.0.1", Enabled: true},
				{Exporter: "10.0.0.2", Enabled: true},
			},
			probes: map[string]bool{
				"10.0.0.1": true,
				"10.0.0.2": true,
				"10.0.0.3": false, // not listed
				"127.0.0.1": false,
			},
		},
		{
			name: "disabled row drops even if listed",
			rows: []settings.ExporterEntry{
				{Exporter: "10.0.0.1", Enabled: true},
				{Exporter: "10.0.0.2", Enabled: false}, // muted
			},
			probes: map[string]bool{
				"10.0.0.1": true,
				"10.0.0.2": false, // disabled → drop
				"10.0.0.3": false, // not listed → drop
			},
		},
		{
			name: "ipv6 entry is matched canonically",
			rows: []settings.ExporterEntry{
				{Exporter: "2001:db8::1", Enabled: true},
			},
			probes: map[string]bool{
				"2001:db8::1": true,
				"2001:db8::2": false,
				"10.0.0.1":    false,
			},
		},
		{
			name: "malformed exporter row is skipped without poisoning the gate",
			rows: []settings.ExporterEntry{
				{Exporter: "not-an-ip", Enabled: true},
				{Exporter: "10.0.0.1", Enabled: true},
			},
			probes: map[string]bool{
				"10.0.0.1": true,
				"10.0.0.2": false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAllowlistStore{}
			store.setRows(tc.rows)
			g := newAllowlistGate()
			if err := g.refreshOnce(context.Background(), store); err != nil {
				t.Fatalf("refreshOnce: %v", err)
			}
			for addr, want := range tc.probes {
				got := g.Allow(mustAddr(t, addr))
				if got != want {
					t.Errorf("Allow(%s) = %v, want %v", addr, got, want)
				}
			}
		})
	}
}

// TestAllowlistGate_RefreshErrorKeepsPreviousMap ensures a transient
// ClickHouse failure on a refresh does not flip the gate back to
// permissive — losing one tick is strictly safer than going open.
func TestAllowlistGate_RefreshErrorKeepsPreviousMap(t *testing.T) {
	store := &fakeAllowlistStore{}
	store.setRows([]settings.ExporterEntry{
		{Exporter: "10.0.0.1", Enabled: true},
	})
	g := newAllowlistGate()
	if err := g.refreshOnce(context.Background(), store); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if !g.Allow(mustAddr(t, "10.0.0.1")) {
		t.Fatal("after first refresh, listed exporter must be allowed")
	}
	if g.Allow(mustAddr(t, "10.0.0.2")) {
		t.Fatal("after first refresh, unlisted exporter must be dropped")
	}

	// Now simulate a transient error: the next List call fails.
	store.mu.Lock()
	store.listErr = errors.New("clickhouse: connection reset")
	store.mu.Unlock()
	if err := g.refreshOnce(context.Background(), store); err == nil {
		t.Fatal("expected refreshOnce to surface the store error")
	}
	// The map must still reflect the pre-failure state.
	if !g.Allow(mustAddr(t, "10.0.0.1")) {
		t.Fatal("after failed refresh, previously-allowed exporter must still be allowed")
	}
	if g.Allow(mustAddr(t, "10.0.0.2")) {
		t.Fatal("after failed refresh, previously-dropped exporter must still be dropped")
	}
}

// TestAllowlistGate_TransitionFromEmptyToDeny confirms the security
// cutover: zero rows = accept-all, then adding the first enabled row
// flips to deny-by-default.
func TestAllowlistGate_TransitionFromEmptyToDeny(t *testing.T) {
	store := &fakeAllowlistStore{}
	g := newAllowlistGate()

	// Phase 1: empty table.
	if err := g.refreshOnce(context.Background(), store); err != nil {
		t.Fatalf("refresh empty: %v", err)
	}
	if !g.Allow(mustAddr(t, "10.0.0.99")) {
		t.Fatal("empty allowlist must accept every source (current-behavior preservation)")
	}

	// Phase 2: operator adds the first row.
	store.setRows([]settings.ExporterEntry{
		{Exporter: "10.0.0.1", Enabled: true},
	})
	if err := g.refreshOnce(context.Background(), store); err != nil {
		t.Fatalf("refresh non-empty: %v", err)
	}
	if !g.Allow(mustAddr(t, "10.0.0.1")) {
		t.Fatal("listed exporter must still be allowed")
	}
	if g.Allow(mustAddr(t, "10.0.0.99")) {
		t.Fatal("after the first row is added the gate must be deny-by-default")
	}
}
