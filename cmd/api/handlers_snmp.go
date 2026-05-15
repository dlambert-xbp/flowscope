package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

// ---- Profile library --------------------------------------------------------
//
// One credential profile is one named, reusable v2c or v3 configuration.
// Bindings (snmp_credentials) either reference a profile by id or carry an
// inline custom credential. The profile library replaces the old role-keyed
// snmp_global_defaults endpoints.

// listProfiles returns every profile with secrets REDACTED.
//
//	GET /api/snmp/profiles
func (h *handlers) listProfiles(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	rows, err := h.creds.ListProfiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":    len(rows),
		"profiles": rows,
	})
}

// getProfile returns one profile with secrets REDACTED.
//
//	GET /api/snmp/profiles/{id}
func (h *handlers) getProfile(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	id := chi.URLParam(r, "id")
	p, err := h.creds.GetProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, snmpx.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "profile not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Community = ""
	p.V3AuthPass = ""
	p.V3PrivPass = ""
	writeJSON(w, http.StatusOK, p)
}

// putProfile creates (when id is "new") or updates an existing profile.
// Empty passphrase fields preserve the existing secret — same "leave
// blank to keep" convention as bindings + the old globals.
//
//	POST /api/snmp/profiles            (create — server allocates id)
//	PUT  /api/snmp/profiles/{id}       (update)
func (h *handlers) putProfile(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	var body snmpx.Profile
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// Path id wins over body id on PUT; POST generates a fresh id.
	pathID := chi.URLParam(r, "id")
	if pathID != "" {
		body.ID = pathID
	}
	if body.ID == "" {
		body.ID = snmpx.NewProfileID()
	} else if _, err := uuid.Parse(body.ID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile id")
		return
	}

	// Preserve existing secrets when caller passed blank.
	if body.Community == "" || body.V3AuthPass == "" || body.V3PrivPass == "" {
		if existing, err := h.creds.GetProfile(r.Context(), body.ID); err == nil {
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

	if err := h.creds.SetProfile(r.Context(), body, actorFromRequest(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": body.ID})
}

// deleteProfile tombstones the profile. Refuses with 409 if any
// binding still references it.
//
//	DELETE /api/snmp/profiles/{id}
func (h *handlers) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.creds.DeleteProfile(r.Context(), id); err != nil {
		if errors.Is(err, snmpx.ErrProfileInUse) {
			writeError(w, http.StatusConflict, "profile is referenced by one or more bindings; unbind them first")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// ---- Bulk discovery scan ----------------------------------------------------
//
// A scan job runs in the api process, probing each address in the chosen
// range against a single profile. The operator polls /api/snmp/scan/{id}
// for progress, then POSTs /commit with the matched IPs they want to bind.

// scanRegistry is the in-memory job table for bulk discovery scans.
type scanRegistry struct {
	mu   sync.Mutex
	jobs map[string]*scanJob
}

func newScanRegistry() *scanRegistry {
	return &scanRegistry{jobs: make(map[string]*scanJob)}
}

func (r *scanRegistry) put(j *scanJob) {
	r.mu.Lock()
	r.jobs[j.ID] = j
	r.mu.Unlock()
}

func (r *scanRegistry) get(id string) (*scanJob, bool) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	return j, ok
}

func (r *scanRegistry) delete(id string) {
	r.mu.Lock()
	delete(r.jobs, id)
	r.mu.Unlock()
}

// scanJob is one bulk-discovery scan. State transitions:
//
//	running → done                  normal completion
//	running → cancelled             operator DELETE
//	running → done (with empty)     ctx error during the worker phase
type scanJob struct {
	ID         string
	ProfileID  string
	Range      string
	Total      int
	StartedAt  time.Time
	FinishedAt time.Time

	mu      sync.Mutex
	State   string // "running" | "done" | "cancelled"
	Probed  int
	Results []snmpx.ScanResult

	cancel context.CancelFunc
}

func (j *scanJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	matched := 0
	rejected := 0
	silent := 0
	for _, r := range j.Results {
		switch {
		case r.Matched:
			matched++
		case r.Silent:
			silent++
		default:
			rejected++
		}
	}
	out := map[string]any{
		"id":          j.ID,
		"profile_id":  j.ProfileID,
		"range":       j.Range,
		"state":       j.State,
		"total":       j.Total,
		"probed":      j.Probed,
		"matched":     matched,
		"rejected":    rejected,
		"silent":      silent,
		"started_at":  j.StartedAt,
		"results":     append([]snmpx.ScanResult(nil), j.Results...),
	}
	if !j.FinishedAt.IsZero() {
		out["finished_at"] = j.FinishedAt
	}
	return out
}

func (j *scanJob) record(r snmpx.ScanResult) {
	j.mu.Lock()
	j.Results = append(j.Results, r)
	j.Probed++
	j.mu.Unlock()
}

func (j *scanJob) markDone(state string) {
	j.mu.Lock()
	j.State = state
	j.FinishedAt = time.Now().UTC()
	j.mu.Unlock()
}

// createScan kicks off a new bulk discovery scan. The HTTP response
// returns immediately with the job id; the operator polls /api/snmp/scan/{id}
// for results.
//
//	POST /api/snmp/scan
//	{ "profile_id": "...", "range": "10.0.0.0/24" }
func (h *handlers) createScan(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil || h.scans == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	var body struct {
		ProfileID string `json:"profile_id"`
		Range     string `json:"range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.ProfileID == "" {
		writeError(w, http.StatusBadRequest, "profile_id required")
		return
	}
	addrs, err := snmpx.ParseRange(body.Range)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prof, err := h.creds.GetProfile(r.Context(), body.ProfileID)
	if err != nil {
		if errors.Is(err, snmpx.ErrProfileNotFound) {
			writeError(w, http.StatusNotFound, "profile not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	job := &scanJob{
		ID:        uuid.NewString(),
		ProfileID: prof.ID,
		Range:     body.Range,
		Total:     len(addrs),
		StartedAt: time.Now().UTC(),
		State:     "running",
		Results:   make([]snmpx.ScanResult, 0, len(addrs)),
		cancel:    cancel,
	}
	h.scans.put(job)

	// Detach the scan from the HTTP request lifetime — the operator's
	// POST returns now and the workers keep running.
	go func() {
		defer cancel()
		scanner := snmpx.NewScanner()
		cfg := snmpx.FromProfile(prof, 0, 0)
		scanner.Scan(jobCtx, cfg, addrs, func(res snmpx.ScanResult) {
			job.record(res)
		})
		if jobCtx.Err() != nil {
			job.markDone("cancelled")
		} else {
			job.markDone("done")
		}
	}()

	writeJSON(w, http.StatusAccepted, job.snapshot())
}

// getScan returns the current state of a scan job, including all
// collected results.
//
//	GET /api/snmp/scan/{id}
func (h *handlers) getScan(w http.ResponseWriter, r *http.Request) {
	if h.scans == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	id := chi.URLParam(r, "id")
	job, ok := h.scans.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "scan not found")
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

// commitScan binds each selected IP to the scan's profile, creating
// a new credential row per IP. IPs not in the matched results are
// ignored. Returns the count bound.
//
//	POST /api/snmp/scan/{id}/commit
//	{ "ips": ["10.0.0.5", "10.0.0.11"] }
func (h *handlers) commitScan(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil || h.scans == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	id := chi.URLParam(r, "id")
	job, ok := h.scans.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "scan not found")
		return
	}
	var body struct {
		IPs []string `json:"ips"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(body.IPs) == 0 {
		writeError(w, http.StatusBadRequest, "ips required")
		return
	}

	// Build a set of matched IPs for fast membership checks.
	matched := make(map[string]bool, len(job.Results))
	job.mu.Lock()
	for _, r := range job.Results {
		if r.Matched {
			matched[r.IP] = true
		}
	}
	job.mu.Unlock()

	// Load the profile once — port + interval come from it.
	prof, err := h.creds.GetProfile(r.Context(), job.ProfileID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "profile lookup: "+err.Error())
		return
	}

	bound := make([]string, 0, len(body.IPs))
	skipped := make([]string, 0)
	for _, ip := range body.IPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			skipped = append(skipped, ip)
			continue
		}
		if !matched[ip] {
			skipped = append(skipped, ip)
			continue
		}
		c := snmpx.Credential{
			Exporter:    ip,
			ProfileID:   prof.ID,
			Port:        prof.Port,
			IntervalSec: prof.IntervalSec,
		}
		if err := h.creds.Set(r.Context(), c, actorFromRequest(r)); err != nil {
			skipped = append(skipped, ip)
			continue
		}
		bound = append(bound, ip)
	}
	sort.Strings(bound)
	sort.Strings(skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"bound":   bound,
		"skipped": skipped,
	})
}

// cancelScan terminates a running scan and removes it from the
// registry. Idempotent; 200 on either running or done.
//
//	DELETE /api/snmp/scan/{id}
func (h *handlers) cancelScan(w http.ResponseWriter, r *http.Request) {
	if h.scans == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	id := chi.URLParam(r, "id")
	job, ok := h.scans.get(id)
	if ok {
		if job.cancel != nil {
			job.cancel()
		}
		h.scans.delete(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}
