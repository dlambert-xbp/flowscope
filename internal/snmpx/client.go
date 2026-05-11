package snmpx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Inventory captures one snapshot of an exporter's SNMP state. It is
// what Walk returns and what cmd/snmp persists into ClickHouse.
type Inventory struct {
	PolledAt       time.Time
	Exporter       string
	SysDescr       string
	SysObjectID    string
	SysUpTimeMs    uint64
	SysName        string
	SysContact     string
	SysLocation    string
	Interfaces     []Interface
	Resources      []ResourceSample
	PollDurationMs uint32
	Status         string // ok | partial | error
}

// ResourceKind enumerates the metric families surfaced on the Devices
// tab. Keep in lockstep with the LowCardinality(String) `kind` column
// on device_resource_samples.
type ResourceKind string

const (
	ResourceKindCPU         ResourceKind = "cpu"
	ResourceKindMemory      ResourceKind = "memory"
	ResourceKindStorage     ResourceKind = "storage"
	ResourceKindTemperature ResourceKind = "temperature"
	ResourceKindFan         ResourceKind = "fan"
)

// ResourceSource identifies the MIB the sample came from. Mirrors the
// LowCardinality(String) `source` column on device_resource_samples
// and helps the UI explain where a value came from (and lets us pick
// a vendor reading over a generic HRMIB one when both are present).
type ResourceSource string

const (
	ResourceSourceHRMIB           ResourceSource = "hrmib"
	ResourceSourceCiscoProcess    ResourceSource = "cisco-process"
	ResourceSourceCiscoMempool    ResourceSource = "cisco-mempool"
	ResourceSourceCiscoEnhMempool ResourceSource = "cisco-enhmempool"
	ResourceSourceJuniperJnx      ResourceSource = "juniper-jnx"
	ResourceSourceAristaEntity    ResourceSource = "arista-entity"
)

// ResourceSample is one row of device_resource_samples. Use whichever
// of value_percent / value_bytes / max_bytes makes sense for the kind:
//   - cpu     → ValuePercent only
//   - memory  → ValueBytes + MaxBytes (UI derives percent)
//   - storage → ValueBytes + MaxBytes
//   - temp    → ValuePercent (carries °C; column is overloaded)
//   - fan     → ValuePercent (carries RPM)
type ResourceSample struct {
	Kind         ResourceKind
	Component    string // human-readable, e.g. "Processor 1", "Pool: Processor"
	ValuePercent float32
	ValueBytes   uint64
	MaxBytes     uint64
	Source       ResourceSource
}

// Interface mirrors the columns of device_snmp_interfaces. Counter
// values come from the IF-MIB ifTable; rate-of-change is computed
// elsewhere by diffing successive snapshots.
type Interface struct {
	IfIndex      uint32
	IfDescr      string
	IfAlias      string
	IfType       uint32
	IfSpeedBps   uint64
	IfMtu        uint32
	AdminStatus  string
	OperStatus   string
	InErrors     uint64
	OutErrors    uint64
	InDiscards   uint64
	OutDiscards  uint64
}

// Client is the operations FlowScope needs from any SNMP backend.
// Implemented by the gosnmp-backed RealClient and by MockClient.
type Client interface {
	Walk(ctx context.Context, target string) (*Inventory, error)
}

// Config configures a real (non-mock) SNMP client. It carries either
// a v2c community OR a v3 user + auth/priv set. Use FromCredential to
// build a Config from a stored Credential (decrypted by the caller).
type Config struct {
	// Version: "v2c" or "v3". Defaults to "v2c" when empty.
	Version string
	// Port defaults to 161.
	Port uint16
	// Timeout per request. Default 2s.
	Timeout time.Duration
	// Retries per request. Default 1.
	Retries int

	// v2c
	Community string

	// v3
	V3Username  string
	V3AuthProto string // '' | MD5 | SHA | SHA-224 | SHA-256 | SHA-384 | SHA-512
	V3AuthPass  string
	V3PrivProto string // '' | DES | AES | AES-192 | AES-256
	V3PrivPass  string
	V3Context   string
}

func (c *Config) defaults() {
	if c.Version == "" {
		c.Version = "v2c"
	}
	if c.Version == "v2c" && c.Community == "" {
		c.Community = "public"
	}
	if c.Port == 0 {
		c.Port = 161
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.Retries < 0 {
		c.Retries = 1
	}
}

// FromCredential builds a Config from a (decrypted) Credential. Used
// by the scheduler at walk time and by the api's /test endpoint.
func FromCredential(c *Credential) Config {
	if c == nil {
		return Config{}
	}
	return Config{
		Version:     c.Version,
		Port:        c.Port,
		Community:   c.Community,
		V3Username:  c.V3Username,
		V3AuthProto: c.V3AuthProto,
		V3AuthPass:  c.V3AuthPass,
		V3PrivProto: c.V3PrivProto,
		V3PrivPass:  c.V3PrivPass,
		V3Context:   c.V3Context,
	}
}

// RealClient wraps gosnmp.GoSNMP for a single (target, credential)
// pair. Each Walk call opens a fresh session — gosnmp is not
// goroutine-safe, and per-target sessions keep failures isolated.
type RealClient struct {
	cfg Config
}

// NewClient returns a real SNMP client configured by c.
func NewClient(c Config) *RealClient {
	c.defaults()
	return &RealClient{cfg: c}
}

// Walk fetches the standard inventory snapshot for target.
func (rc *RealClient) Walk(ctx context.Context, target string) (*Inventory, error) {
	start := time.Now()

	g, err := buildGoSNMP(target, rc.cfg, ctx)
	if err != nil {
		return nil, err
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s: %w", target, err)
	}
	defer g.Conn.Close()

	inv := &Inventory{
		PolledAt: time.Now().UTC(),
		Exporter: target,
		Status:   "ok",
	}

	// Scalar gets — pack into one Get for efficiency.
	scalars := []string{
		OIDSysDescr, OIDSysObjectID, OIDSysUpTime,
		OIDSysContact, OIDSysName, OIDSysLocation,
	}
	res, err := g.Get(scalars)
	if err != nil {
		// Inventory is mandatory; without sys* there is nothing useful
		// to write. Treat as a hard failure.
		return nil, fmt.Errorf("snmp get %s: %w", target, err)
	}
	for _, v := range res.Variables {
		switch v.Name {
		case "." + OIDSysDescr:
			inv.SysDescr = octetString(v)
		case "." + OIDSysObjectID:
			inv.SysObjectID = oidString(v)
		case "." + OIDSysUpTime:
			inv.SysUpTimeMs = uint64(timeticksToMs(v))
		case "." + OIDSysContact:
			inv.SysContact = octetString(v)
		case "." + OIDSysName:
			inv.SysName = octetString(v)
		case "." + OIDSysLocation:
			inv.SysLocation = octetString(v)
		}
	}

	// ifTable / ifXTable bulk walks. Failures here are non-fatal —
	// a device that answers sys* but not ifTable still produces a
	// useful row, and we mark Status accordingly.
	ifaces, err := walkInterfaces(g)
	if err != nil {
		inv.Status = "partial"
	}
	inv.Interfaces = ifaces

	// Resource walks (CPU / memory / storage). All MIBs are optional —
	// HRMIB on a switch that doesn't implement it just returns nothing,
	// and CISCO-PROCESS-MIB on a non-Cisco device 404s. Each helper
	// returns an empty slice on error so a missing MIB never demotes
	// the walk to "partial".
	inv.Resources = walkResources(g)

	inv.PollDurationMs = uint32(time.Since(start).Milliseconds())
	return inv, nil
}

// walkResources fans out across HOST-RESOURCES-MIB and a small set of
// vendor MIBs (Cisco classic today; Juniper / Arista are stubs). Each
// branch is independent — any one MIB missing on the target is fine,
// the operator just sees fewer rows on the resources tile. Errors are
// swallowed and logged via the gosnmp client's own logging since this
// is enrichment, not a hard requirement.
func walkResources(g *gosnmp.GoSNMP) []ResourceSample {
	out := make([]ResourceSample, 0, 8)
	out = append(out, walkHRMIB(g)...)
	out = append(out, walkCiscoCPU(g)...)
	out = append(out, walkCiscoMemory(g)...)
	return out
}

// walkHRMIB pulls CPU load per processor (hrProcessorLoad) and the
// hrStorage table for memory + storage breakdown. Device labels come
// from hrDeviceDescr for CPUs and hrStorageDescr for storage entries.
// hrStorageType discriminates RAM (.2 / .3) vs disk (.4 / .5 / .7) —
// RAM rows land under "memory", disks under "storage".
func walkHRMIB(g *gosnmp.GoSNMP) []ResourceSample {
	out := make([]ResourceSample, 0, 4)

	// CPU: hrProcessorLoad indexed by hrDeviceIndex. Resolve the label
	// from hrDeviceDescr for the same index.
	descrByIdx := map[uint32]string{}
	_ = g.BulkWalk(OIDHrDeviceDescr, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrDeviceDescr); ok {
			descrByIdx[idx] = octetString(pdu)
		}
		return nil
	})
	_ = g.BulkWalk(OIDHrProcessorLoad, func(pdu gosnmp.SnmpPDU) error {
		idx, ok := indexFromOID(pdu.Name, OIDHrProcessorLoad)
		if !ok {
			return nil
		}
		load := float32(integerValue(pdu))
		comp := descrByIdx[idx]
		if comp == "" {
			comp = fmt.Sprintf("CPU %d", idx)
		}
		out = append(out, ResourceSample{
			Kind:         ResourceKindCPU,
			Component:    comp,
			ValuePercent: load,
			Source:       ResourceSourceHRMIB,
		})
		return nil
	})

	// Storage: collect type / descr / alloc / size / used per index,
	// classify, emit.
	type stor struct {
		descr string
		typ   string
		alloc uint64
		size  uint64
		used  uint64
	}
	st := map[uint32]*stor{}
	ensureSt := func(i uint32) *stor {
		if s, ok := st[i]; ok {
			return s
		}
		s := &stor{}
		st[i] = s
		return s
	}
	_ = g.BulkWalk(OIDHrStorageType, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrStorageType); ok {
			ensureSt(idx).typ = oidString(pdu)
		}
		return nil
	})
	_ = g.BulkWalk(OIDHrStorageDescr, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrStorageDescr); ok {
			ensureSt(idx).descr = octetString(pdu)
		}
		return nil
	})
	_ = g.BulkWalk(OIDHrStorageAllocationUnits, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrStorageAllocationUnits); ok {
			ensureSt(idx).alloc = uint64(integerValue(pdu))
		}
		return nil
	})
	_ = g.BulkWalk(OIDHrStorageSize, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrStorageSize); ok {
			ensureSt(idx).size = uint64(integerValue(pdu))
		}
		return nil
	})
	_ = g.BulkWalk(OIDHrStorageUsed, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDHrStorageUsed); ok {
			ensureSt(idx).used = uint64(integerValue(pdu))
		}
		return nil
	})
	for _, s := range st {
		if s.alloc == 0 || s.size == 0 {
			continue
		}
		totalBytes := s.alloc * s.size
		usedBytes := s.alloc * s.used
		var pct float32
		if totalBytes > 0 {
			pct = float32(float64(usedBytes) / float64(totalBytes) * 100)
		}
		kind := classifyHRStorage(s.typ)
		if kind == "" {
			continue
		}
		out = append(out, ResourceSample{
			Kind:         kind,
			Component:    s.descr,
			ValuePercent: pct,
			ValueBytes:   usedBytes,
			MaxBytes:     totalBytes,
			Source:       ResourceSourceHRMIB,
		})
	}
	return out
}

// classifyHRStorage maps an hrStorageType OID to a ResourceKind, or
// returns "" for types we deliberately skip (virtual memory, network
// disk, etc. — useful in theory, noisy on dashboards in practice).
func classifyHRStorage(typeOID string) ResourceKind {
	// Strip a leading dot if gosnmp included one.
	typeOID = strings.TrimPrefix(typeOID, ".")
	switch typeOID {
	case OIDHrStorageRAM:
		return ResourceKindMemory
	case OIDHrStorageFixedDisk:
		return ResourceKindStorage
	default:
		return ""
	}
}

// walkCiscoCPU pulls cpmCPUTotal5minRev (5-min CPU utilization, the
// most stable read) and resolves a human component name via the
// cpmCPUTotalPhysicalIndex pointer back into entPhysicalName.
func walkCiscoCPU(g *gosnmp.GoSNMP) []ResourceSample {
	// Index → physical index ptr.
	physIdx := map[uint32]uint32{}
	_ = g.BulkWalk(OIDCpmCPUTotalPhysicalIndex, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDCpmCPUTotalPhysicalIndex); ok {
			physIdx[idx] = uint32(integerValue(pdu))
		}
		return nil
	})
	// physIndex → human name.
	physName := map[uint32]string{}
	if len(physIdx) > 0 {
		_ = g.BulkWalk(OIDEntPhysicalName, func(pdu gosnmp.SnmpPDU) error {
			if idx, ok := indexFromOID(pdu.Name, OIDEntPhysicalName); ok {
				physName[idx] = octetString(pdu)
			}
			return nil
		})
	}
	out := make([]ResourceSample, 0, 2)
	_ = g.BulkWalk(OIDCpmCPUTotal5minRev, func(pdu gosnmp.SnmpPDU) error {
		idx, ok := indexFromOID(pdu.Name, OIDCpmCPUTotal5minRev)
		if !ok {
			return nil
		}
		comp := physName[physIdx[idx]]
		if comp == "" {
			comp = fmt.Sprintf("CPU %d", idx)
		}
		out = append(out, ResourceSample{
			Kind:         ResourceKindCPU,
			Component:    comp,
			ValuePercent: float32(integerValue(pdu)),
			Source:       ResourceSourceCiscoProcess,
		})
		return nil
	})
	return out
}

// walkCiscoMemory pulls each named memory pool's used + free bytes.
// Pool name comes from ciscoMemoryPoolName; total bytes = used + free.
func walkCiscoMemory(g *gosnmp.GoSNMP) []ResourceSample {
	type pool struct {
		name string
		used uint64
		free uint64
	}
	pools := map[uint32]*pool{}
	ensure := func(i uint32) *pool {
		if p, ok := pools[i]; ok {
			return p
		}
		p := &pool{}
		pools[i] = p
		return p
	}
	_ = g.BulkWalk(OIDCiscoMemoryPoolName, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDCiscoMemoryPoolName); ok {
			ensure(idx).name = octetString(pdu)
		}
		return nil
	})
	_ = g.BulkWalk(OIDCiscoMemoryPoolUsed, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDCiscoMemoryPoolUsed); ok {
			ensure(idx).used = uint64(integerValue(pdu))
		}
		return nil
	})
	_ = g.BulkWalk(OIDCiscoMemoryPoolFree, func(pdu gosnmp.SnmpPDU) error {
		if idx, ok := indexFromOID(pdu.Name, OIDCiscoMemoryPoolFree); ok {
			ensure(idx).free = uint64(integerValue(pdu))
		}
		return nil
	})
	out := make([]ResourceSample, 0, len(pools))
	for _, p := range pools {
		total := p.used + p.free
		if total == 0 {
			continue
		}
		pct := float32(float64(p.used) / float64(total) * 100)
		name := p.name
		if name == "" {
			name = "Pool"
		}
		out = append(out, ResourceSample{
			Kind:         ResourceKindMemory,
			Component:    "Pool: " + name,
			ValuePercent: pct,
			ValueBytes:   p.used,
			MaxBytes:     total,
			Source:       ResourceSourceCiscoMempool,
		})
	}
	return out
}

// walkInterfaces fetches every column we care about from ifTable +
// ifXTable, indexed by ifIndex.
func walkInterfaces(g *gosnmp.GoSNMP) ([]Interface, error) {
	cols := []struct {
		oid string
		fn  func(byIndex map[uint32]*Interface, idx uint32, pdu gosnmp.SnmpPDU)
	}{
		{OIDIfDescr, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).IfDescr = octetString(p) }},
		{OIDIfType, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).IfType = uint32(integerValue(p)) }},
		{OIDIfMtu, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).IfMtu = uint32(integerValue(p)) }},
		{OIDIfSpeed, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) {
			s := uint64(integerValue(p))
			// Only set if ifHighSpeed didn't already populate.
			if e := ensure(m, i); e.IfSpeedBps == 0 {
				e.IfSpeedBps = s
			}
		}},
		{OIDIfHighSpeed, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) {
			ensure(m, i).IfSpeedBps = uint64(integerValue(p)) * 1_000_000
		}},
		{OIDIfAlias, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).IfAlias = octetString(p) }},
		{OIDIfAdminStatus, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) {
			ensure(m, i).AdminStatus = IfAdminStatusName(integerValue(p))
		}},
		{OIDIfOperStatus, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) {
			ensure(m, i).OperStatus = IfOperStatusName(integerValue(p))
		}},
		{OIDIfInErrors, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).InErrors = uint64(integerValue(p)) }},
		{OIDIfOutErrors, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).OutErrors = uint64(integerValue(p)) }},
		{OIDIfInDiscards, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).InDiscards = uint64(integerValue(p)) }},
		{OIDIfOutDiscards, func(m map[uint32]*Interface, i uint32, p gosnmp.SnmpPDU) { ensure(m, i).OutDiscards = uint64(integerValue(p)) }},
	}

	byIndex := make(map[uint32]*Interface)
	for _, c := range cols {
		err := g.BulkWalk(c.oid, func(pdu gosnmp.SnmpPDU) error {
			idx, ok := indexFromOID(pdu.Name, c.oid)
			if !ok {
				return nil
			}
			c.fn(byIndex, idx, pdu)
			return nil
		})
		if err != nil {
			// One column failed — keep what we have, mark partial.
			return interfacesSorted(byIndex), err
		}
	}
	return interfacesSorted(byIndex), nil
}

func ensure(m map[uint32]*Interface, idx uint32) *Interface {
	if e, ok := m[idx]; ok {
		return e
	}
	e := &Interface{IfIndex: idx}
	m[idx] = e
	return e
}

func interfacesSorted(m map[uint32]*Interface) []Interface {
	out := make([]Interface, 0, len(m))
	for _, e := range m {
		out = append(out, *e)
	}
	// Stable order by ifindex for diff-friendliness.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].IfIndex > out[j].IfIndex; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// indexFromOID extracts the trailing instance index from a tabular
// OID. e.g. ".1.3.6.1.2.1.2.2.1.2.5" with table = "1.3.6.1.2.1.2.2.1.2"
// yields 5.
func indexFromOID(full, table string) (uint32, bool) {
	dot := "."
	full = strings.TrimPrefix(full, dot)
	if !strings.HasPrefix(full, table+dot) {
		return 0, false
	}
	tail := strings.TrimPrefix(full, table+dot)
	// Multi-segment indices (rare for ifTable) collapse to the first
	// segment; full multi-instance support arrives if we ever hit a
	// table that uses it.
	first := strings.SplitN(tail, ".", 2)[0]
	n, err := strconv.ParseUint(first, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

func octetString(p gosnmp.SnmpPDU) string {
	switch v := p.Value.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	default:
		return ""
	}
}

func integerValue(p gosnmp.SnmpPDU) int {
	switch v := p.Value.(type) {
	case int:
		return v
	case uint:
		return int(v)
	case uint32:
		return int(v)
	case int64:
		return int(v)
	case uint64:
		return int(v)
	default:
		return 0
	}
}

func oidString(p gosnmp.SnmpPDU) string {
	if s, ok := p.Value.(string); ok {
		return s
	}
	return ""
}

func timeticksToMs(p gosnmp.SnmpPDU) uint64 {
	// SNMP TimeTicks are hundredths of seconds.
	return uint64(integerValue(p)) * 10
}

// buildGoSNMP constructs a gosnmp.GoSNMP from a Config. The v3 path
// pulls in the SNMPv3 user-based security model (USM) parameters
// supported by gosnmp; FromCredential ensures we pass plaintext
// passphrases (the store decrypts on the way in).
func buildGoSNMP(target string, cfg Config, ctx context.Context) (*gosnmp.GoSNMP, error) {
	g := &gosnmp.GoSNMP{
		Target:  target,
		Port:    cfg.Port,
		Timeout: cfg.Timeout,
		Retries: cfg.Retries,
		Context: ctx,
	}
	switch cfg.Version {
	case "", "v2c":
		g.Version = gosnmp.Version2c
		g.Community = cfg.Community
	case "v3":
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.MsgFlags = v3MsgFlagsFor(cfg)
		g.SecurityParameters = &gosnmp.UsmSecurityParameters{
			UserName:                 cfg.V3Username,
			AuthenticationProtocol:   v3AuthProto(cfg.V3AuthProto),
			AuthenticationPassphrase: cfg.V3AuthPass,
			PrivacyProtocol:          v3PrivProto(cfg.V3PrivProto),
			PrivacyPassphrase:        cfg.V3PrivPass,
		}
		g.ContextName = cfg.V3Context
	default:
		return nil, fmt.Errorf("snmpx: unsupported version %q", cfg.Version)
	}
	return g, nil
}

// v3MsgFlagsFor picks the gosnmp message flags by what passphrases
// are present. authPriv (sign + encrypt) is the default when both
// are configured; authNoPriv when only auth; noAuthNoPriv otherwise.
func v3MsgFlagsFor(cfg Config) gosnmp.SnmpV3MsgFlags {
	switch {
	case cfg.V3AuthPass != "" && cfg.V3PrivPass != "":
		return gosnmp.AuthPriv
	case cfg.V3AuthPass != "":
		return gosnmp.AuthNoPriv
	default:
		return gosnmp.NoAuthNoPriv
	}
}

func v3AuthProto(p string) gosnmp.SnmpV3AuthProtocol {
	switch p {
	case "MD5":
		return gosnmp.MD5
	case "SHA":
		return gosnmp.SHA
	case "SHA-224":
		return gosnmp.SHA224
	case "SHA-256":
		return gosnmp.SHA256
	case "SHA-384":
		return gosnmp.SHA384
	case "SHA-512":
		return gosnmp.SHA512
	default:
		return gosnmp.NoAuth
	}
}

func v3PrivProto(p string) gosnmp.SnmpV3PrivProtocol {
	switch p {
	case "DES":
		return gosnmp.DES
	case "AES":
		return gosnmp.AES
	case "AES-192":
		return gosnmp.AES192
	case "AES-256":
		return gosnmp.AES256
	default:
		return gosnmp.NoPriv
	}
}
