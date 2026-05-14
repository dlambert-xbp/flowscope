package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/alerteng"
	"github.com/dlambert-xbp/flowscope/internal/audit"
	"github.com/dlambert-xbp/flowscope/internal/authz"
	"github.com/dlambert-xbp/flowscope/internal/notifier"
	"github.com/dlambert-xbp/flowscope/internal/services"
	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// settingsDeps groups the collaborators every Settings/Services
// handler reaches for. Embedded into handlers via a separate field
// (h.settings) so the existing fields stay tidy.
type settingsDeps struct {
	store    *settings.Store
	resolver *services.Resolver
	audit    audit.Writer
	reader   audit.Reader
}

// recordAudit writes one audit event using the request context.
// Failures are logged via the structured logger but never fail the
// request — losing an audit row is preferable to losing the
// operator's mutation.
func (h *handlers) recordAudit(r *http.Request, action audit.Action, resource, target string, before, after any) {
	if h.settings.audit == nil {
		return
	}
	subj := authz.SubjectFrom(r.Context())
	actor := subj.Actor
	if actor == "" {
		actor = "anonymous"
	}
	_ = h.settings.audit.Record(r.Context(), audit.Event{
		Actor:     actor,
		Action:    action,
		Resource:  resource,
		Target:    target,
		Before:    before,
		After:     after,
		RequestID: middleware.GetReqID(r.Context()),
		SourceIP:  clientIP(r),
	})
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

func (h *handlers) actor(r *http.Request) string {
	subj := authz.SubjectFrom(r.Context())
	if subj.Actor != "" {
		return subj.Actor
	}
	return "anonymous"
}

/* =============================================================================
   /api/services/lookup
   /api/services/library?proto=tcp&q=https&limit=100
   ============================================================================= */

// servicesLookup resolves a (proto, port) pair through the layered
// resolver (custom > built-in). Used by the UI for chip labels and
// for the live-tail row decoration. Designed to be cheap; the UI
// caches results client-side via TanStack Query.
func (h *handlers) servicesLookup(w http.ResponseWriter, r *http.Request) {
	proto := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("proto")))
	portStr := r.URL.Query().Get("port")
	if proto == "" || portStr == "" {
		writeError(w, http.StatusBadRequest, "proto and port are required")
		return
	}
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid port")
		return
	}
	res := h.settings.resolver.Resolve(proto, uint16(port64))
	writeJSON(w, http.StatusOK, res)
}

// servicesLibrary returns a paginated, filterable view of the built-in
// dataset. Used by the Services panel's "browse" tab. Custom entries
// are not folded in here — they live on /api/services/custom.
func (h *handlers) servicesLibrary(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	proto := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("proto")))
	limit := parseInt(r.URL.Query().Get("limit"), 200)
	if limit > 1000 {
		limit = 1000
	}
	offset := parseInt(r.URL.Query().Get("offset"), 0)

	type row struct {
		Name        string         `json:"name"`
		Proto       string         `json:"proto"`
		Port        uint16         `json:"port"`
		Description string         `json:"description,omitempty"`
		Source      services.Source `json:"source"`
		Multi       bool           `json:"multi"`
		Frequency   float64        `json:"frequency,omitempty"`
	}

	var rows []row
	// Walk the well-known port space. We filter aggressively so a
	// query like "?q=vxlan&proto=udp" returns fast even though the
	// underlying dataset is ~22k rows.
	for port := 1; port <= 65535; port++ {
		for _, p := range []string{"tcp", "udp", "sctp", "dccp"} {
			if proto != "" && proto != p {
				continue
			}
			res := services.Lookup(p, uint16(port))
			if !res.Found {
				continue
			}
			match := func(name string) bool { return q == "" || strings.Contains(strings.ToLower(name), q) }
			if !match(res.Primary.Name) {
				skip := true
				for _, alt := range res.Alternatives {
					if match(alt.Name) {
						skip = false
						break
					}
				}
				if skip {
					continue
				}
			}
			rows = append(rows, row{
				Name:        res.Primary.Name,
				Proto:       res.Primary.Proto,
				Port:        res.Primary.Port,
				Description: res.Primary.Description,
				Source:      res.Primary.Source,
				Multi:       res.Multi,
				Frequency:   res.Primary.Frequency,
			})
		}
	}

	total := len(rows)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"rows":    rows[offset:end],
		"counts":  map[string]int{"built_in": services.BuiltInCount()},
	})
}

/* =============================================================================
   /api/services/custom (list, create, update, delete) + cache refresh
   ============================================================================= */

func (h *handlers) listCustomServices(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	rows, err := h.settings.store.CustomServices.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(rows),
		"rows":  rows,
	})
}

func (h *handlers) putCustomService(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var c settings.CustomService
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if id := chi.URLParam(r, "id"); id != "" {
		// Path-bound id wins — protects against client/server drift.
		if uid, err := uuid.Parse(id); err == nil {
			c.ID = uid
		}
	}
	var before *settings.CustomService
	if c.ID != uuid.Nil {
		if cur, err := h.settings.store.CustomServices.Get(r.Context(), c.ID.String()); err == nil {
			before = cur
		}
	}
	saved, err := h.settings.store.CustomServices.Upsert(r.Context(), c, h.actor(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := audit.ActionCreate
	if before != nil {
		action = audit.ActionUpdate
	}
	h.recordAudit(r, action, audit.ResourceCustomService, saved.ID.String(), before, saved)
	h.refreshResolver(r.Context())
	writeJSON(w, http.StatusOK, saved)
}

func (h *handlers) deleteCustomService(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	before, _ := h.settings.store.CustomServices.Get(r.Context(), id)
	if err := h.settings.store.CustomServices.Delete(r.Context(), id, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionDelete, audit.ResourceCustomService, id, before, nil)
	h.refreshResolver(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// refreshResolver re-reads the custom services table and pushes the
// snapshot into the in-process Resolver. Cheap operation (one
// SELECT FINAL) and it ensures lookups served by /api/services/lookup
// reflect the most-recent edit immediately. The resolver is also
// refreshed periodically by main() in case multiple api replicas
// share the table.
func (h *handlers) refreshResolver(ctx context.Context) {
	if h.settings.store == nil || h.settings.resolver == nil {
		return
	}
	rows, err := h.settings.store.CustomServices.List(ctx)
	if err != nil {
		return
	}
	cs := make([]services.CustomEntry, 0, len(rows))
	for _, r := range rows {
		cs = append(cs, services.CustomEntry{
			Proto:       r.Proto,
			PortLo:      r.PortLo,
			PortHi:      r.PortHi,
			Name:        r.Name,
			Description: r.Description,
			Group:       r.Group,
			Owner:       r.Owner,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	h.settings.resolver.SetCustoms(cs)
}

/* =============================================================================
   /api/settings/general (KV)
   ============================================================================= */

func (h *handlers) listGeneralSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	rows, err := h.settings.store.AppSettings.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (h *handlers) putGeneralSetting(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	name := chi.URLParam(r, "name")
	if !validGeneralKey(name) {
		writeError(w, http.StatusBadRequest, "unknown general setting key")
		return
	}
	var body struct {
		Value any `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	before, _ := h.settings.store.AppSettings.Get(r.Context(), name)
	v := settings.AppSettingValue{Name: name, Value: body.Value}
	if err := h.settings.store.AppSettings.Set(r.Context(), v, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionUpdate, audit.ResourceAppSetting, name, before, v)
	writeJSON(w, http.StatusOK, v)
}

// effectiveConfig returns the runtime-effective values for the
// display-side keys in app_settings, merged on top of hard-coded
// defaults. Used by the SPA on boot to seed theme / time range /
// brand display name without each component re-fetching.
//
// Retention values are deliberately omitted — they take effect only
// at init-container time via ALTER TABLE TTL, not at request time,
// and surfacing them here would invite the misimpression that the
// SPA can change them on the fly.
//
//	GET /api/config/effective
func (h *handlers) effectiveConfig(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"display_name":       "FlowScope",
		"default_theme":      "system",
		"default_time_range": "5m",
		"timezone":           "UTC",
	}
	if h.settings.store != nil {
		rows, err := h.settings.store.AppSettings.List(r.Context())
		if err == nil {
			for _, row := range rows {
				switch row.Name {
				case "display_name", "default_theme", "default_time_range", "timezone":
					out[row.Name] = row.Value
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// validGeneralKey closes the set of allowed keys. New keys land here
// in the same PR that makes them — keeping the set explicit means a
// typo in the UI can't silently create a junk row.
func validGeneralKey(name string) bool {
	switch name {
	case "display_name",
		"default_time_range",
		"default_theme",
		"timezone",
		"flow_retention_days",
		"counter_retention_days":
		return true
	}
	return false
}

/* =============================================================================
   /api/settings/exporters/allowlist
   ============================================================================= */

func (h *handlers) listAllowlist(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	rows, err := h.settings.store.Allowlist.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

func (h *handlers) putAllowlist(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var e settings.ExporterEntry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if v := chi.URLParam(r, "exporter"); v != "" {
		e.Exporter = v
	}
	before, _ := h.settings.store.Allowlist.Get(r.Context(), e.Exporter)
	if err := h.settings.store.Allowlist.Upsert(r.Context(), e, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := audit.ActionCreate
	if before != nil {
		action = audit.ActionUpdate
	}
	h.recordAudit(r, action, audit.ResourceExporterAllowlist, e.Exporter, before, e)
	writeJSON(w, http.StatusOK, e)
}

func (h *handlers) deleteAllowlist(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	before, _ := h.settings.store.Allowlist.Get(r.Context(), exporter)
	if err := h.settings.store.Allowlist.Delete(r.Context(), exporter, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionDelete, audit.ResourceExporterAllowlist, exporter, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* =============================================================================
   /api/settings/tokens
   ============================================================================= */

func (h *handlers) listTokens(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	rows, err := h.settings.store.APITokens.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

func (h *handlers) createToken(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var body struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	tok, err := h.settings.store.APITokens.Create(r.Context(), body.Name, body.Scope, h.actor(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Audit AFTER mint so we have an id; redact plaintext from the
	// after-image (never persist plaintext anywhere except the response
	// to the operator that created it).
	redacted := *tok
	redacted.Plaintext = ""
	h.recordAudit(r, audit.ActionCreate, audit.ResourceAPIToken, tok.ID.String(), nil, redacted)
	writeJSON(w, http.StatusCreated, tok)
}

func (h *handlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	before, _ := h.settings.store.APITokens.Get(r.Context(), id)
	if err := h.settings.store.APITokens.Revoke(r.Context(), id, h.actor(r)); err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionDelete, audit.ResourceAPIToken, id, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

/* =============================================================================
   /api/settings/audit (read-only)
   ============================================================================= */

func (h *handlers) listAudit(w http.ResponseWriter, r *http.Request) {
	if h.settings.reader == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	q := audit.ListQuery{
		Resource: r.URL.Query().Get("resource"),
		Actor:    r.URL.Query().Get("actor"),
		Action:   r.URL.Query().Get("action"),
		Limit:    parseInt(r.URL.Query().Get("limit"), 100),
		Offset:   parseInt(r.URL.Query().Get("offset"), 0),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Since = t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.Until = t
		}
	}
	rows, err := h.settings.reader.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

/* =============================================================================
   /api/settings/alert-rules
   ============================================================================= */

func (h *handlers) listAlertRules(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	rows, err := h.settings.store.AlertRules.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	effective, err := alerteng.Effective(r.Context(), h.settings.store.AlertRules)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"count":     len(rows),
		"available": availableRules(),
		"effective": effective,
	})
}

func (h *handlers) putAlertRule(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var s settings.AlertRuleSetting
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if id := chi.URLParam(r, "id"); id != "" {
		s.RuleID = id
	}
	if !knownRule(s.RuleID) {
		writeError(w, http.StatusBadRequest, "unknown rule_id")
		return
	}
	before, _ := h.settings.store.AlertRules.Get(r.Context(), s.RuleID)
	if err := h.settings.store.AlertRules.Upsert(r.Context(), s, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Dual-write: keep the legacy alert_rule_settings row and the
	// per-template seed instance in sync so the existing AlertsTuning
	// UI remains functional during the migration window. Without this
	// the engine (which now reads alert_rule_instances) would ignore
	// edits made via the legacy editor. Dropped together with the
	// legacy table in phase 7.
	if seed, err := h.settings.store.AlertInstances.EnsureSeed(r.Context(),
		s.RuleID, "Default · "+s.RuleID, paramsAsMap(s.Params), s.Severity,
	); err == nil {
		seed.Enabled = s.Enabled
		seed.Severity = s.Severity
		seed.Params = paramsAsMap(s.Params)
		seed.Runbook = s.Runbook
		seed.Channels = s.Channels
		_, _ = h.settings.store.AlertInstances.Update(r.Context(), *seed, h.actor(r))
	}
	action := audit.ActionCreate
	if before != nil {
		action = audit.ActionUpdate
	}
	h.recordAudit(r, action, audit.ResourceAlertRuleSetting, s.RuleID, before, s)
	writeJSON(w, http.StatusOK, s)
}

// paramsAsMap normalizes the Params field of an AlertRuleSetting
// (which can be any JSON shape) to the map[string]any the instance
// store expects. Anything else becomes an empty map.
func paramsAsMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// availableRules describes the Go-coded rules in internal/alerteng so
// the UI can render the editor (label + parameter spec). Static for
// v1; future rules add an entry here as they ship. Keep the entries in
// the same order as DefaultRules() so the Settings panel reads top-to-
// bottom in the order an operator would learn about them: flow-level,
// then interface-level (SNMP), then chassis-level (SNMP).
func availableRules() any {
	return []map[string]any{
		{
			"rule_id":     "exporter_silent",
			"label":       "Exporter silent",
			"description": "Fires when an exporter that recently produced flows stops producing them.",
			"params": []map[string]any{
				{"name": "silent_seconds", "kind": "int", "default": 60, "min": 10, "max": 3600},
				{"name": "active_seconds", "kind": "int", "default": 600, "min": 60, "max": 86400},
			},
			"default_severity": "critical",
		},
		{
			"rule_id":     "heavy_talker",
			"label":       "Heavy talker",
			"description": "Fires when a (src, dst) pair moves more than threshold bytes in the trailing window.",
			"params": []map[string]any{
				{"name": "window_seconds", "kind": "int", "default": 300, "min": 60, "max": 3600},
				{"name": "bytes_threshold", "kind": "int", "default": 1073741824, "min": 1048576, "max": 1099511627776},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "interface_oper_status_change",
			"label":       "Interface oper-status change",
			"description": "Fires when an interface's SNMP if_oper_status transitions between successive polls.",
			"params": []map[string]any{
				{"name": "debounce_seconds", "kind": "int", "default": 60, "min": 10, "max": 3600},
				{"name": "lookback_hours", "kind": "int", "default": 24, "min": 1, "max": 168},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "interface_utilization_high",
			"label":       "Interface utilization high",
			"description": "Fires when an interface's recent throughput exceeds the configured percentage of ifSpeed.",
			"params": []map[string]any{
				{"name": "threshold_pct", "kind": "int", "default": 80, "min": 1, "max": 100},
				{"name": "critical_bump_pct", "kind": "int", "default": 15, "min": 0, "max": 50},
				{"name": "window_seconds", "kind": "int", "default": 300, "min": 60, "max": 3600},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "interface_errors_rate",
			"label":       "Interface errors/discards rate",
			"description": "Fires when combined errors+discards per minute on an interface exceeds the threshold.",
			"params": []map[string]any{
				{"name": "window_seconds", "kind": "int", "default": 300, "min": 60, "max": 3600},
				{"name": "errors_per_min", "kind": "int", "default": 10, "min": 1, "max": 100000},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "top_talker_baseline_anomaly",
			"label":       "Top-talker baseline anomaly",
			"description": "Fires when a (src, dst) pair's last-hour bytes exceed the multiplier of its same-hour-of-day 7-day baseline.",
			"params": []map[string]any{
				{"name": "multiplier", "kind": "float", "default": 3.0, "min": 1.5, "max": 100.0},
				{"name": "min_baseline_bytes", "kind": "int", "default": 1000000000, "min": 1048576, "max": 1099511627776},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "device_cpu_high",
			"label":       "Device CPU high",
			"description": "Fires when an SNMP-polled CPU is at or above the configured percentage.",
			"params": []map[string]any{
				{"name": "threshold_pct", "kind": "int", "default": 80, "min": 1, "max": 100},
				{"name": "critical_bump_pct", "kind": "int", "default": 15, "min": 0, "max": 50},
				{"name": "lookback_seconds", "kind": "int", "default": 1800, "min": 60, "max": 86400},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "device_memory_high",
			"label":       "Device memory high",
			"description": "Fires when an SNMP-polled memory pool exceeds the configured percentage of its capacity.",
			"params": []map[string]any{
				{"name": "threshold_pct", "kind": "int", "default": 85, "min": 1, "max": 100},
				{"name": "critical_bump_pct", "kind": "int", "default": 10, "min": 0, "max": 50},
				{"name": "lookback_seconds", "kind": "int", "default": 1800, "min": 60, "max": 86400},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "device_storage_high",
			"label":       "Device storage high",
			"description": "Fires per-filesystem when used bytes cross the configured percentage of total capacity.",
			"params": []map[string]any{
				{"name": "threshold_pct", "kind": "int", "default": 85, "min": 1, "max": 100},
				{"name": "critical_bump_pct", "kind": "int", "default": 10, "min": 0, "max": 50},
				{"name": "lookback_seconds", "kind": "int", "default": 3600, "min": 60, "max": 86400},
			},
			"default_severity": "warning",
		},
		{
			"rule_id":     "device_unreachable",
			"label":       "Device unreachable",
			"description": "Fires when SNMP polling for a previously-seen device is failing or has gone stale.",
			"params": []map[string]any{
				{"name": "stale_seconds", "kind": "int", "default": 2700, "min": 60, "max": 86400},
				{"name": "lookback_hours", "kind": "int", "default": 24, "min": 1, "max": 168},
			},
			"default_severity": "critical",
		},
	}
}

func knownRule(id string) bool {
	switch id {
	case "exporter_silent", "heavy_talker",
		"interface_oper_status_change", "interface_utilization_high",
		"interface_errors_rate", "top_talker_baseline_anomaly",
		"device_cpu_high", "device_memory_high", "device_storage_high",
		"device_unreachable":
		return true
	}
	return false
}

/* =============================================================================
   /api/settings/integrations/webhooks
   ============================================================================= */

func (h *handlers) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	rows, err := h.settings.store.Webhooks.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

func (h *handlers) putWebhook(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var wh settings.Webhook
	if err := json.NewDecoder(r.Body).Decode(&wh); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if id := chi.URLParam(r, "id"); id != "" {
		if uid, err := uuid.Parse(id); err == nil {
			wh.ID = uid
		}
	}
	var before *settings.Webhook
	if wh.ID != uuid.Nil {
		if cur, err := h.settings.store.Webhooks.Get(r.Context(), wh.ID.String()); err == nil {
			before = cur
		}
	}
	saved, err := h.settings.store.Webhooks.Upsert(r.Context(), wh, h.actor(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := audit.ActionCreate
	if before != nil {
		action = audit.ActionUpdate
	}
	h.recordAudit(r, action, audit.ResourceWebhook, saved.ID.String(), before, saved)
	writeJSON(w, http.StatusOK, saved)
}

func (h *handlers) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	before, _ := h.settings.store.Webhooks.Get(r.Context(), id)
	if err := h.settings.store.Webhooks.Delete(r.Context(), id, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionDelete, audit.ResourceWebhook, id, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// testWebhook fires a synthetic alert through the dispatcher path so
// the operator can verify a freshly-configured endpoint before
// enabling it for real alerts. Requires h.testDispatcher (Dispatcher)
// to have been wired in cmd/api/main.go; returns 503 otherwise so
// the UI can render a clear "test feature unavailable" instead of
// failing opaquely.
//
//	POST /api/settings/integrations/webhooks/{id}/test
func (h *handlers) testWebhook(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	if h.testDispatcher == nil {
		writeError(w, http.StatusServiceUnavailable,
			"webhook tester unavailable (FLOWSCOPE_SNMP_KEY required to decrypt endpoint secrets)")
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := h.settings.store.Webhooks.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The list/get path returns has_secret but not the plaintext; we
	// reach directly into the underlying row to pull secret_ct and
	// decrypt it the same way the dispatcher would for production
	// traffic. Keeping that translation here (vs. in the store)
	// preserves the rule that secrets never leak through the public
	// store API.
	secret, err := h.resolveWebhookSecret(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "decrypt secret: "+err.Error())
		return
	}

	ep := notifier.Endpoint{
		ID:             rec.ID.String(),
		Name:           rec.Name,
		Kind:           rec.Kind,
		URL:            rec.URL,
		Secret:         secret,
		Headers:        rec.HeaderTemplate,
		SeverityFilter: rec.SeverityFilter,
	}
	res, dispErr := h.testDispatcher.SendTest(r.Context(), ep)

	// Audit the test attempt so an operator can later trace "who
	// pinged this endpoint when".
	h.recordAudit(r, audit.ActionUpdate, audit.ResourceWebhook, id, nil, map[string]any{
		"action":      "test",
		"http_status": res.HTTPStatus,
		"ok":          res.OK,
		"error":       res.Error,
	})

	status := http.StatusOK
	if dispErr != nil {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, res)
}

// resolveWebhookSecret reaches into webhook_endpoints to pull and
// decrypt the secret. Kept in cmd/api (not settings.Store) to
// guarantee that the public store API never returns plaintext —
// callers that need plaintext must go through this dedicated path
// and only the test handler does.
func (h *handlers) resolveWebhookSecret(ctx context.Context, id string) (string, error) {
	if h.crypter == nil {
		return "", nil
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return "", err
	}
	const q = `SELECT secret_ct FROM webhook_endpoints FINAL WHERE id = ?`
	var secretCT string
	if err := h.conn.QueryRow(ctx, q, uid).Scan(&secretCT); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", nil
		}
		return "", err
	}
	if secretCT == "" {
		return "", nil
	}
	return h.crypter.Decrypt(secretCT)
}

/* =============================================================================
   /api/settings/oidc
   ============================================================================= */

func (h *handlers) getOIDC(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, settings.OIDCConfig{LoginFlowStatus: settings.LoginFlowStatusPhase2})
		return
	}
	c, err := h.settings.store.OIDC.Get(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *handlers) putOIDC(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var c settings.OIDCConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	before, _ := h.settings.store.OIDC.Get(r.Context())
	if err := h.settings.store.OIDC.Set(r.Context(), c, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c.LoginFlowStatus = settings.LoginFlowStatusPhase2
	c.ClientSecret = ""
	h.recordAudit(r, audit.ActionUpdate, audit.ResourceOIDCConfig, "singleton", before, c)
	writeJSON(w, http.StatusOK, c)
}

/* =============================================================================
   /api/settings/advanced — read-only metadata about tunables
   ============================================================================= */

// AdvancedField describes one tunable for the UI to render. Reload
// is "live" when the owning service re-reads on a refresh tick, or
// "restart" when the value is read once at boot. v1 is honest:
// nothing in the codebase reloads at runtime today, so every field
// is "restart". Adding a live-reloadable field means flipping that
// here and wiring the reload in the service.
type AdvancedField struct {
	Name        string `json:"name"`
	Service     string `json:"service"`
	EnvVar      string `json:"env_var,omitempty"`
	Description string `json:"description"`
	Reload      string `json:"reload"` // 'live' | 'restart'
	DefaultText string `json:"default_text,omitempty"`
}

func (h *handlers) listAdvanced(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"fields": advancedFields()})
}

func advancedFields() []AdvancedField {
	return []AdvancedField{
		{
			Name: "FLOWSCOPE_NETFLOW_PORT", Service: "ingest", EnvVar: "FLOWSCOPE_NETFLOW_PORT",
			Description: "UDP listener for NetFlow v5/v9.", Reload: "restart", DefaultText: "2055",
		},
		{
			Name: "FLOWSCOPE_SFLOW_PORT", Service: "ingest", EnvVar: "FLOWSCOPE_SFLOW_PORT",
			Description: "UDP listener for sFlow v5.", Reload: "restart", DefaultText: "6343",
		},
		{
			Name: "FLOWSCOPE_IPFIX_PORT", Service: "ingest", EnvVar: "FLOWSCOPE_IPFIX_PORT",
			Description: "UDP listener for IPFIX.", Reload: "restart", DefaultText: "4739",
		},
		{
			Name: "FLOWSCOPE_HTTP_ADDR", Service: "api", EnvVar: "FLOWSCOPE_HTTP_ADDR",
			Description: "Listen address for the api service.", Reload: "restart", DefaultText: ":8080",
		},
		{
			Name: "FLOWSCOPE_LOG_LEVEL", Service: "all", EnvVar: "FLOWSCOPE_LOG_LEVEL",
			Description: "Structured log level: debug | info | warn | error.", Reload: "restart", DefaultText: "info",
		},
		{
			Name: "FLOWSCOPE_SNMP_WORKERS", Service: "snmp", EnvVar: "FLOWSCOPE_SNMP_WORKERS",
			Description: "Worker pool size for SNMP walks.", Reload: "restart", DefaultText: "8",
		},
		{
			Name: "FLOWSCOPE_SNMP_KEY", Service: "snmp + api", EnvVar: "FLOWSCOPE_SNMP_KEY",
			Description: "Master key for v3 credential sealing. MUST stay constant — rotating invalidates every stored credential.",
			Reload:      "restart",
		},
		{
			Name: "Flow retention (warm)", Service: "store",
			Description: "Days the flows table keeps records before TTL. Adjust via app_settings.flow_retention_days; restart required for the schema-level TTL change.",
			Reload:      "restart", DefaultText: "30",
		},
		{
			Name: "Counter retention", Service: "store",
			Description: "Days iface_counter_samples are kept.",
			Reload:      "restart", DefaultText: "30",
		},
	}
}

