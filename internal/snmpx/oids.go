// Package snmpx wraps gosnmp with the small, opinionated walk
// surface FlowScope needs: device inventory (sysDescr / sysName /
// sysObjectID / sysUpTime / sysLocation / sysContact), the standard
// IF-MIB ifTable, and a mock client for development without a lab.
//
// VISION.md §3.1 — SNMP is reserved for fallback enrichment and
// triggered walks. The scheduler in this package walks each bound
// device on a configurable cadence (default 60s, per-credential
// override via interval_sec). It does not fleet-poll every five
// minutes; bounded concurrency + per-device in-flight dedup keeps a
// slow target from stacking work on the pool.
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

	// HOST-RESOURCES-MIB — generic CPU + memory + storage. Works on
	// Linux/BSD and a subset of enterprise switches; commonly partial
	// or missing on network gear (which is why we also fall back to
	// vendor MIBs below).
	//
	// hrProcessorTable is indexed by hrDeviceIndex; hrProcessorLoad is
	// an integer 0–100 averaged over the last minute per CPU.
	OIDHrDeviceDescr   = "1.3.6.1.2.1.25.3.2.1.3"  // human label for the device row
	OIDHrProcessorLoad = "1.3.6.1.2.1.25.3.3.1.2"  // 0–100 per CPU
	// hrStorageTable carries one entry per logical storage (RAM, swap,
	// each mounted disk). hrStorageType discriminates RAM vs disk so
	// we can route to the "memory" or "storage" kind. AllocationUnits
	// (bytes per slot) * Size (slots) = total bytes; * Used (slots) =
	// used bytes.
	OIDHrStorageType            = "1.3.6.1.2.1.25.2.3.1.2" // OID; .2 = RAM, .4 = fixed disk, .3 = virtual, etc.
	OIDHrStorageDescr           = "1.3.6.1.2.1.25.2.3.1.3"
	OIDHrStorageAllocationUnits = "1.3.6.1.2.1.25.2.3.1.4"
	OIDHrStorageSize            = "1.3.6.1.2.1.25.2.3.1.5"
	OIDHrStorageUsed            = "1.3.6.1.2.1.25.2.3.1.6"

	// hrStorageType discriminator values (last digit of the OID
	// hrStorageType returns). RAM = 2, virtual memory = 3, fixed disk = 4,
	// removable disk = 5, network disk = 6, network alloc = 7,
	// flash memory = 8 (rare).
	OIDHrStorageRAM        = "1.3.6.1.2.1.25.2.1.2"
	OIDHrStorageVirtualMem = "1.3.6.1.2.1.25.2.1.3"
	OIDHrStorageFixedDisk  = "1.3.6.1.2.1.25.2.1.4"

	// CISCO-PROCESS-MIB — Cisco-specific CPU. Better than HRMIB on
	// IOS/IOS-XE because it surfaces per-process-context CPU averaged
	// over 5sec/1min/5min. We grab the 5-minute average (cpmCPUTotal5minRev)
	// keyed by cpmCPUTotalIndex (an arbitrary integer).
	OIDCpmCPUTotal5minRev = "1.3.6.1.4.1.9.9.109.1.1.1.1.8"
	OIDCpmCPUTotal1minRev = "1.3.6.1.4.1.9.9.109.1.1.1.1.7"
	// cpmCPUTotalPhysicalIndex points back into entPhysicalTable for
	// the human label (e.g. "Switch1 Cpu of Module 1"). We resolve it
	// when present, falling back to "CPU N" with the table index.
	OIDCpmCPUTotalPhysicalIndex = "1.3.6.1.4.1.9.9.109.1.1.1.1.2"

	// CISCO-MEMORY-POOL-MIB — Cisco-specific memory by named pool
	// ("Processor", "I/O", etc.). Used + Free in bytes, name string.
	OIDCiscoMemoryPoolName = "1.3.6.1.4.1.9.9.48.1.1.1.2"
	OIDCiscoMemoryPoolUsed = "1.3.6.1.4.1.9.9.48.1.1.1.5"
	OIDCiscoMemoryPoolFree = "1.3.6.1.4.1.9.9.48.1.1.1.6"

	// ENTITY-MIB — used to resolve the cpmCPUTotalPhysicalIndex to a
	// human-readable component name. Also drives the PSU walk below
	// when entPhysicalClass = 6 (powerSupply).
	OIDEntPhysicalName        = "1.3.6.1.2.1.47.1.1.1.1.7"
	OIDEntPhysicalClass       = "1.3.6.1.2.1.47.1.1.1.1.5"
	OIDEntPhysicalDescr       = "1.3.6.1.2.1.47.1.1.1.1.2"

	// ENTITY-SENSOR-MIB (RFC 3433) — standardised per-sensor readings
	// for temperature, fan speed, voltage, current, watts on most
	// vendors. Sensors are indexed by entPhysicalIndex (same table as
	// entPhysicalName), so the same physIndex → human-name map covers
	// labels for sensors and for CPU rows.
	OIDEntPhySensorType         = "1.3.6.1.2.1.99.1.1.1.1" // enum: 8=celsius, 9=truthvalue, 10=rpm, 4=volts, 5=amps, 6=watts, ...
	OIDEntPhySensorScale        = "1.3.6.1.2.1.99.1.1.1.2" // enum: 9=units, 8=milli, 7=micro, ...
	OIDEntPhySensorPrecision    = "1.3.6.1.2.1.99.1.1.1.3" // integer: digits after the implied decimal
	OIDEntPhySensorValue        = "1.3.6.1.2.1.99.1.1.1.4" // INT32 raw reading
	OIDEntPhySensorOperStatus   = "1.3.6.1.2.1.99.1.1.1.5" // enum: 1=ok, 2=unavailable, 3=nonoperational
	OIDEntPhySensorUnitsDisplay = "1.3.6.1.2.1.99.1.1.1.6" // operator-readable unit string when present

	// CISCO-ENTITY-FRU-CONTROL-MIB — per-PSU operational status.
	// cefcFRUPowerOperStatus values: 1=offEnvOther, 2=on, 3=offAdmin,
	// 4=offDenied, 5=offEnvPower, 6=offEnvTemp, 7=offEnvFan, 8=failed,
	// 9=onButFanFail, 10=offCooling, 11=offConnectorRating, 12=onButInlinePowerFail.
	// Indexed by entPhysicalIndex, so the entPhysicalName map labels
	// the row.
	OIDCefcFRUPowerOperStatus = "1.3.6.1.4.1.9.9.117.1.1.2.1.2"

	// LLDP-MIB (IEEE 802.1AB) — primary vendor-neutral neighbor
	// discovery. Tables are indexed by (lldpRemTimeMark,
	// lldpRemLocalPortNum, lldpRemIndex). lldpRemLocalPortNum is
	// the local IF-MIB ifIndex; the other two columns disambiguate
	// repeated neighbors per port.
	//
	// lldpRemChassisIdSubtype tells the parser how to interpret the
	// raw bytes in lldpRemChassisId:
	//   1=chassisComponent (string)
	//   2=interfaceAlias (string)
	//   3=portComponent (string)
	//   4=macAddress       (6 raw bytes → colon-separated hex)
	//   5=networkAddress   (1-byte AF + N raw bytes → IP string)
	//   6=interfaceName    (string)
	//   7=local            (string)
	OIDLldpRemChassisIdSubtype = "1.0.8802.1.1.2.1.4.1.1.4"
	OIDLldpRemChassisID        = "1.0.8802.1.1.2.1.4.1.1.5"
	OIDLldpRemPortIDSubtype    = "1.0.8802.1.1.2.1.4.1.1.6"
	OIDLldpRemPortID           = "1.0.8802.1.1.2.1.4.1.1.7"
	OIDLldpRemPortDesc         = "1.0.8802.1.1.2.1.4.1.1.8"
	OIDLldpRemSysName          = "1.0.8802.1.1.2.1.4.1.1.9"
	OIDLldpRemSysDesc          = "1.0.8802.1.1.2.1.4.1.1.10"
	OIDLldpRemSysCapEnabled    = "1.0.8802.1.1.2.1.4.1.1.12"
	// lldpRemManAddrTable is indexed by the same three keys plus
	// (lldpRemManAddrSubtype, lldpRemManAddr). We only care that
	// the address exists — the subtype 1=ipv4 / 2=ipv6 is encoded in
	// the index so we can decode without a separate column fetch.
	OIDLldpRemManAddrIfSubtype = "1.0.8802.1.1.2.1.4.2.1.3"

	// CISCO-CDP-MIB — Cisco fallback for devices that don't speak
	// LLDP (older IOS, some pre-Catalyst 9k). Tables are indexed by
	// (cdpCacheIfIndex, cdpCacheDeviceIndex). cdpCacheIfIndex IS the
	// local IF-MIB ifIndex, so we skip the ifTable lookup that LLDP
	// requires. cdpCacheCapabilities is a 4-byte bitmap (router /
	// bridge / source-route bridge / switch / host / IGMP / repeater /
	// phone / remote / AP-CAPWAP / two-port-mac-relay / STA-only).
	OIDCdpCacheDeviceID     = "1.3.6.1.4.1.9.9.23.1.2.1.1.6"
	OIDCdpCacheDevicePort   = "1.3.6.1.4.1.9.9.23.1.2.1.1.7"
	OIDCdpCachePlatform     = "1.3.6.1.4.1.9.9.23.1.2.1.1.8"
	OIDCdpCacheCapabilities = "1.3.6.1.4.1.9.9.23.1.2.1.1.9"
	OIDCdpCacheVersion      = "1.3.6.1.4.1.9.9.23.1.2.1.1.5"
	// cdpCacheAddress is the management IP, encoded as raw bytes with
	// cdpCacheAddressType (1=IP, 2=DECNET, ...) as the protocol hint.
	// We accept only address-type 1 (IPv4) and infer IPv6 from a 16-
	// byte length.
	OIDCdpCacheAddressType = "1.3.6.1.4.1.9.9.23.1.2.1.1.3"
	OIDCdpCacheAddress     = "1.3.6.1.4.1.9.9.23.1.2.1.1.4"

	// BGP4-MIB (RFC 4273) — IPv4-only, ASN limited to 16 bits. The
	// table is indexed by bgpPeerRemoteAddr (the IPv4 peer address).
	// bgpPeerState values: 1=idle, 2=connect, 3=active, 4=opensent,
	// 5=openconfirm, 6=established. bgpPeerAdminStatus: 1=stop, 2=start.
	// bgpPeerFsmEstablishedTime is in seconds since the peer last
	// transitioned to Established (TimeStamp). Use this MIB only when
	// the richer cbgpPeer2 / jnxBgpM2 paths return nothing.
	OIDBgpPeerIdentifier            = "1.3.6.1.2.1.15.3.1.1"  // IpAddress: peer router id
	OIDBgpPeerState                 = "1.3.6.1.2.1.15.3.1.2"  // INTEGER: 1..6
	OIDBgpPeerAdminStatus           = "1.3.6.1.2.1.15.3.1.3"  // INTEGER: 1=stop, 2=start
	OIDBgpPeerLocalAddr             = "1.3.6.1.2.1.15.3.1.5"  // IpAddress
	OIDBgpPeerLocalAS               = "1.3.6.1.2.1.15.2"      // scalar bgpLocalAs
	OIDBgpPeerRemoteAddr            = "1.3.6.1.2.1.15.3.1.7"  // IpAddress; also the index
	OIDBgpPeerRemoteAS              = "1.3.6.1.2.1.15.3.1.9"  // INTEGER (16-bit only in this MIB)
	OIDBgpPeerFsmEstablishedTime    = "1.3.6.1.2.1.15.3.1.16" // Counter32 (seconds since last Established)
	OIDBgpPeerInUpdateElapsedTime   = "1.3.6.1.2.1.15.3.1.24" // Counter32 (seconds since last update)

	// CISCO-BGP4-MIB cbgpPeer3Table — Cisco's VRF-aware BGP table.
	// Available on IOS 12.4+, IOS-XR, IOS-XE, and most NX-OS releases.
	// Indexed by:
	//   cbgpPeer3VrfName          SnmpAdminString  (length-prefixed in OID)
	//   cbgpPeer3Type             InetAddressType  (1=ipv4, 2=ipv6, 3=ipv4z, 4=ipv6z)
	//   cbgpPeer3RemoteAddr       InetAddress      (length-prefixed in OID)
	//
	// State / AdminStatus enums match BGP4-MIB. RemoteAs / LocalAs are
	// Unsigned32 so 4-byte ASNs travel cleanly. FsmEstablishedTime is
	// Gauge32 seconds since the last transition INTO Established;
	// InUpdateElapsedTime is Gauge32 seconds since the last update.
	//
	// cbgpPeer2Table (under .1.2.5) predates this and lacks VRF; we
	// skip it entirely — anything that runs cbgpPeer2 also runs
	// cbgpPeer3, and cbgpPeer3 with vrf="default" gives identical
	// global-table data.
	OIDCbgpPeer3State               = "1.3.6.1.4.1.9.9.187.1.2.9.1.3"
	OIDCbgpPeer3AdminStatus         = "1.3.6.1.4.1.9.9.187.1.2.9.1.4"
	OIDCbgpPeer3LocalAs             = "1.3.6.1.4.1.9.9.187.1.2.9.1.8"
	OIDCbgpPeer3RemoteAs            = "1.3.6.1.4.1.9.9.187.1.2.9.1.11"
	OIDCbgpPeer3FsmEstablishedTime  = "1.3.6.1.4.1.9.9.187.1.2.9.1.19"
	OIDCbgpPeer3InUpdateElapsedTime = "1.3.6.1.4.1.9.9.187.1.2.9.1.27"

	// ARISTA-BGP4V2-MIB — Arista's IETF-BGP4V2-MIB-derived BGP table.
	// Native on EOS; the only standard MIB Arista exposes that
	// surfaces non-default routing instances. OID arc:
	//
	//   arista                 = 1.3.6.1.4.1.30065
	//   aristaExperiment       = arista.4
	//   aristaBgp4V2           = aristaExperiment.1            (.30065.4.1)
	//   aristaBgp4V2Objects    = aristaBgp4V2.1                (.30065.4.1.1)
	//   aristaBgp4V2PeerTable  = aristaBgp4V2Objects.2         (.30065.4.1.1.2)
	//   aristaBgp4V2PeerEntry  = aristaBgp4V2PeerTable.1       (.30065.4.1.1.2.1)
	//   aristaBgp4V2PeerEventTimesTable  = aristaBgp4V2Objects.4 (.30065.4.1.1.4)
	//   aristaBgp4V2PeerEventTimesEntry  = ...4.1
	//
	// Index for the peer entry: (PeerInstance, RemoteAddrType,
	// RemoteAddr). PeerInstance is Unsigned32 — one sub-OID. The
	// RemoteAddr is length-prefixed (InetAddress encoding: length
	// byte + N address bytes). Per the MIB the instance number is
	// "1 for single-instance impls"; vendors that support multi-VRF
	// number them sequentially. ARISTA-BGP4V2-MIB does NOT carry a
	// VRF-name lookup, so the walker renders instance==1 as
	// "default" and others as "vrf-<N>".
	OIDAristaBgp4V2PeerLocalAs        = "1.3.6.1.4.1.30065.4.1.1.2.1.7"
	OIDAristaBgp4V2PeerRemoteAs       = "1.3.6.1.4.1.30065.4.1.1.2.1.10"
	OIDAristaBgp4V2PeerAdminStatus    = "1.3.6.1.4.1.30065.4.1.1.2.1.12"
	OIDAristaBgp4V2PeerState          = "1.3.6.1.4.1.30065.4.1.1.2.1.13"
	OIDAristaBgp4V2PeerDescription    = "1.3.6.1.4.1.30065.4.1.1.2.1.14"
	OIDAristaBgp4V2PeerFsmEstTime     = "1.3.6.1.4.1.30065.4.1.1.4.1.1"
	OIDAristaBgp4V2PeerInUpdElapsed   = "1.3.6.1.4.1.30065.4.1.1.4.1.2"

	// ARISTA-VRF-MIB — operator-configured VRF inventory. Used by
	// the BGP walker to discover the list of VRFs on the device,
	// then walk RFC 4273 BGP4-MIB once per VRF using SNMP context
	// addressing (`community@vrfname` for v2c, contextName for v3).
	// EOS exposes BGP per VRF via per-context BGP4-MIB views — no
	// vendor-specific BGP MIB carries the correlation, but every VRF
	// has its own SNMP context with a complete BGP4-MIB.
	//
	//   arista          = 1.3.6.1.4.1.30065
	//   aristaMibs      = arista.3
	//   aristaVrfMIB    = aristaMibs.18                 (.30065.3.18)
	//   aristaVrfMibObjects = aristaVrfMIB.1            (.30065.3.18.1)
	//   aristaVrfTable  = aristaVrfMibObjects.1         (.30065.3.18.1.1)
	//   aristaVrfEntry  = aristaVrfTable.1              (.30065.3.18.1.1.1)
	//
	// aristaVrfEntry columns:
	//   .1 aristaVrfName            (index, SnmpAdminString)
	//   .2 aristaVrfRoutingStatus
	//   .3 aristaVrfRouteDistinguisher
	//   .4 aristaVrfState
	//
	// We only need the row keys (the VRF names) — those are the
	// suffixes of any column under the entry. Walking column 4
	// (RoutingStatus) is cheap and gives us the index per row.
	OIDAristaVrfRoutingStatus = "1.3.6.1.4.1.30065.3.18.1.1.1.2"

	// JUNIPER-BGP4-V2-MIB jnxBgpM2PeerTable — Junos's modern BGP MIB.
	// Indexed by jnxBgpM2PeerInstance + jnxBgpM2PeerLocalAddrType +
	// jnxBgpM2PeerLocalAddr + jnxBgpM2PeerRemoteAddrType +
	// jnxBgpM2PeerRemoteAddr. jnxBgpM2PeerState matches the BGP4-MIB
	// state enum.
	OIDJnxBgpM2PeerState              = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.2"
	OIDJnxBgpM2PeerStatus             = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.3" // bgpAdminStatus equivalent
	OIDJnxBgpM2PeerLocalAs            = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.9"
	OIDJnxBgpM2PeerRemoteAs           = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.13"
	OIDJnxBgpM2PeerFsmEstablishedTime = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.16"
	OIDJnxBgpM2PeerInUpdateElapsedTime = "1.3.6.1.4.1.2636.5.1.1.2.1.1.1.17"
	OIDJnxBgpM2PeerDescription        = "1.3.6.1.4.1.2636.5.1.1.2.6.1.1.2"
)

// BgpPeerStateName maps the BGP4-MIB INTEGER enum to canonical string
// names used by the bgp_peers table and the BGPNeighborDown rule.
func BgpPeerStateName(v int) string {
	switch v {
	case 1:
		return "idle"
	case 2:
		return "connect"
	case 3:
		return "active"
	case 4:
		return "opensent"
	case 5:
		return "openconfirm"
	case 6:
		return "established"
	default:
		return "unknown"
	}
}

// BgpAdminStatusName renders the bgpPeerAdminStatus INTEGER enum.
func BgpAdminStatusName(v int) string {
	switch v {
	case 1:
		return "stop"
	case 2:
		return "start"
	default:
		return ""
	}
}

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
