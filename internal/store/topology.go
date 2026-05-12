package store

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// NeighborRow is one row of the lldp_neighbors table, as returned by
// QueryNeighbors. Mirrors the table layout plus a couple of derived
// JSON-friendly fields (RemoteManagementAddr is rendered as a string,
// or empty when NULL).
type NeighborRow struct {
	LastSeen             time.Time `json:"last_seen"`
	FirstSeen            time.Time `json:"first_seen"`
	LocalExporter        string    `json:"local_exporter"`
	LocalIfIndex         uint32    `json:"local_ifindex"`
	LocalPortName        string    `json:"local_port_name"`
	DiscoveryProto       string    `json:"discovery_proto"`
	RemoteChassisID      string    `json:"remote_chassis_id"`
	RemoteSysName        string    `json:"remote_sys_name"`
	RemoteSysDesc        string    `json:"remote_sys_desc"`
	RemotePortID         string    `json:"remote_port_id"`
	RemoteCapabilities   string    `json:"remote_capabilities"`
	RemoteManagementAddr string    `json:"remote_management_addr"`
}

// QueryNeighbors returns every current neighbor row for one exporter.
// Uses FINAL on the ReplacingMergeTree so the freshest row per
// (local_ifindex, discovery_proto, remote_chassis_id, remote_port_id)
// wins — operators see the latest snapshot, not stale duplicates
// awaiting merge. Bounded by the table's 30-day TTL, so missing
// devices fall off the graph on their own.
func QueryNeighbors(ctx context.Context, conn driver.Conn, exporter netip.Addr) ([]NeighborRow, error) {
	exp16 := toIPv6(exporter)
	const q = `
SELECT
    last_seen, first_seen,
    local_ifindex, local_port_name,
    discovery_proto,
    remote_chassis_id, remote_sys_name, remote_sys_desc,
    remote_port_id, remote_capabilities,
    remote_management_addr
FROM lldp_neighbors FINAL
WHERE local_exporter = ?
ORDER BY local_ifindex, discovery_proto, remote_chassis_id`
	rows, err := conn.Query(ctx, q, exp16)
	if err != nil {
		return nil, fmt.Errorf("store: query neighbors: %w", err)
	}
	defer rows.Close()
	out := make([]NeighborRow, 0, 16)
	for rows.Next() {
		var (
			r    NeighborRow
			mgmt *netip.Addr
		)
		if err := rows.Scan(
			&r.LastSeen, &r.FirstSeen,
			&r.LocalIfIndex, &r.LocalPortName,
			&r.DiscoveryProto,
			&r.RemoteChassisID, &r.RemoteSysName, &r.RemoteSysDesc,
			&r.RemotePortID, &r.RemoteCapabilities,
			&mgmt,
		); err != nil {
			return nil, fmt.Errorf("store: scan neighbor: %w", err)
		}
		r.LocalExporter = exporter.Unmap().String()
		if mgmt != nil {
			r.RemoteManagementAddr = mgmt.Unmap().String()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopologyNode is one device on the network graph. Discovered = true
// means the device was named in someone else's LLDP/CDP table but
// FlowScope hasn't walked it (no SNMP credentials, not in flows yet,
// or simply unreachable). Reachable distinguishes a recently-seen
// exporter from a stale one — anything older than 5 min reads as
// "offline" in the UI.
type TopologyNode struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Address       string   `json:"address"`      // canonical IP; empty for discovered-only nodes that haven't been mapped
	SysDescr      string   `json:"sys_descr"`
	SysLocation   string   `json:"sys_location"` // device_inventory.sys_location; empty for discovered-only nodes and devices we haven't walked. The UI groups nodes by this value in "site" scope.
	Capabilities  []string `json:"capabilities"` // ["bridge","router"] etc., empty when unknown
	Discovered    bool     `json:"discovered"`   // true = not actively monitored
	Reachable     bool     `json:"reachable"`    // last_seen_at < 5min ago
	LastSeen      string   `json:"last_seen"`    // RFC3339; empty when never seen
}

// TopologyEdge is one link between two devices. The (Source, Target)
// pair is canonicalised by lexicographically sorting the two endpoint
// IDs at compute time so a bidirectional A↔B link collapses to a
// single edge. SourcePort + TargetPort identify the local interfaces
// at each end, with one side typically known precisely (the local
// exporter's ifIndex + name) and the other carrying the chassis-ID-
// keyed RemotePortID from the TLV.
type TopologyEdge struct {
	ID             string `json:"id"`
	Source         string `json:"source"`
	Target         string `json:"target"`
	SourcePort     string `json:"source_port"`
	TargetPort     string `json:"target_port"`
	DiscoveryProto string `json:"discovery_proto"`
	LastSeen       string `json:"last_seen"`
}

// TopologyResponse is what /api/topology returns. The shape is
// designed for direct consumption by react-flow on the SPA side —
// nodes / edges keyed identically to the prop names.
type TopologyResponse struct {
	Nodes []TopologyNode `json:"nodes"`
	Edges []TopologyEdge `json:"edges"`
}

// QueryTopology builds the full graph. Joins lldp_neighbors against
// device_inventory (for known-device enrichment) and exporter_health
// (for reachability). Returns a deduplicated bidirectional graph.
//
// Bidirectional dedup: a single edge in the response represents the
// physical link, not the direction. When both ends ran LLDP and
// reported each other, two rows describe the same cable. We collapse
// them by sorting the (source, target) IDs lexicographically and
// keying the edge by the sorted pair plus a per-end port disambiguator.
// First row wins; the second is dropped.
//
// Unknown remotes: anything in lldp_neighbors whose chassis can't be
// mapped to a known exporter (no inventory join hit, no management
// IP that matches) lands as a Discovered=true node with a synthetic
// "chassis:<id>" ID so the operator can still see "I'm peering with
// something I don't own".
func QueryTopology(ctx context.Context, conn driver.Conn) (*TopologyResponse, error) {
	// Pull every current neighbor row across all exporters.
	const qn = `
SELECT
    IPv6NumToString(local_exporter),
    local_ifindex, local_port_name,
    discovery_proto,
    remote_chassis_id, remote_sys_name, remote_sys_desc,
    remote_port_id, remote_capabilities, remote_management_addr,
    last_seen
FROM lldp_neighbors FINAL`
	rows, err := conn.Query(ctx, qn)
	if err != nil {
		return nil, fmt.Errorf("store: query topology: %w", err)
	}
	defer rows.Close()
	type rowT struct {
		localExporter      string
		localIfIndex       uint32
		localPortName      string
		proto              string
		chassisID          string
		remoteSysName      string
		remoteSysDesc      string
		remotePortID       string
		remoteCaps         string
		remoteMgmt         string
		lastSeen           time.Time
	}
	var raw []rowT
	for rows.Next() {
		var r rowT
		var mgmt *netip.Addr
		if err := rows.Scan(
			&r.localExporter,
			&r.localIfIndex, &r.localPortName,
			&r.proto,
			&r.chassisID, &r.remoteSysName, &r.remoteSysDesc,
			&r.remotePortID, &r.remoteCaps, &mgmt,
			&r.lastSeen,
		); err != nil {
			return nil, fmt.Errorf("store: scan topology row: %w", err)
		}
		r.localExporter = unmap4in6(r.localExporter)
		if mgmt != nil {
			r.remoteMgmt = mgmt.Unmap().String()
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Walked-device enrichment: sys_name + sys_descr + iface_count
	// for every exporter we've walked. Lets us label nodes with the
	// hostname instead of just an IP, and lets a "discovered-only"
	// remote that happens to be a walked device map back to its
	// canonical ID.
	type devInfo struct {
		sysName     string
		sysDescr    string
		sysLocation string
	}
	knownByIP := map[string]devInfo{}
	knownBySysName := map[string]string{} // sys_name → IP
	{
		const q = `
SELECT
    IPv6NumToString(exporter),
    argMax(sys_name, polled_at),
    argMax(sys_descr, polled_at),
    argMax(sys_location, polled_at)
FROM device_inventory
WHERE polled_at >= now() - INTERVAL 30 DAY
GROUP BY exporter`
		drows, derr := conn.Query(ctx, q)
		if derr == nil {
			for drows.Next() {
				var ipStr, sn, sd, sl string
				if err := drows.Scan(&ipStr, &sn, &sd, &sl); err == nil {
					ipStr = unmap4in6(ipStr)
					knownByIP[ipStr] = devInfo{sysName: sn, sysDescr: sd, sysLocation: sl}
					if sn != "" {
						knownBySysName[strings.ToLower(sn)] = ipStr
					}
				}
			}
			drows.Close()
		}
	}

	// Reachability: a device is reachable if it has an
	// exporter_health row newer than 5 minutes ago. Falls back to
	// "unreachable" when there's no row at all (probably never sent
	// flows; rendered greyed-out so operators notice).
	reachable := map[string]bool{}
	lastSeenByIP := map[string]time.Time{}
	{
		const q = `
SELECT IPv6NumToString(exporter), max(ts)
FROM exporter_health
WHERE ts >= now() - INTERVAL 1 HOUR
GROUP BY exporter`
		hrows, herr := conn.Query(ctx, q)
		if herr == nil {
			cutoff := time.Now().Add(-5 * time.Minute)
			for hrows.Next() {
				var ipStr string
				var ts time.Time
				if err := hrows.Scan(&ipStr, &ts); err == nil {
					ipStr = unmap4in6(ipStr)
					lastSeenByIP[ipStr] = ts
					if ts.After(cutoff) {
						reachable[ipStr] = true
					}
				}
			}
			hrows.Close()
		}
	}

	// resolveRemote returns the canonical node ID for the remote
	// end of an edge, plus the node payload to register. Preference
	// order: (1) management IP matches a known exporter, (2)
	// sys_name matches a known exporter's hostname, (3) treat as
	// discovered-only.
	resolveRemote := func(chassisID, sysName, sysDesc, mgmtIP, caps string) (string, TopologyNode) {
		// (1) management IP wins.
		if mgmtIP != "" {
			if dev, ok := knownByIP[mgmtIP]; ok {
				return mgmtIP, TopologyNode{
					ID:           mgmtIP,
					Label:        firstNonEmpty(dev.sysName, mgmtIP),
					Address:      mgmtIP,
					SysDescr:     dev.sysDescr,
					SysLocation:  dev.sysLocation,
					Capabilities: splitCaps(caps),
					Discovered:   false,
				}
			}
		}
		// (2) sys_name match.
		if sysName != "" {
			if ipStr, ok := knownBySysName[strings.ToLower(sysName)]; ok {
				dev := knownByIP[ipStr]
				return ipStr, TopologyNode{
					ID:           ipStr,
					Label:        firstNonEmpty(dev.sysName, ipStr),
					Address:      ipStr,
					SysDescr:     dev.sysDescr,
					SysLocation:  dev.sysLocation,
					Capabilities: splitCaps(caps),
					Discovered:   false,
				}
			}
		}
		// (3) discovered-only. ID is the chassis ID so multiple
		// neighbors of the same unknown device collapse correctly.
		// SysLocation stays empty — we haven't walked the device, so
		// no inventory row to source it from. The "site" scope filter
		// on the SPA falls back to device scope when the selected
		// node has no sys_location.
		nodeID := "chassis:" + chassisID
		label := sysName
		if label == "" {
			label = chassisID
		}
		return nodeID, TopologyNode{
			ID:           nodeID,
			Label:        label,
			Address:      mgmtIP, // possibly empty; helps the UI explain "discovered with mgmt IP X but not walked"
			SysDescr:     sysDesc,
			Capabilities: splitCaps(caps),
			Discovered:   true,
		}
	}

	nodes := map[string]TopologyNode{}
	addNode := func(n TopologyNode) {
		if existing, ok := nodes[n.ID]; ok {
			// Prefer the more enriched record. Known beats discovered.
			if !existing.Discovered {
				return
			}
		}
		// Fill reachability + last_seen at registration time.
		if n.Address != "" {
			n.Reachable = reachable[n.Address]
			if ts, ok := lastSeenByIP[n.Address]; ok {
				n.LastSeen = ts.UTC().Format(time.RFC3339)
			}
		}
		nodes[n.ID] = n
	}

	// Register every local exporter as a known node, even when its
	// own neighbor walk returned zero rows. This way an isolated
	// device still appears in the graph as a one-node island, which
	// is what the operator expects.
	for ip, dev := range knownByIP {
		addNode(TopologyNode{
			ID:          ip,
			Label:       firstNonEmpty(dev.sysName, ip),
			Address:     ip,
			SysDescr:    dev.sysDescr,
			SysLocation: dev.sysLocation,
		})
	}

	type edgeKey struct {
		a, b           string
		aPort, bPort   string
		proto          string
	}
	edges := map[edgeKey]TopologyEdge{}

	for _, r := range raw {
		// Local end: always a known device (we walked it).
		localDev := knownByIP[r.localExporter]
		addNode(TopologyNode{
			ID:          r.localExporter,
			Label:       firstNonEmpty(localDev.sysName, r.localExporter),
			Address:     r.localExporter,
			SysDescr:    localDev.sysDescr,
			SysLocation: localDev.sysLocation,
		})

		remoteID, remoteNode := resolveRemote(r.chassisID, r.remoteSysName, r.remoteSysDesc, r.remoteMgmt, r.remoteCaps)
		addNode(remoteNode)

		// Canonicalise edge direction: smaller ID is "source".
		src, srcPort := r.localExporter, fmt.Sprintf("%s (ifindex %d)", r.localPortName, r.localIfIndex)
		dst, dstPort := remoteID, r.remotePortID
		if src > dst {
			src, dst = dst, src
			srcPort, dstPort = dstPort, srcPort
		}
		// Strip ifindex annotation when the port name is empty —
		// don't render "(ifindex 0)" garbage on edge labels.
		srcPort = cleanPortLabel(srcPort, r.localPortName, r.localIfIndex)
		dstPort = cleanPortLabel(dstPort, "", 0)

		k := edgeKey{a: src, b: dst, aPort: srcPort, bPort: dstPort, proto: r.proto}
		// Bidirectional dedup: also collapse the symmetric (a→b vs
		// b→a) report where both sides ran LLDP. The canonical sort
		// above already makes the two rows hash to the same a/b,
		// but the port labels may differ (each side reports the
		// other's port). To collapse those properly, key only on
		// (a, b, proto) for the primary dedup, and pick a stable
		// edge with both ports filled in if either side knows them.
		stableKey := edgeKey{a: src, b: dst, proto: r.proto}
		if existing, ok := edges[stableKey]; ok {
			// Backfill missing ports if the second report fills them.
			if existing.SourcePort == "" && srcPort != "" {
				existing.SourcePort = srcPort
				edges[stableKey] = existing
			}
			if existing.TargetPort == "" && dstPort != "" {
				existing.TargetPort = dstPort
				edges[stableKey] = existing
			}
			// Last-seen → max.
			if r.lastSeen.After(parseRFC3339OrZero(existing.LastSeen)) {
				existing.LastSeen = r.lastSeen.UTC().Format(time.RFC3339)
				edges[stableKey] = existing
			}
			continue
		}
		_ = k
		edges[stableKey] = TopologyEdge{
			ID:             fmt.Sprintf("%s—%s/%s", src, dst, r.proto),
			Source:         src,
			Target:         dst,
			SourcePort:     srcPort,
			TargetPort:     dstPort,
			DiscoveryProto: r.proto,
			LastSeen:       r.lastSeen.UTC().Format(time.RFC3339),
		}
	}

	// Flatten and stable-sort outputs so the response is deterministic
	// across calls — easier for snapshot tests and avoids react-flow
	// re-running layout on every refresh just because Go map ordering
	// drifted.
	out := &TopologyResponse{
		Nodes: make([]TopologyNode, 0, len(nodes)),
		Edges: make([]TopologyEdge, 0, len(edges)),
	}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	for _, e := range edges {
		out.Edges = append(out.Edges, e)
	}
	sort.Slice(out.Edges, func(i, j int) bool {
		if out.Edges[i].Source == out.Edges[j].Source {
			return out.Edges[i].Target < out.Edges[j].Target
		}
		return out.Edges[i].Source < out.Edges[j].Source
	})
	return out, nil
}

// cleanPortLabel turns "(ifindex N)" garbage into either the bare
// port name or just "ifindex N". Keeps edge labels readable when the
// device hasn't yet been walked for its ifTable.
func cleanPortLabel(label, name string, idx uint32) string {
	if name != "" {
		return name
	}
	if idx > 0 {
		return fmt.Sprintf("ifindex %d", idx)
	}
	return label
}

// splitCaps splits the comma-separated capabilities string the walker
// produces into a slice. Empty in → empty out (not nil) so the JSON
// always renders [] rather than null.
func splitCaps(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// parseRFC3339OrZero parses s as RFC3339 or returns the zero time.
// Used to fold last_seen across the two halves of a bidirectional
// edge.
func parseRFC3339OrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// unmap4in6 strips ClickHouse's "::ffff:" prefix from a v4-mapped IPv6
// string so the API returns dotted-quad. Mirrors the helper in
// internal/snmpx; duplicated here to avoid an import cycle.
func unmap4in6(s string) string {
	const pfx = "::ffff:"
	if len(s) > len(pfx) && s[:len(pfx)] == pfx {
		return s[len(pfx):]
	}
	return s
}
