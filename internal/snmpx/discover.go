package snmpx

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// MaxScanAddresses caps how many addresses a single bulk discovery scan
// may target. /24 (256 IPs) is the operational sweet spot — large
// enough to cover a typical management subnet, small enough that a
// worst-case all-dead scan finishes in well under a minute on the
// default worker pool. Operators with larger ranges split into
// multiple scans, which is also the audit-friendly behavior.
const MaxScanAddresses = 256

// DefaultScanWorkers is the bounded concurrency for SNMP probes during
// a bulk discovery scan. Separate from the snmp scheduler's worker
// pool so a long scan in the api process can't starve LLDP / BGP /
// interface walks on the snmp service.
const DefaultScanWorkers = 8

// DefaultScanTimeout is the per-probe SNMP timeout for bulk discovery
// scans. Single profile per scan means each IP gets exactly one
// chance, so we err slightly toward "give a real but slow device a
// chance" rather than the 1s tightness used on auto-bind first probes.
const DefaultScanTimeout = 2 * time.Second

// ScanResult is one row in a bulk discovery scan's output. Matched=true
// means the probe got back a sysDescr (i.e. the profile authenticated
// and the agent responded). Matched=false with Error set means the IP
// was alive but the profile didn't authenticate (rejected) or some
// other non-timeout error occurred. Matched=false with Error empty and
// Silent=true means the IP didn't respond at all.
type ScanResult struct {
	IP       string `json:"ip"`
	Matched  bool   `json:"matched"`
	Silent   bool   `json:"silent,omitempty"`
	SysName  string `json:"sys_name,omitempty"`
	SysDescr string `json:"sys_descr,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ScanCallback is invoked once per probed IP, in arbitrary order
// (worker pool). Implementations must be safe for concurrent calls.
type ScanCallback func(ScanResult)

// Scanner runs bulk discovery scans against a fixed list of addresses
// using a single chosen Profile. Stateless across scans — instantiate
// per scan or share, both are fine.
type Scanner struct {
	Workers int
	Timeout time.Duration
}

// NewScanner returns a Scanner with sensible defaults.
func NewScanner() *Scanner {
	return &Scanner{Workers: DefaultScanWorkers, Timeout: DefaultScanTimeout}
}

// Scan probes every address in addrs using cfg (the profile already
// projected into a Client Config). cb receives one ScanResult per IP.
// Returns the total count probed; ctx cancellation halts further
// dispatch but in-flight probes finish naturally.
func (s *Scanner) Scan(ctx context.Context, cfg Config, addrs []string, cb ScanCallback) int {
	workers := s.Workers
	if workers <= 0 {
		workers = DefaultScanWorkers
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultScanTimeout
	}
	cfg.Timeout = timeout
	cfg.Retries = 0

	jobs := make(chan string, workers*2)
	var wg sync.WaitGroup
	var probed int
	var probedMu sync.Mutex

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range jobs {
				res := probeOne(ctx, cfg, ip, timeout)
				if cb != nil {
					cb(res)
				}
				probedMu.Lock()
				probed++
				probedMu.Unlock()
			}
		}()
	}

	for _, ip := range addrs {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return probed
		case jobs <- ip:
		}
	}
	close(jobs)
	wg.Wait()
	return probed
}

// probeOne runs a single SNMP probe and projects the outcome into a
// ScanResult. Uses Walk (not a single Get) so we surface sysName +
// sysDescr the operator can recognize on the results table.
func probeOne(ctx context.Context, cfg Config, ip string, timeout time.Duration) ScanResult {
	probeCtx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	client := NewClient(cfg)
	inv, err := client.Walk(probeCtx, ip)
	if err != nil {
		msg := err.Error()
		if containsAny(msg, "timeout", "i/o timeout", "request timeout") {
			return ScanResult{IP: ip, Silent: true}
		}
		return ScanResult{IP: ip, Error: trimErr(msg)}
	}
	return ScanResult{
		IP:       ip,
		Matched:  true,
		SysName:  inv.SysName,
		SysDescr: inv.SysDescr,
	}
}

// trimErr keeps long underlying error strings from blowing up the
// JSON response on a 256-row results table.
func trimErr(s string) string {
	const max = 160
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ParseRange accepts CIDR (10.0.0.0/24), a dashed range
// (10.0.0.1-10.0.0.50), or a single IP (10.0.0.5 or 10.0.0.5/32) and
// returns the enumerated addresses. Rejects ranges larger than
// MaxScanAddresses with a clear error so the api can return 400.
//
// Both v4 and v6 work, though the /24-equivalent cap means v6 is
// effectively single-host. Network + broadcast addresses are NOT
// stripped from CIDR ranges — operators may legitimately want to
// probe them on point-to-point links.
func ParseRange(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("range is required")
	}

	// CIDR form. /32 is the single-IP case.
	if strings.Contains(input, "/") {
		pfx, err := netip.ParsePrefix(input)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR: %w", err)
		}
		pfx = pfx.Masked()
		bits := pfx.Bits()
		hostBits := pfx.Addr().BitLen() - bits
		if hostBits > 32 {
			return nil, fmt.Errorf("range too large")
		}
		count := uint64(1) << uint(hostBits)
		if count > MaxScanAddresses {
			return nil, fmt.Errorf("range %s expands to %d addresses; max %d per scan", input, count, MaxScanAddresses)
		}
		out := make([]string, 0, count)
		addr := pfx.Addr()
		for i := uint64(0); i < count; i++ {
			out = append(out, addr.Unmap().String())
			addr = addr.Next()
			if !addr.IsValid() {
				break
			}
		}
		return out, nil
	}

	// Dashed range form. Endpoints must be same family.
	if strings.Contains(input, "-") {
		parts := strings.SplitN(input, "-", 2)
		lo, err := netip.ParseAddr(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("invalid range start: %w", err)
		}
		hi, err := netip.ParseAddr(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid range end: %w", err)
		}
		if lo.Is4() != hi.Is4() || lo.Is6() != hi.Is6() {
			return nil, fmt.Errorf("range endpoints must be same address family")
		}
		if hi.Less(lo) {
			return nil, fmt.Errorf("range end must be >= start")
		}
		out := make([]string, 0, 16)
		cur := lo
		for {
			out = append(out, cur.Unmap().String())
			if len(out) > MaxScanAddresses {
				return nil, fmt.Errorf("range %s expands to >%d addresses; max %d per scan", input, MaxScanAddresses, MaxScanAddresses)
			}
			if cur == hi {
				break
			}
			cur = cur.Next()
			if !cur.IsValid() {
				return nil, fmt.Errorf("range overflow")
			}
		}
		return out, nil
	}

	// Bare single IP.
	addr, err := netip.ParseAddr(input)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}
	return []string{addr.Unmap().String()}, nil
}
