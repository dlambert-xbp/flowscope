package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/go-chi/chi/v5"

	"github.com/dlambert-xbp/flowscope/internal/notifier"
	"github.com/dlambert-xbp/flowscope/internal/rdns"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
	"github.com/dlambert-xbp/flowscope/internal/store"
)

// handlers groups HTTP handler methods that share a ClickHouse
// connection. The optional creds store powers the Settings → SNMP
// admin endpoints; when nil those endpoints return 503. settings
// carries the broader Settings/Services collaborators added in the
// 000005 settings slice — store, audit writer, audit reader, and
// the in-process service-name resolver. crypter and testDispatcher
// are wired only when FLOWSCOPE_SNMP_KEY is set; the webhook test
// endpoint returns 503 otherwise.
type handlers struct {
	conn             driver.Conn
	creds            snmpx.CredentialStore
	crypter          *snmpx.Crypter
	testDispatcher   *notifier.Dispatcher
	settings         settingsDeps
	ingestHealthURL  string
	ingestHealthHTTP *http.Client
	rdns             *rdns.Cache
	// auth carries the OIDC login-flow collaborators (Phase 2). The
	// signer is nil when FLOWSCOPE_SESSION_KEY_REF is unset — in that
	// case the /auth/* endpoints return 503 and the session-source on
	// the authz middleware is left unwired.
	auth authDeps
	// topoCache memoises /api/topology for ~30s so a dashboard with
	// the Neighbors tab open doesn't re-aggregate the full graph
	// every poll cycle. The walker cadence is 5 minutes anyway.
	topoCache topologyCache
	// scans holds in-flight and completed bulk-discovery scan jobs.
	// In-memory only: scans are short-lived and survive process
	// restarts only if the operator was actively reviewing one (they
	// kick off a new scan). nil when h.creds is nil — discovery is
	// useless without a profile library.
	scans *scanRegistry
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

// summary returns the Overview-tab aggregates over a time range.
//
//	GET /api/summary?window=300s
//	GET /api/summary?from=2026-05-07T14:00:00Z&to=2026-05-07T15:00:00Z
//
// Window defaults to 5 minutes; values from "1s" to "168h" are accepted.
// Absolute from/to (RFC3339) take precedence when both are supplied.
func (h *handlers) summary(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	s, err := store.QuerySummary(r.Context(), h.conn, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// dnsLookup performs reverse DNS for one or more IPs and returns
// a map keyed by the input IP. Private IPs (RFC 1918, etc.) are
// returned as skipped=true with no hostname. Public IPs are looked
// up against the api service's local resolver with a 200ms timeout
// per lookup, cached in-memory for an hour on success, 5min on
// no-PTR, 30s on resolver error.
//
//	GET /api/dns/lookup?ip=8.8.8.8
//	GET /api/dns/lookup?ip=8.8.8.8&ip=1.1.1.1
func (h *handlers) dnsLookup(w http.ResponseWriter, r *http.Request) {
	ips := r.URL.Query()["ip"]
	if len(ips) == 0 {
		writeError(w, http.StatusBadRequest, "ip query param is required (one or more)")
		return
	}
	// Cap input to keep one bad request from spawning thousands of
	// concurrent lookups. The Flows tab batches at most ~50 IPs
	// per visible page; 100 here is comfortable headroom.
	if len(ips) > 100 {
		ips = ips[:100]
	}
	results := h.rdns.LookupBatch(r.Context(), ips)
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(results),
		"results": results,
	})
}

// healthIngest proxies the ingest service's /health/ingest JSON
// snapshot. The URL is configured via FLOWSCOPE_INGEST_HEALTH_URL
// (typical: http://flowscope-ingest:9100/health/ingest); when
// unset this returns a 503 with a clear hint so the Overview
// IngestPanel can show "ingest health not configured" instead of
// hard-erroring.
//
//	GET /api/health/ingest
func (h *handlers) healthIngest(w http.ResponseWriter, r *http.Request) {
	if h.ingestHealthURL == "" {
		writeError(w, http.StatusServiceUnavailable,
			"FLOWSCOPE_INGEST_HEALTH_URL not set on the api service; ingest health not exposed",
		)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, h.ingestHealthURL, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := h.ingestHealthHTTP.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ingest health fetch: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// healthExporters returns per-exporter loss aggregated over the
// time range. Powers the Overview "Exporter accuracy" panel.
//
//	GET /api/health/exporters?window=300s
func (h *handlers) healthExporters(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	rows, err := store.QueryExporterHealth(r.Context(), h.conn, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"window": tr.WindowDuration().String(),
	})
}

// healthStorage returns ClickHouse-side write health: insert lag,
// recent rows/sec, table row-count estimates. Powers the Overview
// Storage panel.
//
//	GET /api/health/storage
func (h *handlers) healthStorage(w http.ResponseWriter, r *http.Request) {
	s, err := store.QueryStorageHealth(r.Context(), h.conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// healthStreams returns one row per ingest source observed over the
// time range. Powers the Overview "Streams" panel.
//
//	GET /api/health/streams?window=300s
func (h *handlers) healthStreams(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	rows, err := store.QuerySourceBreakdown(r.Context(), h.conn, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"window": tr.WindowDuration().String(),
	})
}

// flowsFlagsTimeseries returns bucketed TCP flag counts over the
// time range, narrowed by the filter. Powers the drawer
// "Connection state" tab.
//
//	GET /api/flows/flags-timeseries?window=300s&bucket=5
//	     &exporter=...&src_addr=...&dst_addr=...&...
//
// Bucket defaults to autoBucket(window) when 0 or missing; same
// auto-bucket curve as /api/flows/timeseries so a drawer with
// both tabs open shows aligned bucket boundaries.
func (h *handlers) flowsFlagsTimeseries(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	bucket := parseInt(r.URL.Query().Get("bucket"), 0)
	if bucket <= 0 {
		bucket = autoBucket(tr)
	}
	rows, err := store.QueryFlagsTimeseries(r.Context(), h.conn, tr, bucket, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":          len(rows),
		"buckets":        rows,
		"bucket_seconds": bucket,
		"window":         tr.WindowDuration().String(),
	})
}

// flowsTimeseries returns bucketed bytes/packets/flows over the
// time range, narrowed by the filter. Powers the drill-in chart on
// the Flows-tab drawer.
//
//	GET /api/flows/timeseries?window=300s&bucket=5
//	     &exporter=...&src_addr=...&dst_addr=...&src_port=...&dst_port=...&proto=...
//
// bucket is in seconds; defaults to autoBucket(window) when 0 or
// missing. All filter keys mirror /api/top/*, so the drawer can
// reuse the same FilterTrigger wiring.
func (h *handlers) flowsTimeseries(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	bucket := parseInt(r.URL.Query().Get("bucket"), 0)
	if bucket <= 0 {
		bucket = autoBucket(tr)
	}
	rows, err := store.QueryFlowsTimeseries(r.Context(), h.conn, tr, bucket, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":          len(rows),
		"points":         rows,
		"bucket_seconds": bucket,
		"window":         tr.WindowDuration().String(),
	})
}

// autoBucket targets ~120 points across the window so the chart is
// dense but readable. Values are clamped to a small set of human-
// friendly multiples (1/5/15/30s, 1/5/15m, 1h) so axes don't show
// awkward intervals.
func autoBucket(tr store.TimeRange) int {
	span := int(tr.Seconds())
	if span < 120 {
		span = 120
	}
	target := span / 120
	switch {
	case target <= 1:
		return 1
	case target <= 5:
		return 5
	case target <= 15:
		return 15
	case target <= 30:
		return 30
	case target <= 60:
		return 60
	case target <= 300:
		return 300
	case target <= 900:
		return 900
	default:
		return 3600
	}
}

// flowsList returns a paginated, filterable, sortable list of flow
// records. Powers the Investigate panel on the Flows tab.
//
//	GET /api/flows/list?window=300s&limit=50&offset=0&sort=observed&dir=desc
//	     &exporter=...&src_addr=...&dst_addr=...&src_port=...&dst_port=...&proto=...
//
// Limit defaults to 50 (max 500); offset defaults to 0 (max 100,000).
// sort ∈ {observed, bytes, packets}, dir ∈ {asc, desc}. All filter
// keys mirror /api/top/* exactly, so the same filter chip set narrows
// both surfaces.
func (h *handlers) flowsList(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	q := r.URL.Query()
	limit := parseInt(q.Get("limit"), 50)
	offset := parseInt(q.Get("offset"), 0)
	sort := store.ParseFlowsListSort(q.Get("sort"))
	dir := store.ParseFlowsListDir(q.Get("dir"))
	rows, err := store.QueryFlowsList(
		r.Context(), h.conn, tr, limit, offset, sort, dir, parseFilter(r),
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"flows":  rows,
		"limit":  limit,
		"offset": offset,
		"sort":   string(sort),
		"dir":    string(dir),
		"window": tr.WindowDuration().String(),
	})
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
		Exporter:      q.Get("exporter"),
		SrcAddr:       q.Get("src_addr"),
		DstAddr:       q.Get("dst_addr"),
		SrcPort:       parseUint16(q.Get("src_port")),
		DstPort:       parseUint16(q.Get("dst_port")),
		Proto:         parseUint16(q.Get("proto")),
		InputIfIndex:  parseUint32(q.Get("input_ifindex")),
		OutputIfIndex: parseUint32(q.Get("output_ifindex")),
		SrcAS:         parseUint32(q.Get("src_as")),
		DstAS:         parseUint32(q.Get("dst_as")),
	}
}

func parseUint32(s string) uint32 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
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
	tr := parseTimeRange(r, 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopTalkers(r.Context(), h.conn, tr, limit, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
	})
}

func (h *handlers) topServices(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopServices(r.Context(), h.conn, tr, limit, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
	})
}

func (h *handlers) topProtocols(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopProtocols(r.Context(), h.conn, tr, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
	})
}

func (h *handlers) topASN(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopASN(r.Context(), h.conn, tr, limit, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
	})
}

func (h *handlers) topConversations(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopConversations(r.Context(), h.conn, tr, limit, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
	})
}

func (h *handlers) topInterfaces(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	limit := parseInt(r.URL.Query().Get("limit"), 20)
	sort := store.ParseTopNSort(r.URL.Query().Get("sort"))
	rows, err := store.QueryTopInterfaces(r.Context(), h.conn, tr, limit, sort, parseFilter(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"source": "flows",
		"sort":   string(sort),
		"window": tr.WindowDuration().String(),
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
// next scheduler tick. Profile-referenced bindings resolve the profile
// before walking so the test exercises the same code path the
// scheduler does.
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
	var cfg snmpx.Config
	if cred.UsesProfile() {
		p, perr := h.creds.GetProfile(r.Context(), cred.ProfileID)
		if perr != nil {
			writeError(w, http.StatusFailedDependency, "profile lookup failed: "+perr.Error())
			return
		}
		cfg = snmpx.FromProfile(p, cred.Port, cred.Interval)
	} else {
		cfg = snmpx.FromCredential(cred)
	}
	client := snmpx.NewClient(cfg)
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
		"ok":               true,
		"sys_descr":        inv.SysDescr,
		"sys_name":         inv.SysName,
		"interfaces":       len(inv.Interfaces),
		"poll_duration_ms": inv.PollDurationMs,
	})
}

// requestSnmpWalk enqueues an immediate SNMP walk for exporter. The
// snmp scheduler picks the request up on its next dispatch tick (≤ 30s)
// and walks regardless of the configured cadence. Returns 202 Accepted
// once the request is durably queued — the actual walk completes
// asynchronously and the result lands in device_inventory /
// device_snmp_interfaces the way scheduled walks do.
//
//	POST /api/devices/{exporter}/snmp/walk
func (h *handlers) requestSnmpWalk(w http.ResponseWriter, r *http.Request) {
	if h.creds == nil {
		writeError(w, http.StatusServiceUnavailable, "credential management disabled")
		return
	}
	exporter := chi.URLParam(r, "exporter")
	if _, err := netip.ParseAddr(exporter); err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	if err := h.creds.RequestWalk(r.Context(), exporter, actorFromRequest(r)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "exporter": exporter})
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

// alertDetail returns the full timeline + linked flows for one alert.
//
//	GET /api/alerts/{id}
//
// Powers the alert detail modal: a click on an alert row opens the
// dialog with (a) the alert summary, (b) every alert_events row for
// this (rule, scope, group_key) as a chronological timeline, and (c)
// a short list of linked flows derived from the rule's labels.
func (h *handlers) alertDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	d, err := store.QueryAlertDetail(r.Context(), h.conn, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "alert not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
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
// the time range.
//
//	GET /api/devices?window=300s
//	GET /api/devices?from=...&to=...
func (h *handlers) devices(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	rows, err := store.QueryDevices(r.Context(), h.conn, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":   len(rows),
		"devices": rows,
		"window":  tr.WindowDuration().String(),
	})
}

// deviceResources returns the per-component CPU / memory / storage
// timeseries for one exporter over the requested window. The body
// shape is one entry per (kind, component), each carrying the
// freshest reading plus a small sparkline array of percent values.
// Drives the Devices Summary tab tiles + sparklines.
//
//	GET /api/devices/{exporter}/resources?window=24h
//	GET /api/devices/{exporter}/resources?from=...&to=...
//
// Empty rows array is the normal "no SNMP yet" case — the UI shows
// the "no resource data" empty state instead of an error.
func (h *handlers) deviceResources(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	tr := parseTimeRange(r, 24*time.Hour)
	rows, err := store.QueryDeviceResources(r.Context(), h.conn, exporter, tr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exporter": exporter.Unmap().String(),
		"count":    len(rows),
		"rows":     rows,
		"window":   tr.WindowDuration().String(),
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

// deviceBGP returns the BGP peers an exporter has, grouped by VRF.
// Empty list means we have not observed any BGP peers from this
// device in the last 24h — could be a non-BGP-speaking switch, a
// device the snmp service hasn't walked yet, or a device we walked
// but whose BGP4-MIB is empty (no peers configured).
//
//	GET /api/devices/{exporter}/bgp
func (h *handlers) deviceBGP(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	groups, err := store.QueryDeviceBGP(r.Context(), h.conn, exporter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total := 0
	for _, g := range groups {
		total += g.PeerCount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exporter":   exporter.Unmap().String(),
		"vrf_count":  len(groups),
		"peer_count": total,
		"vrfs":       groups,
	})
}

// device returns the summary for a single exporter.
//
//	GET /api/devices/{exporter}?window=300s
//	GET /api/devices/{exporter}?from=...&to=...
func (h *handlers) device(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	tr := parseTimeRange(r, 5*time.Minute)
	d, err := store.QueryDevice(r.Context(), h.conn, exporter, tr)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "exporter not found in flows or SNMP inventory")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// interfaces returns one row per (exporter, ifindex) seen in the
// time range, ranked by peak bandwidth.
//
//	GET /api/interfaces?window=300s&exporter=10.2.0.11
//	GET /api/interfaces?from=...&to=...&exporter=10.2.0.11
func (h *handlers) interfaces(w http.ResponseWriter, r *http.Request) {
	tr := parseTimeRange(r, 5*time.Minute)
	exporter := r.URL.Query().Get("exporter")
	rows, err := store.QueryInterfaces(r.Context(), h.conn, tr, exporter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      len(rows),
		"interfaces": rows,
		"source":     "counters",
		"window":     tr.WindowDuration().String(),
	})
}

// interfaceTimeseries returns bytes/sec rates derived from successive
// counter samples. Per VISION.md §3.3, this is the AUTHORITATIVE rate.
//
//	GET /api/interfaces/{exporter}/{ifindex}/timeseries?seconds=300
//	GET /api/interfaces/{exporter}/{ifindex}/timeseries?from=...&to=...
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

	// Prefer absolute from/to when both are present; otherwise honor the
	// `seconds` param for backwards compatibility, and finally fall back
	// to the trailing `window` form.
	q := r.URL.Query()
	var tr store.TimeRange
	if from, errFrom := time.Parse(time.RFC3339, q.Get("from")); errFrom == nil {
		if to, errTo := time.Parse(time.RFC3339, q.Get("to")); errTo == nil {
			tr = store.AbsoluteRange(from, to)
		}
	}
	if !tr.IsAbsolute() {
		if s := q.Get("seconds"); s != "" {
			seconds := parseInt(s, 300)
			if seconds < 1 {
				seconds = 1
			}
			if seconds > 24*60*60 {
				seconds = 24 * 60 * 60
			}
			tr = store.TrailingWindow(time.Duration(seconds) * time.Second)
		} else {
			tr = store.TrailingWindow(parseWindow(q.Get("window"), 5*time.Minute))
		}
	}

	ts, err := store.QueryInterfaceTimeseries(r.Context(), h.conn, exporter, uint32(ifindex64), tr)
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

// parseTimeRange returns a TimeRange from the request's query string.
// When both `from` and `to` are present and parseable as RFC3339
// timestamps, it returns an absolute range. Otherwise it falls back to
// the trailing `window` parameter (or the supplied default).
func parseTimeRange(r *http.Request, defWindow time.Duration) store.TimeRange {
	q := r.URL.Query()
	fromStr := q.Get("from")
	toStr := q.Get("to")
	if fromStr != "" && toStr != "" {
		from, errFrom := time.Parse(time.RFC3339, fromStr)
		to, errTo := time.Parse(time.RFC3339, toStr)
		if errFrom == nil && errTo == nil && !from.IsZero() && !to.IsZero() {
			return store.AbsoluteRange(from, to)
		}
	}
	return store.TrailingWindow(parseWindow(q.Get("window"), defWindow))
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
