package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/store"
)

// handlers groups HTTP handler methods that share a ClickHouse
// connection. All methods are pure read paths.
type handlers struct {
	conn driver.Conn
}

// health is a minimal liveness probe used by Kubernetes / Container
// Apps. It does not query ClickHouse — readiness via /api/summary is
// the right probe for "can serve traffic".
func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "flowscope-api",
		"time":    time.Now().UTC(),
	})
}

// summary returns the Overview-tab aggregates over a trailing window.
//
//	GET /api/summary?window=300s
//
// Window defaults to 5 minutes; values from "1s" to "168h" are accepted.
func (h *handlers) summary(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	s, err := store.QuerySummary(r.Context(), h.conn, window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// recentFlows returns the most recent flows, newest first.
//
//	GET /api/flows/recent?limit=100
//
// Limit defaults to 100, max 1000.
func (h *handlers) recentFlows(w http.ResponseWriter, r *http.Request) {
	limit := parseInt(r.URL.Query().Get("limit"), 100)
	flows, err := store.QueryRecentFlows(r.Context(), h.conn, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(flows),
		"flows": flows,
	})
}

// parseWindow parses a Go duration string with a default. Bounded to
// [1s, 168h] to keep the SQL bounded.
func parseWindow(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	if d < time.Second {
		return time.Second
	}
	if d > 168*time.Hour {
		return 168 * time.Hour
	}
	return d
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
