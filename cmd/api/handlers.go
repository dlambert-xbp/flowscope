package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/go-chi/chi/v5"

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
//	GET /api/flows/recent?limit=100&exporter=10.2.0.11
//
// Limit defaults to 100, max 1000. exporter is optional.
func (h *handlers) recentFlows(w http.ResponseWriter, r *http.Request) {
	limit := parseInt(r.URL.Query().Get("limit"), 100)
	exporter := r.URL.Query().Get("exporter")
	flows, err := store.QueryRecentFlows(r.Context(), h.conn, limit, exporter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(flows),
		"flows": flows,
	})
}

// parseFilter pulls the supported filter query parameters off a
// request and returns a store.FlowFilter. Unknown params are ignored;
// invalid IPs and ports are also ignored (silently dropped) so a
// half-typed URL doesn't break the dashboard. The store.buildWhere
// helper validates IPs again at query time and reports parse errors.
func parseFilter(r *http.Request) store.FlowFilter {
	q := r.URL.Query()
	return store.FlowFilter{
		Exporter: q.Get("exporter"),
		SrcAddr:  q.Get("src_addr"),
		DstAddr:  q.Get("dst_addr"),
		SrcPort:  parseUint16(q.Get("src_port")),
		DstPort:  parseUint16(q.Get("dst_port")),
		Proto:    parseUint16(q.Get("proto")),
	}
}

func parseUint16(s string) uint16 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// topTalkers / topServices / topProtocols / topConversations all
// share window=, limit= (where applicable), and the filter query
// parameters parsed by parseFilter. Source label = "flows".
func (h *handlers) topTalkers(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	rows, err := store.QueryTopTalkers(r.Context(), h.conn, window, limit, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"window": window.String(),
	})
}

func (h *handlers) topServices(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	rows, err := store.QueryTopServices(r.Context(), h.conn, window, limit, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"window": window.String(),
	})
}

func (h *handlers) topProtocols(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	rows, err := store.QueryTopProtocols(r.Context(), h.conn, window, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"window": window.String(),
	})
}

func (h *handlers) topConversations(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	rows, err := store.QueryTopConversations(r.Context(), h.conn, window, limit, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"window": window.String(),
	})
}

// alerts lists alerts in the supplied state bucket.
//
//	GET /api/alerts?state=open|acknowledged|closed
//
// state defaults to "open+acknowledged" combined when omitted.
func (h *handlers) alerts(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	rows, err := store.QueryAlerts(r.Context(), h.conn, state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"alerts": rows,
		"state":  state,
	})
}

// alertSummary returns the four-bucket counts for the Alerts tab
// summary stats.
//
//	GET /api/alerts/summary
func (h *handlers) alertSummary(w http.ResponseWriter, r *http.Request) {
	s, err := store.QueryAlertSummary(r.Context(), h.conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// ackAlert marks one alert as acknowledged.
//
//	POST /api/alerts/{id}/ack
func (h *handlers) ackAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := actorFromRequest(r)
	if err := store.AckAlert(r.Context(), h.conn, id, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "alert not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "state": "acknowledged"})
}

// closeAlert closes one alert manually.
//
//	POST /api/alerts/{id}/close
func (h *handlers) closeAlert(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actor := actorFromRequest(r)
	if err := store.CloseAlert(r.Context(), h.conn, id, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "alert not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "state": "closed"})
}

// actorFromRequest pulls the operator identity. With OIDC the actor
// comes from the JWT subject claim; until that ships we accept an
// X-Operator header and fall back to "anonymous".
func actorFromRequest(r *http.Request) string {
	if v := r.Header.Get("X-Operator"); v != "" {
		return v
	}
	return "anonymous"
}

// devices lists every exporter that produced at least one flow in
// the trailing window.
//
//	GET /api/devices?window=300s
func (h *handlers) devices(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	rows, err := store.QueryDevices(r.Context(), h.conn, window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(rows),
		"devices": rows,
		"window":  window.String(),
	})
}

// device returns the summary for a single exporter.
//
//	GET /api/devices/{exporter}?window=300s
func (h *handlers) device(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	d, err := store.QueryDevice(r.Context(), h.conn, exporter, window)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no flows from this exporter in the requested window")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// interfaces returns one row per (exporter, ifindex) seen in the
// trailing window, ranked by peak bandwidth.
//
//	GET /api/interfaces?window=300s&exporter=10.2.0.11
func (h *handlers) interfaces(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r.URL.Query().Get("window"), 5*time.Minute)
	exporter := r.URL.Query().Get("exporter")
	rows, err := store.QueryInterfaces(r.Context(), h.conn, window, exporter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      len(rows),
		"interfaces": rows,
		"source":     "counters",
		"window":     window.String(),
	})
}

// interfaceTimeseries returns bytes/sec rates derived from successive
// counter samples. Per VISION.md §3.3, this is the AUTHORITATIVE rate.
//
//	GET /api/interfaces/{exporter}/{ifindex}/timeseries?seconds=300
func (h *handlers) interfaceTimeseries(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	ifindexStr := chi.URLParam(r, "ifindex")

	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	ifindex64, err := strconv.ParseUint(ifindexStr, 10, 32)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ifindex")
		return
	}

	seconds := parseInt(r.URL.Query().Get("seconds"), 300)
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 24*60*60 {
		seconds = 24 * 60 * 60
	}
	window := time.Duration(seconds) * time.Second

	ts, err := store.QueryInterfaceTimeseries(r.Context(), h.conn, exporter, uint32(ifindex64), window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ts)
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
