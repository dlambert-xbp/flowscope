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
	PollDurationMs uint32
	Status         string // ok | partial | error
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

// Config configures a real (non-mock) SNMP client.
type Config struct {
	// Community string for v2c. v3 support arrives in a follow-up
	// slice once encrypted credential storage lands.
	Community string
	// Port defaults to 161.
	Port uint16
	// Timeout per request. Default 2s.
	Timeout time.Duration
	// Retries per request. Default 1.
	Retries int
}

func (c *Config) defaults() {
	if c.Community == "" {
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

// RealClient wraps gosnmp.GoSNMP for a single (target, community)
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

	g := &gosnmp.GoSNMP{
		Target:    target,
		Port:      rc.cfg.Port,
		Community: rc.cfg.Community,
		Version:   gosnmp.Version2c,
		Timeout:   rc.cfg.Timeout,
		Retries:   rc.cfg.Retries,
		Context:   ctx,
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

	inv.PollDurationMs = uint32(time.Since(start).Milliseconds())
	return inv, nil
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
