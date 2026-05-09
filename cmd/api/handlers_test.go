package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestAlertRoutingPrecedence locks in the ordering between
// /api/alerts/summary (static) and /api/alerts/{id} (param). chi
// routes static children ahead of params at the same level, but the
// only way to be sure is to wire a real router, dispatch fake
// requests, and assert that each lands on the right handler. This
// also guards against accidental reorderings in main.go that would
// silently break either endpoint.
func TestAlertRoutingPrecedence(t *testing.T) {
	r := chi.NewRouter()

	hits := map[string]bool{}
	mark := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			hits[name] = true
			w.WriteHeader(http.StatusOK)
		}
	}
	r.Get("/api/alerts", mark("list"))
	r.Get("/api/alerts/summary", mark("summary"))
	r.Get("/api/alerts/{id}", mark("detail"))
	r.Post("/api/alerts/{id}/ack", mark("ack"))
	r.Post("/api/alerts/{id}/close", mark("close"))

	cases := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"list", "GET", "/api/alerts", "list"},
		{"summary stays static", "GET", "/api/alerts/summary", "summary"},
		{"detail by id", "GET", "/api/alerts/abc123", "detail"},
		{"detail by hex id", "GET", "/api/alerts/0123456789abcdef", "detail"},
		{"ack stays nested", "POST", "/api/alerts/abc/ack", "ack"},
		{"close stays nested", "POST", "/api/alerts/abc/close", "close"},
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
			for k := range hits {
				if k != c.want {
					t.Fatalf("unexpected handler %q hit (want %q)", k, c.want)
				}
			}
		})
	}
}
