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
