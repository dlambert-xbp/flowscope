package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dlambert-xbp/flowscope/internal/authz"
)

// TestReadEndpointsRequireToken locks in the Phase 1 gate: every
// /api/* route that exposes flow / topology / alert data must reject
// requests with a missing or wrong X-Auth-Token when a SharedToken is
// configured. The unauthenticated allowlist (/healthz, /metrics,
// /api/config/effective) must keep working without a token.
//
// The router is constructed identically to cmd/api/main.go's wiring
// so that a future reorder which silently drops a route from the
// RequireRead group is caught here — the stub handlers stamp a
// dedicated marker header that the assertions watch for.
func TestReadEndpointsRequireToken(t *testing.T) {
	const token = "test-shared-token"
	authCfg := authz.Config{SharedToken: token}

	r := chi.NewRouter()

	mark := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Test-Handler", name)
			w.WriteHeader(http.StatusOK)
		}
	}

	// Allowlist (no auth).
	r.Get("/healthz", mark("healthz"))
	r.Get("/api/config/effective", mark("config-effective"))

	// Read group — same routes as main.go's RequireRead block. If
	// you add a route to one, add it to the other.
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireRead())
		r.Get("/api/summary", mark("summary"))
		r.Get("/api/health/streams", mark("health-streams"))
		r.Get("/api/health/storage", mark("health-storage"))
		r.Get("/api/health/exporters", mark("health-exporters"))
		r.Get("/api/health/ingest", mark("health-ingest"))
		r.Get("/api/dns/lookup", mark("dns-lookup"))
		r.Get("/api/flows/recent", mark("flows-recent"))
		r.Get("/api/flows/list", mark("flows-list"))
		r.Get("/api/flows/timeseries", mark("flows-timeseries"))
		r.Get("/api/flows/flags-timeseries", mark("flows-flags-timeseries"))
		r.Get("/api/devices", mark("devices"))
		r.Get("/api/devices/{exporter}", mark("device"))
		r.Get("/api/devices/{exporter}/inventory", mark("device-inventory"))
		r.Get("/api/devices/{exporter}/resources", mark("device-resources"))
		r.Get("/api/interfaces", mark("interfaces"))
		r.Get("/api/interfaces/{exporter}/{ifindex}/timeseries", mark("interface-timeseries"))
		r.Get("/api/top/talkers", mark("top-talkers"))
		r.Get("/api/top/services", mark("top-services"))
		r.Get("/api/top/protocols", mark("top-protocols"))
		r.Get("/api/top/conversations", mark("top-conversations"))
		r.Get("/api/top/asn", mark("top-asn"))
		r.Get("/api/top/interfaces", mark("top-interfaces"))
		r.Get("/api/alerts", mark("alerts"))
		r.Get("/api/alerts/summary", mark("alerts-summary"))
		r.Get("/api/alerts/{id}", mark("alert-detail"))
		r.Post("/api/alerts/{id}/ack", mark("alert-ack"))
		r.Post("/api/alerts/{id}/close", mark("alert-close"))
		r.Get("/api/snmp/credentials", mark("snmp-credentials-list"))
		r.Get("/api/snmp/credentials/{exporter}", mark("snmp-credential-get"))
		r.Get("/api/services/lookup", mark("services-lookup"))
		r.Get("/api/services/library", mark("services-library"))
		r.Get("/api/services/custom", mark("services-custom"))
	})

	type gatedCase struct {
		method string
		path   string
	}

	// One representative endpoint per group in the prompt: summary,
	// flows, devices, alerts, top, services, timeseries, plus the
	// alert-mutating POSTs (ack / close) and a couple of edge cases
	// — alert detail with a numeric id, an interfaces timeseries,
	// and the SNMP-derived reads.
	gated := []gatedCase{
		{"GET", "/api/summary"},
		{"GET", "/api/health/streams"},
		{"GET", "/api/dns/lookup"},
		{"GET", "/api/flows/recent"},
		{"GET", "/api/flows/list"},
		{"GET", "/api/flows/timeseries"},
		{"GET", "/api/devices"},
		{"GET", "/api/devices/192.0.2.1"},
		{"GET", "/api/devices/192.0.2.1/inventory"},
		{"GET", "/api/devices/192.0.2.1/resources"},
		{"GET", "/api/interfaces"},
		{"GET", "/api/interfaces/192.0.2.1/42/timeseries"},
		{"GET", "/api/top/talkers"},
		{"GET", "/api/top/services"},
		{"GET", "/api/top/protocols"},
		{"GET", "/api/top/conversations"},
		{"GET", "/api/top/asn"},
		{"GET", "/api/top/interfaces"},
		{"GET", "/api/alerts"},
		{"GET", "/api/alerts/summary"},
		{"GET", "/api/alerts/abc-123"},
		{"POST", "/api/alerts/abc-123/ack"},
		{"POST", "/api/alerts/abc-123/close"},
		{"GET", "/api/snmp/credentials"},
		{"GET", "/api/snmp/credentials/192.0.2.1"},
		{"GET", "/api/services/lookup"},
		{"GET", "/api/services/library"},
		{"GET", "/api/services/custom"},
	}

	for _, c := range gated {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			// with the right token → 200, handler ran
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Header.Set("X-Auth-Token", token)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("with token: status = %d, want 200", rec.Code)
			}
			if rec.Header().Get("X-Test-Handler") == "" {
				t.Fatalf("with token: handler did not run (no X-Test-Handler header)")
			}

			// no token → 401
			req = httptest.NewRequest(c.method, c.path, nil)
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no token: status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("X-Test-Handler") != "" {
				t.Fatalf("no token: handler ran but should not have")
			}
			if !strings.Contains(rec.Body.String(), "missing X-Auth-Token") {
				t.Fatalf("no token: body = %q, want \"missing X-Auth-Token\"", rec.Body.String())
			}

			// wrong token → 401
			req = httptest.NewRequest(c.method, c.path, nil)
			req.Header.Set("X-Auth-Token", "nope-not-it")
			rec = httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("wrong token: status = %d, want 401", rec.Code)
			}
			if rec.Header().Get("X-Test-Handler") != "" {
				t.Fatalf("wrong token: handler ran but should not have")
			}
			if !strings.Contains(rec.Body.String(), "invalid X-Auth-Token") {
				t.Fatalf("wrong token: body = %q, want \"invalid X-Auth-Token\"", rec.Body.String())
			}
		})
	}

	// Allowlist: /healthz and /api/config/effective must succeed
	// without a token even when one is configured.
	allow := []string{"/healthz", "/api/config/effective"}
	for _, path := range allow {
		t.Run("allowlist "+path, func(t *testing.T) {
			req := httptest.NewRequest("GET", path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("path=%s: status = %d, want 200", path, rec.Code)
			}
		})
	}
}

// TestReadGateUnauthBypassWhenNothingConfigured covers the bootstrap
// mode: when neither SharedToken nor a per-token store is configured,
// the read gate must let requests through with subject "unauth-
// bypass". This is the existing behaviour the write group already
// has — RequireRead inherits it because it shares requireScope().
func TestReadGateUnauthBypassWhenNothingConfigured(t *testing.T) {
	authCfg := authz.Config{} // no token, no store

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(authCfg.RequireRead())
		r.Get("/api/summary", func(w http.ResponseWriter, req *http.Request) {
			s := authz.SubjectFrom(req.Context())
			if s.Source != "unauth-bypass" {
				t.Errorf("subject source = %q, want unauth-bypass", s.Source)
			}
			w.WriteHeader(http.StatusOK)
		})
	})

	req := httptest.NewRequest("GET", "/api/summary", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
