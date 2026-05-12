package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestTopologyRouting locks in the route surface added in this PR.
// Both /api/topology and /api/devices/{exporter}/neighbors must
// resolve to their handlers and not collide with the existing device
// detail / inventory / resources routes.
func TestTopologyRouting(t *testing.T) {
	r := chi.NewRouter()

	hits := map[string]bool{}
	mark := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			hits[name] = true
			w.WriteHeader(http.StatusOK)
		}
	}
	r.Get("/api/devices/{exporter}", mark("device"))
	r.Get("/api/devices/{exporter}/inventory", mark("inventory"))
	r.Get("/api/devices/{exporter}/resources", mark("resources"))
	r.Get("/api/devices/{exporter}/neighbors", mark("neighbors"))
	r.Get("/api/topology", mark("topology"))

	cases := []struct {
		name, method, path, want string
	}{
		{"topology root", "GET", "/api/topology", "topology"},
		{"device detail", "GET", "/api/devices/10.0.0.1", "device"},
		{"device inventory", "GET", "/api/devices/10.0.0.1/inventory", "inventory"},
		{"device resources", "GET", "/api/devices/10.0.0.1/resources", "resources"},
		{"device neighbors", "GET", "/api/devices/10.0.0.1/neighbors", "neighbors"},
		{"ipv6 device neighbors", "GET", "/api/devices/2001:db8::1/neighbors", "neighbors"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k := range hits {
				delete(hits, k)
			}
			req := httptest.NewRequest(c.method, c.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; path=%s", rec.Code, c.path)
			}
			if !hits[c.want] {
				t.Fatalf("handler %q not hit; hits=%v", c.want, hits)
			}
		})
	}
}

// TestDeviceNeighborsBadExporter verifies the handler validates the
// exporter path param before reaching the store layer. A bad IP must
// return 400, not bubble up a SQL parse error.
func TestDeviceNeighborsBadExporter(t *testing.T) {
	h := &handlers{}
	r := chi.NewRouter()
	r.Get("/api/devices/{exporter}/neighbors", h.deviceNeighbors)

	req := httptest.NewRequest("GET", "/api/devices/not-an-ip/neighbors", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
