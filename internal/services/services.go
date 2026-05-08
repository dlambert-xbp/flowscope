// Package services resolves transport-layer (proto, port) tuples to
// human-readable service names. Two layers:
//
//  1. Built-in dataset: nmap-services + the IANA Service Names and
//     Transport Protocol Port Number Registry, embedded at build time
//     and parsed once at init. Read-only, lock-free.
//
//  2. Operator-defined custom services: arbitrary-name overrides that
//     can target a single port or a port range, optionally tagged with
//     a logical "group" the alert engine can reference. These live in
//     ClickHouse (table custom_services) and are layered on top of the
//     built-ins by Resolver.
//
// Resolution rule: most-specific match wins. A single-port custom
// outranks a narrow custom range outranks a wide custom range outranks
// the built-in. When more than one built-in name exists for the same
// (proto, port) the dataset is honest about it — Result.Multi is true
// and Result.Alternatives carries the rest. The UI marks these with a
// "*" so operators see when a label is one of several plausible
// meanings.
package services

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed data/nmap-services.txt
var nmapData []byte

//go:embed data/iana-ports.csv
var ianaData []byte

// Source identifies the origin of an Entry.
type Source string

const (
	SourceNmap   Source = "nmap"
	SourceIANA   Source = "iana"
	SourceBoth   Source = "both"
	SourceCustom Source = "custom"
)

// Entry is one resolved service. Built-in entries have PortLo == 0 ==
// PortHi (the zero values are omitted from JSON); custom entries set
// PortLo/PortHi to their declared range. Frequency is the nmap
// open-port frequency (0..1) or 0 when not from nmap.
type Entry struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Proto       string  `json:"proto"`
	Port        uint16  `json:"port"`
	PortLo      uint16  `json:"port_lo,omitempty"`
	PortHi      uint16  `json:"port_hi,omitempty"`
	Group       string  `json:"group,omitempty"`
	Source      Source  `json:"source"`
	Frequency   float64 `json:"frequency,omitempty"`
}

// Result is the answer to a (proto, port) lookup. Primary is the
// best candidate to display; Alternatives are the other known
// meanings in priority order. Multi == len(Alternatives) > 0 — the UI
// uses it to render the "*" marker without re-counting.
type Result struct {
	Found        bool    `json:"found"`
	Primary      Entry   `json:"primary"`
	Alternatives []Entry `json:"alternatives,omitempty"`
	Multi        bool    `json:"multi"`
}

// CustomEntry is an operator-defined service. Range support: PortLo ==
// PortHi means a single port. Group is optional and used by the alert
// engine to reference logical port collections.
type CustomEntry struct {
	Proto       string    `json:"proto"`
	PortLo      uint16    `json:"port_lo"`
	PortHi      uint16    `json:"port_hi"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Group       string    `json:"group,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// key indexes the built-in map. proto is canonicalised to lower-case;
// callers should use validProto for input.
type key struct {
	proto string
	port  uint16
}

var (
	builtIn     map[key][]Entry
	builtInOnce sync.Once
)

// validProto is the closed set of transports we accept on input. SCTP
// and DCCP are present in the IANA dataset; FlowScope ingest does not
// process them today but the lookup table is happy to include them.
var validProto = map[string]bool{"tcp": true, "udp": true, "sctp": true, "dccp": true}

func ensureBuiltIn() {
	builtInOnce.Do(func() {
		builtIn = make(map[key][]Entry, 32_000)
		parseNmap()
		parseIANA()
		sortEntries()
	})
}

// Lookup returns the resolved built-in entry for (proto, port). The
// caller is the api when it has not been wired through a Resolver
// (i.e. no custom-services overlay) — most code paths should go
// through a Resolver instead.
func Lookup(proto string, port uint16) Result {
	ensureBuiltIn()
	proto = strings.ToLower(proto)
	if !validProto[proto] {
		return Result{}
	}
	list := builtIn[key{proto, port}]
	if len(list) == 0 {
		return Result{}
	}
	r := Result{Found: true, Primary: list[0]}
	if len(list) > 1 {
		r.Multi = true
		r.Alternatives = append([]Entry(nil), list[1:]...)
	}
	return r
}

// BuiltInCount returns the number of built-in entries indexed across
// all (proto, port) keys. Useful for surfacing "X well-known ports"
// in the Settings UI.
func BuiltInCount() int {
	ensureBuiltIn()
	n := 0
	for _, list := range builtIn {
		n += len(list)
	}
	return n
}

// Resolver layers a snapshot of operator-defined custom services on
// top of the built-in dataset. Reads are lock-free for the built-in
// layer and RWMutex-protected for the customs slice. SetCustoms
// replaces the slice atomically; callers feed it from a periodic
// refresh of the custom_services table.
type Resolver struct {
	mu      sync.RWMutex
	customs []CustomEntry
}

// NewResolver returns an empty Resolver. Built-in lookups work
// immediately; SetCustoms enables overrides.
func NewResolver() *Resolver { return &Resolver{} }

// SetCustoms replaces the operator-defined overlay atomically. The
// input slice is copied; the caller is free to mutate it after.
func (r *Resolver) SetCustoms(c []CustomEntry) {
	cp := make([]CustomEntry, len(c))
	copy(cp, c)
	r.mu.Lock()
	r.customs = cp
	r.mu.Unlock()
}

// Customs returns a snapshot of the current overlay.
func (r *Resolver) Customs() []CustomEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CustomEntry, len(r.customs))
	copy(out, r.customs)
	return out
}

// Resolve returns the best entry for (proto, port). Custom entries
// always outrank built-ins; among customs, the narrowest range wins.
// The losing built-in (if any) and any built-in alternatives are
// preserved as Result.Alternatives so the UI can show "also known as".
func (r *Resolver) Resolve(proto string, port uint16) Result {
	proto = strings.ToLower(proto)
	if !validProto[proto] {
		return Result{}
	}

	var (
		best     *CustomEntry
		bestSpan uint32 = 65537 // sentinel — anything in 1..65536 wins
	)
	r.mu.RLock()
	for i := range r.customs {
		c := &r.customs[i]
		if !strings.EqualFold(c.Proto, proto) {
			continue
		}
		if port < c.PortLo || port > c.PortHi {
			continue
		}
		span := uint32(c.PortHi) - uint32(c.PortLo) + 1
		if span < bestSpan {
			bestSpan = span
			best = c
		}
	}
	r.mu.RUnlock()

	bi := Lookup(proto, port)
	if best == nil {
		return bi
	}

	primary := Entry{
		Name:        best.Name,
		Description: best.Description,
		Proto:       proto,
		Port:        port,
		PortLo:      best.PortLo,
		PortHi:      best.PortHi,
		Group:       best.Group,
		Source:      SourceCustom,
	}
	var alts []Entry
	if bi.Found {
		alts = append(alts, bi.Primary)
		alts = append(alts, bi.Alternatives...)
	}
	return Result{
		Found:        true,
		Primary:      primary,
		Alternatives: alts,
		Multi:        len(alts) > 0,
	}
}

/* ----------------------------- built-in parsers ----------------------------- */

func parseNmap() {
	sc := bufio.NewScanner(bytes.NewReader(nmapData))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 3 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" || name == "unknown" {
			continue
		}
		portProto := strings.TrimSpace(fields[1])
		freq, _ := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		slash := strings.IndexByte(portProto, '/')
		if slash <= 0 {
			continue
		}
		proto := strings.ToLower(portProto[slash+1:])
		if !validProto[proto] {
			continue
		}
		port64, err := strconv.ParseUint(portProto[:slash], 10, 16)
		if err != nil {
			continue
		}
		k := key{proto, uint16(port64)}
		builtIn[k] = append(builtIn[k], Entry{
			Name:      name,
			Proto:     proto,
			Port:      uint16(port64),
			Source:    SourceNmap,
			Frequency: freq,
		})
	}
}

func parseIANA() {
	r := csv.NewReader(bytes.NewReader(ianaData))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	// Header
	if _, err := r.Read(); err != nil {
		return
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) < 4 {
			continue
		}
		name := strings.TrimSpace(rec[0])
		if name == "" {
			continue
		}
		portField := strings.TrimSpace(rec[1])
		if portField == "" {
			continue
		}
		proto := strings.ToLower(strings.TrimSpace(rec[2]))
		if !validProto[proto] {
			continue
		}
		desc := strings.TrimSpace(rec[3])
		// Guardrails: skip the Reserved/Unassigned shells and the
		// "Discard / Reserved" markers that have no service name.
		if strings.EqualFold(desc, "Reserved") || strings.EqualFold(desc, "Unassigned") {
			continue
		}
		lo, hi, ok := parsePortRange(portField)
		if !ok {
			continue
		}
		// Skip pathological ranges: IANA occasionally lists very wide
		// ranges (e.g. 0-1023 type assignments). Anything wider than
		// 256 ports is treated as metadata, not a service binding —
		// the operator-defined custom list is the right place for
		// those.
		if hi-lo+1 > 256 {
			continue
		}
		for p := lo; p <= hi; p++ {
			mergeIANA(proto, uint16(p), name, desc)
		}
	}
}

func mergeIANA(proto string, port uint16, name, desc string) {
	k := key{proto, port}
	list := builtIn[k]
	for i := range list {
		if list[i].Name == name {
			list[i].Source = SourceBoth
			if list[i].Description == "" {
				list[i].Description = desc
			}
			builtIn[k] = list
			return
		}
	}
	builtIn[k] = append(list, Entry{
		Name:        name,
		Description: desc,
		Proto:       proto,
		Port:        port,
		Source:      SourceIANA,
	})
}

func parsePortRange(s string) (lo, hi int, ok bool) {
	if i := strings.IndexByte(s, '-'); i >= 0 {
		a, e1 := strconv.Atoi(s[:i])
		b, e2 := strconv.Atoi(s[i+1:])
		if e1 != nil || e2 != nil || a < 0 || b < 0 || a > 65535 || b > 65535 || a > b {
			return 0, 0, false
		}
		return a, b, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 {
		return 0, 0, false
	}
	return n, n, true
}

// sortEntries orders alternatives so the most authoritative name
// appears first: IANA-assigned names beat nmap-only names; among
// nmap-only names, higher open-frequency beats lower. Ties are stable
// (input order preserved).
func sortEntries() {
	for k, list := range builtIn {
		sort.SliceStable(list, func(i, j int) bool {
			ai := list[i].Source == SourceIANA || list[i].Source == SourceBoth
			aj := list[j].Source == SourceIANA || list[j].Source == SourceBoth
			if ai != aj {
				return ai
			}
			return list[i].Frequency > list[j].Frequency
		})
		builtIn[k] = list
	}
}
