package snmpx

import (
	"fmt"
	"net"
	"net/netip"
)

// unmap4in6 strips ClickHouse's "::ffff:" prefix from a v4-mapped
// IPv6 string, returning the IPv4 dotted form. Pure IPv6 passes
// through unchanged. Local copy to avoid coupling snmpx to
// alerteng or store internals.
func unmap4in6(s string) string {
	const pfx = "::ffff:"
	if len(s) > len(pfx) && s[:len(pfx)] == pfx {
		return s[len(pfx):]
	}
	return s
}

// ipv6Bytes returns the 16-byte representation of an IP-string for
// the IPv6 ClickHouse column. Mirrors store.toIPv6.
func ipv6Bytes(s string) (net.IP, error) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return nil, fmt.Errorf("snmpx: parse %q: %w", s, err)
	}
	a := addr.As16()
	return a[:], nil
}
