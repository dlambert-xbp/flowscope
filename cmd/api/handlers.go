package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/go-chi/chi/v5"

	"github.com/dlambert-xbp/flowscope/internal/snmpx"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

// handlers groups HTTP handler methods that share a ClickHouse
// connection. The optional creds store powers the Settings → SNMP
// admin endpoints; when nil those endpoints return 503.
type handlers struct {
	conn  driver.Conn
	creds snmpx.CredentialStore
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

// listCredentials returns every configured SNMP binding with all
// secrets REDACTED. The has_community / has_auth_pass / has_priv_pass
// booleans tell the UI whether a secret is present without leaking
// the value.
//
//	GET /api/snmp/credentials
//
// 503 when FLOWSCOPE_SNMP_KEY is unset (no credential store is
// available). The Settings UI surfaces that as a banner.
func (h *handlers) listCredentials(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled (FLOWSCOPE_SNMP_KEY not set)")
		return
	}
	rows, err := h.creds.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":       len(rows),
		"credentials": rows,
	})
}

// getCredential returns one binding with secrets REDACTED. The api
// never serves decrypted passphrases — the only consumer of plaintext
// is the snmp scheduler running in the same trust domain.
//
//	GET /api/snmp/credentials/{exporter}
func (h *handlers) getCredential(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	c, err := h.creds.Get(r.Context(), exporter)
	if err != nil {
		if errors.Is(err, snmpx.ErrCredNotFound) {
			writeError(w, http.StatusNotFound, "no credential configured")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Redact secrets before returning.
	c.Community = ""
	c.V3AuthPass = ""
	c.V3PrivPass = ""
	writeJSON(w, http.StatusOK, c)
}

// putCredential creates or replaces the binding for {exporter}.
//
//	PUT /api/snmp/credentials/{exporter}
//	Content-Type: application/json
//	{ "version": "v3", "v3_username": "noc-ro", ... }
//
// Empty passphrase fields on PUT mean "leave the existing secret
// alone". This lets the UI render a "secret already set" indicator
// without forcing the operator to retype every time they tweak a
// non-secret field like interval or context.
func (h *handlers) putCredential(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	var body snmpx.Credential
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if _, err := netip.ParseAddr(exporter); err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	body.Exporter = exporter

	// Preserve existing secrets when the operator left them blank.
	if body.Community == "" || body.V3AuthPass == "" || body.V3PrivPass == "" {
		if existing, err := h.creds.Get(r.Context(), exporter); err == nil {
			if body.Community == "" {
				body.Community = existing.Community
			}
			if body.V3AuthPass == "" {
				body.V3AuthPass = existing.V3AuthPass
			}
			if body.V3PrivPass == "" {
				body.V3PrivPass = existing.V3PrivPass
			}
		}
	}

	if err := h.creds.Set(r.Context(), body, actorFromRequest(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exporter": exporter})
}

// deleteCredential removes the binding. Subsequent walks fall back
// to the cluster-wide community / mock client.
//
//	DELETE /api/snmp/credentials/{exporter}
func (h *handlers) deleteCredential(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	if err := h.creds.Delete(r.Context(), exporter); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exporter": exporter})
}

// testCredential performs an ad-hoc walk of just sysDescr against
// the configured binding and returns the result. Cheap, fast, and
// confirms credentials work without the operator waiting for the
// next scheduler tick.
//
//	POST /api/snmp/credentials/{exporter}/test
func (h *handlers) testCredential(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	cred, err := h.creds.Get(r.Context(), exporter)
	if err != nil {
		if errors.Is(err, snmpx.ErrCredNotFound) {
			writeError(w, http.StatusNotFound, "no credential configured")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	client := snmpx.NewClient(snmpx.FromCredential(cred))
	walkCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	inv, err := client.Walk(walkCtx, exporter)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"sys_descr":       inv.SysDescr,
		"sys_name":        inv.SysName,
		"interfaces":      len(inv.Interfaces),
		"poll_duration_ms": inv.PollDurationMs,
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

// deviceInventory returns the latest SNMP snapshot for one exporter.
//
//	GET /api/devices/{exporter}/inventory
//
// 404 when SNMP has never walked this device. The Devices tab
// surfaces that state as "no SNMP data yet".
func (h *handlers) deviceInventory(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	inv, err := store.QueryDeviceInventory(r.Context(), h.conn, exporter)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no SNMP inventory yet for this exporter")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inv)
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
