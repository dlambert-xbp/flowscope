package alerteng

// rules_bgp.go — BGP-state alert rule. Reads bgp_peers (populated by
// the snmp service's bgpPeerTable walker). Fires when the latest
// state for a peer is anything other than "established" and that
// non-established state has held for at least EstablishedMinSeconds.
//
// The dwell window keeps a brief route-flap from spamming the alert
// stream — when a session re-establishes inside the window the
// engine's stability-window auto-close handles the rest.

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// BGPNeighborDown fires when the latest argMax(state, polled_at) for
// a (exporter, peer_addr) is not "established" and the most recent
// poll is fresher than LookbackSeconds (so a long-decommissioned peer
// can't keep firing forever).
type BGPNeighborDown struct {
	EstablishedMinSeconds int // dwell time before firing — default 60
	LookbackSeconds       int // upper bound on staleness — default 3600
}

func (BGPNeighborDown) ID() string              { return "bgp_neighbor_down" }
func (BGPNeighborDown) Severity() string        { return SeverityCritical }
func (BGPNeighborDown) DefaultSeverity() string { return SeverityCritical }
func (BGPNeighborDown) Runbook() string {
	return "Fires when an SNMP-polled BGP peer's most recent state is anything other than " +
		"'established'. Common causes: control-plane restart on the remote, BGP TCP keepalive " +
		"loss, route-policy change rejecting the session, IP reachability loss to the remote, " +
		"or admin shutdown. Cross-check the device's `show ip bgp summary` (or vendor " +
		"equivalent) and the latest poll timestamp in the bgp_peers row."
}
func (r BGPNeighborDown) DefaultParams() map[string]any {
	return map[string]any{
		"established_min_seconds": r.EstablishedMinSeconds,
		"lookback_seconds":        r.LookbackSeconds,
	}
}

func (r BGPNeighborDown) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	return r.EvaluateScoped(ctx, conn, ScopeSelector{}, r.DefaultParams())
}

// EvaluateScoped honors scope.Exporters (which devices to consider)
// and scope.BGPPeers (which peer addresses to consider). ASN-based
// matching (scope.ASNRemote) is supported via a HAVING filter on the
// argMax(peer_asn).
func (r BGPNeighborDown) EvaluateScoped(ctx context.Context, conn driver.Conn, scope ScopeSelector, params map[string]any) ([]Violation, error) {
	dwell := paramInt(params, "established_min_seconds", r.EstablishedMinSeconds)
	if dwell <= 0 {
		dwell = 60
	}
	lookback := paramInt(params, "lookback_seconds", r.LookbackSeconds)
	if lookback <= 0 {
		lookback = 3600
	}
	exporterFrag, exporterArgs := buildExporterWhere("exporter", scope)
	peerFrag, peerArgs := buildBGPPeerWhere("peer_addr", scope)
	asnFrag, asnArgs := buildASNHaving(scope)

	where := "polled_at >= now() - INTERVAL ? SECOND"
	args := []any{uint64(lookback)}
	if exporterFrag != "" {
		where += " AND " + exporterFrag
		args = append(args, exporterArgs...)
	}
	if peerFrag != "" {
		where += " AND " + peerFrag
		args = append(args, peerArgs...)
	}
	having := `state != 'established'
   AND latest <= now() - INTERVAL ? SECOND
   AND latest >= now() - INTERVAL ? SECOND`
	args = append(args, uint64(dwell), uint64(lookback))
	if asnFrag != "" {
		having += " AND " + asnFrag
		args = append(args, asnArgs...)
	}
	q := `
SELECT IPv6NumToString(exporter)             AS exporter_ip,
       IPv6NumToString(peer_addr)            AS peer_ip,
       argMax(peer_asn, polled_at)           AS peer_asn,
       argMax(local_asn, polled_at)          AS local_asn,
       argMax(state, polled_at)              AS state,
       argMax(peer_description, polled_at)   AS peer_description,
       argMax(source, polled_at)             AS source,
       max(polled_at)                        AS latest
FROM bgp_peers
WHERE ` + where + `
GROUP BY exporter, peer_addr
HAVING ` + having
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("bgp_neighbor_down: %w", err)
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var (
			rawExp, rawPeer, state, descr, source string
			peerASN, localASN                     uint32
			latest                                time.Time
		)
		if err := rows.Scan(&rawExp, &rawPeer, &peerASN, &localASN, &state, &descr, &source, &latest); err != nil {
			return nil, err
		}
		exp := unmap4in6(rawExp)
		peer := unmap4in6(rawPeer)
		out = append(out, buildBGPDownViolation(exp, peer, peerASN, localASN, state, descr, source))
	}
	return out, rows.Err()
}

// buildBGPPeerWhere produces the SQL fragment for scope.BGPPeers. The
// IPv6 column comparison goes through IPv6NumToString so v4-mapped
// peers compare cleanly with operator-supplied dotted-quad strings.
func buildBGPPeerWhere(col string, scope ScopeSelector) (string, []any) {
	if len(scope.BGPPeers) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(scope.BGPPeers))
	for _, p := range scope.BGPPeers {
		args = append(args, p)
	}
	placeholders := ""
	for i := range args {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	return fmt.Sprintf("IPv6NumToString(%s) IN (%s)", col, placeholders), args
}

// buildASNHaving produces the HAVING clause fragment for ASN-based
// scope filtering. Returns "" when no ASN filter is set.
func buildASNHaving(scope ScopeSelector) (string, []any) {
	if len(scope.ASNRemote) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(scope.ASNRemote))
	for _, a := range scope.ASNRemote {
		args = append(args, uint64(a))
	}
	placeholders := ""
	for i := range args {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	return fmt.Sprintf("argMax(peer_asn, polled_at) IN (%s)", placeholders), args
}

// buildBGPDownViolation formats the alert. Pulled out so unit tests
// can exercise the copy without ClickHouse.
func buildBGPDownViolation(exporter, peer string, peerASN, localASN uint32, state, descr, source string) Violation {
	label := peer
	if descr != "" {
		label = fmt.Sprintf("%s (%s)", descr, peer)
	}
	scope := fmt.Sprintf("%s · %s", exporter, peer)
	return Violation{
		Scope:    scope,
		GroupKey: "bgp_" + exporter + "_" + peer,
		Title: fmt.Sprintf(
			"BGP %s on %s · peer %s (AS%d) state=%s",
			directionLabel(localASN, peerASN), exporter, label, peerASN, state,
		),
		Body: fmt.Sprintf(
			"BGP session from %s (AS%d) to %s (AS%d) is in state %q. The latest poll observed "+
				"this state via the %s MIB. A session that has been non-established for the "+
				"configured dwell window indicates either a real outage or a flap exceeding "+
				"the dwell threshold — verify on the device.",
			exporter, localASN, peer, peerASN, state, source,
		),
		Severity: SeverityCritical,
		Labels: map[string]string{
			"exporter":  exporter,
			"peer_addr": peer,
			"peer_asn":  fmt.Sprintf("%d", peerASN),
			"local_asn": fmt.Sprintf("%d", localASN),
			"state":     state,
			"source":    source,
		},
	}
}

// directionLabel returns "iBGP" when local and peer ASNs match,
// "eBGP" otherwise. Used in the alert title so the operator can tell
// at a glance whether the session is internal or external — the
// remediation playbooks differ.
func directionLabel(localASN, peerASN uint32) string {
	if localASN != 0 && localASN == peerASN {
		return "iBGP session"
	}
	return "eBGP session"
}
