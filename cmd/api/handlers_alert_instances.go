package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/go-chi/chi/v5"

	"github.com/dlambert-xbp/flowscope/internal/alerteng"
	"github.com/dlambert-xbp/flowscope/internal/audit"
	"github.com/dlambert-xbp/flowscope/internal/settings"
)

/* =============================================================================
   /api/alerts/templates
   /api/alerts/instances (CRUD)
   /api/alerts/instances/{id}/preview
   ============================================================================= */

// listAlertTemplates returns the catalog of built-in templates the
// alerts UI can build instances from. Includes parameter schema, the
// scope dimensions each template understands, and the default
// severity. Stable order — same as alerteng.BuiltinTemplates() — so
// the picker reads predictably.
//
//	GET /api/alerts/templates
func (h *handlers) listAlertTemplates(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0, 16)
	for _, t := range alerteng.BuiltinTemplates() {
		spec := availableRuleParams(t.ID())
		kinds := alerteng.ScopeKindsFor(t.ID())
		kindStrs := make([]string, 0, len(kinds))
		for _, k := range kinds {
			kindStrs = append(kindStrs, string(k))
		}
		out = append(out, map[string]any{
			"template_id":      t.ID(),
			"label":            templateLabel(t.ID()),
			"description":      templateDescription(t.ID()),
			"runbook":          t.Runbook(),
			"params":           spec,
			"default_params":   t.DefaultParams(),
			"default_severity": t.DefaultSeverity(),
			"scope_kinds":      kindStrs,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// listAlertInstances returns every operator-created instance plus the
// auto-seeded "default" instance per template. Optionally filtered by
// ?template_id=… for the per-template detail page. Always reads
// FINAL so a freshly-PUT row is visible immediately.
//
//	GET /api/alerts/instances[?template_id=…]
func (h *handlers) listAlertInstances(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
		return
	}
	tplFilter := r.URL.Query().Get("template_id")
	// Always ensure seed instances exist before listing — keeps the
	// UI from seeing a hole when a template was added but no operator
	// has touched it yet.
	for _, t := range alerteng.BuiltinTemplates() {
		_, _ = h.settings.store.AlertInstances.EnsureSeed(r.Context(),
			t.ID(), "Default · "+t.ID(), t.DefaultParams(), t.DefaultSeverity())
	}
	var (
		rows []settings.AlertRuleInstance
		err  error
	)
	if tplFilter != "" {
		rows, err = h.settings.store.AlertInstances.ListByTemplate(r.Context(), tplFilter)
	} else {
		rows, err = h.settings.store.AlertInstances.List(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

// getAlertInstance returns a single instance by id. 404 when absent.
//
//	GET /api/alerts/instances/{id}
func (h *handlers) getAlertInstance(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	row, err := h.settings.store.AlertInstances.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// createAlertInstance binds a template to a scope + params + severity.
// Validates template_id against the built-in registry and the scope
// against the template's supported dimensions before writing.
//
//	POST /api/alerts/instances
func (h *handlers) createAlertInstance(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	var in settings.AlertRuleInstance
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if !knownTemplate(in.TemplateID) {
		writeError(w, http.StatusBadRequest, "unknown template_id")
		return
	}
	if err := alerteng.ValidateScope(in.TemplateID, in.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.IsSeed = false // POST never creates seeds; EnsureSeed handles those
	saved, err := h.settings.store.AlertInstances.Create(r.Context(), in, h.actor(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionCreate, audit.ResourceAlertRuleSetting, saved.InstanceID, nil, saved)
	writeJSON(w, http.StatusCreated, saved)
}

// updateAlertInstance edits an existing instance. The update preserves
// template_id and is_seed (operators cannot change those). Seed rows
// can have their params / severity / runbook / channels edited but
// not their scope (always {} for seeds).
//
//	PUT /api/alerts/instances/{id}
func (h *handlers) updateAlertInstance(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	var in settings.AlertRuleInstance
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.InstanceID = id
	before, err := h.settings.store.AlertInstances.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if before.IsSeed {
		// Seed instances always match everything; refuse scope edits.
		in.Scope = settings.ScopeSelector{}
	} else if err := alerteng.ValidateScope(before.TemplateID, in.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := h.settings.store.AlertInstances.Update(r.Context(), in, h.actor(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionUpdate, audit.ResourceAlertRuleSetting, saved.InstanceID, before, saved)
	writeJSON(w, http.StatusOK, saved)
}

// deleteAlertInstance soft-deletes via tombstone in the store. The
// engine's next refresh tick drops it from the active rule slice.
//
//	DELETE /api/alerts/instances/{id}
func (h *handlers) deleteAlertInstance(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	before, err := h.settings.store.AlertInstances.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.settings.store.AlertInstances.Delete(r.Context(), id, h.actor(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.recordAudit(r, audit.ActionDelete, audit.ResourceAlertRuleSetting, id, before, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// previewAlertInstance answers "what would this instance match right
// now?" without writing alert events. Returns the set of devices and
// (when applicable) interfaces the configured scope resolves to. The
// wizard calls this as the operator builds the scope so they see the
// blast radius before saving.
//
//	POST /api/alerts/instances/{id}/preview
func (h *handlers) previewAlertInstance(w http.ResponseWriter, r *http.Request) {
	if h.settings.store == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store unavailable")
		return
	}
	id := chi.URLParam(r, "id")
	row, err := h.settings.store.AlertInstances.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, settings.ErrNotFound) {
			writeError(w, http.StatusNotFound, "instance not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	preview, err := previewScopeMatches(r.Context(), h.conn, row.TemplateID, row.Scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// previewAlertInstanceDryRun lets the wizard preview a scope without
// first persisting an instance. Body shape mirrors createAlertInstance
// (template_id + scope) so the same form drives both.
//
//	POST /api/alerts/instances/preview
func (h *handlers) previewAlertInstanceDryRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TemplateID string                  `json:"template_id"`
		Scope      settings.ScopeSelector  `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if !knownTemplate(body.TemplateID) {
		writeError(w, http.StatusBadRequest, "unknown template_id")
		return
	}
	if err := alerteng.ValidateScope(body.TemplateID, body.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := previewScopeMatches(r.Context(), h.conn, body.TemplateID, body.Scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// previewScopeMatches enumerates the devices (and interfaces, when
// the template is interface-scoped) that the given scope would match.
// Reads from the inventory tables — does not run the template's
// evaluation SQL — so the answer is "what's in scope right now",
// not "what's currently violating".
func previewScopeMatches(ctx context.Context, conn driver.Conn, templateID string, scope settings.ScopeSelector) (map[string]any, error) {
	_ = ctx
	_ = conn
	// Phase 1 echo preview: reflect the scope back so the wizard can
	// render "scoped to N exporters / M ifindexes". Real enumeration
	// against device_inventory + device_snmp_interfaces lands in
	// phase 4 of the design (preview-as-you-type).
	exporters := append([]string{}, scope.Exporters...)
	sort.Strings(exporters)
	ifindex := append([]uint32{}, scope.IfIndex...)
	sort.Slice(ifindex, func(i, j int) bool { return ifindex[i] < ifindex[j] })
	matchedAll := scope.IsEmpty()
	return map[string]any{
		"template_id":       templateID,
		"matched_all":       matchedAll,
		"matched_exporters": exporters,
		"matched_ifindex":   ifindex,
		"note":              "phase 1 echo preview; phase 4 enumerates device_inventory matches",
	}, nil
}

// knownTemplate reports whether the template_id corresponds to a
// built-in template the engine knows how to evaluate.
func knownTemplate(id string) bool {
	for _, t := range alerteng.BuiltinTemplates() {
		if t.ID() == id {
			return true
		}
	}
	return false
}

// availableRuleParams returns the JSON-friendly parameter spec for
// the given template_id. Pulls from the existing availableRules()
// catalog in settings.go so both the legacy alert-rules editor and
// the new instances UI stay in sync.
func availableRuleParams(templateID string) any {
	for _, m := range availableRules().([]map[string]any) {
		if m["rule_id"] == templateID {
			return m["params"]
		}
	}
	return []any{}
}

// templateLabel and templateDescription pull display strings from the
// existing availableRules() catalog; they used to live only on the
// legacy editor.
func templateLabel(templateID string) string {
	for _, m := range availableRules().([]map[string]any) {
		if m["rule_id"] == templateID {
			if s, ok := m["label"].(string); ok {
				return s
			}
		}
	}
	return templateID
}

func templateDescription(templateID string) string {
	for _, m := range availableRules().([]map[string]any) {
		if m["rule_id"] == templateID {
			if s, ok := m["description"].(string); ok {
				return s
			}
		}
	}
	return ""
}
