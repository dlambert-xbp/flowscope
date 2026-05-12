package main

import (
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dlambert-xbp/flowscope/internal/store"
)

// topologyCacheTTL is how long /api/topology results stay cached
// server-side. The walker cadence is 5 minutes, so anything under
// that is upper-bounded by walker freshness anyway — 30 seconds
// strikes a balance between "page load is cheap" and "panning the
// dashboard reflects a new neighbor walking promptly".
const topologyCacheTTL = 30 * time.Second

// topologyCache is a tiny single-entry cache for the full graph.
// The graph is fleet-wide and read-mostly so a process-level cache
// is appropriate; we don't shard by tenant or user since there's
// only ever one of either.
type topologyCache struct {
	mu       sync.Mutex
	resp     *store.TopologyResponse
	cachedAt time.Time
}

// topology returns the network graph as nodes/edges ready for
// react-flow on the SPA side. Cached for topologyCacheTTL — operators
// who just configured a new device can re-render within 30 seconds.
//
//	GET /api/topology
//
// Response shape:
//
//	{
//	  "nodes": [{"id": "10.0.0.1", "label": "core-01", ...}, ...],
//	  "edges": [{"id": "...", "source": "...", "target": "...", ...}, ...]
//	}
//
// Bidirectional edges are deduplicated server-side: A↔B is one edge,
// not two. The discovery_proto field carries either "lldp" or "cdp".
func (h *handlers) topology(w http.ResponseWriter, r *http.Request) {
	h.topoCache.mu.Lock()
	if h.topoCache.resp != nil && time.Since(h.topoCache.cachedAt) < topologyCacheTTL {
		cached := h.topoCache.resp
		h.topoCache.mu.Unlock()
		writeJSON(w, http.StatusOK, cached)
		return
	}
	h.topoCache.mu.Unlock()

	resp, err := store.QueryTopology(r.Context(), h.conn)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.topoCache.mu.Lock()
	h.topoCache.resp = resp
	h.topoCache.cachedAt = time.Now()
	h.topoCache.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// deviceNeighbors returns one device's adjacency list — the same
// shape as the lldp_neighbors table plus the resolved local port
// name (already persisted denormalised by the walker).
//
//	GET /api/devices/{exporter}/neighbors
//
// Empty rows is the common case when SNMP hasn't walked the device
// yet, or when the device doesn't implement LLDP / CDP. The Devices
// → Neighbors tab uses this directly when the operator drills into
// one device.
func (h *handlers) deviceNeighbors(w http.ResponseWriter, r *http.Request) {
	exporterStr := chi.URLParam(r, "exporter")
	exporter, err := netip.ParseAddr(exporterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid exporter address")
		return
	}
	rows, err := store.QueryNeighbors(r.Context(), h.conn, exporter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exporter":  exporter.Unmap().String(),
		"count":     len(rows),
		"neighbors": rows,
	})
}
