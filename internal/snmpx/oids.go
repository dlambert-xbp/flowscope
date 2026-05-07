// Package snmpx wraps gosnmp with the small, opinionated walk
// surface FlowScope needs: device inventory (sysDescr / sysName /
// sysObjectID / sysUpTime / sysLocation / sysContact), the standard
// IF-MIB ifTable, and a mock client for development without a lab.
//
// VISION.md §3.1 — SNMP is reserved for fallback enrichment and
// triggered walks. The scheduler in this package walks each bound
// device on a configurable cadence (default 15 min). It does not
// fleet-poll every five minutes.
package snmpx

// Standard SNMP OIDs we read. All scalar OIDs end in `.0` and are
// fetched via Get; tabular OIDs (ifTable columns) are fetched via
// BulkWalk.
const (
	OIDSysDescr    = "1.3.6.1.2.1.1.1.0"
	OIDSysObjectID = "1.3.6.1.2.1.1.2.0"
	OIDSysUpTime   = "1.3.6.1.2.1.1.3.0"
	OIDSysContact  = "1.3.6.1.2.1.1.4.0"
	OIDSysName     = "1.3.6.1.2.1.1.5.0"
	OIDSysLocation = "1.3.6.1.2.1.1.6.0"

	// IF-MIB ifTable columns.
	OIDIfDescr        = "1.3.6.1.2.1.2.2.1.2"
	OIDIfType         = "1.3.6.1.2.1.2.2.1.3"
	OIDIfMtu          = "1.3.6.1.2.1.2.2.1.4"
	OIDIfSpeed        = "1.3.6.1.2.1.2.2.1.5"  // 32-bit; saturates at ~4 Gbps
	OIDIfAdminStatus  = "1.3.6.1.2.1.2.2.1.7"
	OIDIfOperStatus   = "1.3.6.1.2.1.2.2.1.8"
	OIDIfInErrors     = "1.3.6.1.2.1.2.2.1.14"
	OIDIfOutErrors    = "1.3.6.1.2.1.2.2.1.20"
	OIDIfInDiscards   = "1.3.6.1.2.1.2.2.1.13"
	OIDIfOutDiscards  = "1.3.6.1.2.1.2.2.1.19"

	// IF-MIB ifXTable extensions (use these where available).
	OIDIfHighSpeed = "1.3.6.1.2.1.31.1.1.1.15" // Mbps; preferred over ifSpeed
	OIDIfAlias     = "1.3.6.1.2.1.31.1.1.1.18" // operator description
)

// ifAdminStatusName / ifOperStatusName render IF-MIB integer codes
// into the human-friendly strings the api stores.
func IfAdminStatusName(v int) string {
	switch v {
	case 1:
		return "up"
	case 2:
		return "down"
	case 3:
		return "testing"
	default:
		return "unknown"
	}
}

func IfOperStatusName(v int) string {
	switch v {
	case 1:
		return "up"
	case 2:
		return "down"
	case 3:
		return "testing"
	case 4:
		return "unknown"
	case 5:
		return "dormant"
	case 6:
		return "notPresent"
	case 7:
		return "lowerLayerDown"
	default:
		return "unknown"
	}
}
