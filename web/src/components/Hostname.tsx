import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import type { DNSLookupResult } from '../api'

// Hostname renders a faint hostname under (or beside) a public IP
// when the api service can resolve a PTR record for it. Private IPs
// (RFC 1918, RFC 4193, link-local, multicast) are recognised
// client-side and skip the network round-trip entirely — there's
// no authoritative reverse DNS for those blocks outside a
// customer's own resolver.
//
// Usage:
//
//	<>
//	  10.110.0.182
//	  <Hostname ip="10.110.0.182" />   // no-op, private
//	</>
//	<>
//	  23.220.210.10
//	  <Hostname ip="23.220.210.10" />  // → "a23-220-210-10.deploy.akamai.net"
//	</>
//
// Multiple instances of the same IP de-dup at the TanStack Query
// cache level, so rendering 50 rows of the same destination only
// fires one HTTP request.

export function Hostname({
  ip,
  className = 'font-mono italic text-faint text-[10.5px]',
  prefix = ' · ',
}: {
  ip: string
  className?: string
  prefix?: string
}) {
  const lookup = useReverseDNS(ip)
  if (!lookup) return null
  if (lookup.skipped) return null
  if (!lookup.hostname) return null
  return (
    <span className={className}>
      {prefix}
      {lookup.hostname}
    </span>
  )
}

// useReverseDNS resolves one IP via the api's /api/dns/lookup
// endpoint. Returns undefined while loading or on error so callers
// can render the bare IP. Private IPs short-circuit before
// reaching the network — useful when we're about to render a long
// list of mixed private/public IPs.
export function useReverseDNS(ip: string): DNSLookupResult | undefined {
  const isPriv = isPrivate(ip)
  const q = useQuery({
    queryKey: ['rdns', ip],
    queryFn: () => api.dnsLookup([ip]),
    enabled: !!ip && !isPriv,
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  })
  if (isPriv) {
    return { ip, hostname: '', skipped: true, at: '' }
  }
  return q.data?.results?.[ip]
}

// isPrivate matches the server-side rdns.isPrivate so we don't
// fire wasted requests for IPs the backend would skip anyway.
// Covers RFC 1918, RFC 4193, link-local, loopback, multicast.
function isPrivate(ip: string): boolean {
  if (!ip) return true
  if (ip.includes(':')) return isPrivateIPv6(ip)
  return isPrivateIPv4(ip)
}

function isPrivateIPv4(ip: string): boolean {
  const parts = ip.split('.').map((p) => Number(p))
  if (parts.length !== 4 || parts.some((p) => Number.isNaN(p) || p < 0 || p > 255)) {
    return true
  }
  const [a, b] = parts
  if (a === 10) return true
  if (a === 172 && b >= 16 && b <= 31) return true
  if (a === 192 && b === 168) return true
  if (a === 127) return true
  if (a === 0) return true
  if (a === 169 && b === 254) return true
  if (a >= 224) return true // multicast + reserved
  if (a === 100 && b >= 64 && b <= 127) return true // CGNAT
  return false
}

function isPrivateIPv6(ip: string): boolean {
  const lower = ip.toLowerCase()
  if (lower === '::' || lower === '::1') return true
  if (lower.startsWith('fc') || lower.startsWith('fd')) return true // ULA RFC 4193
  if (lower.startsWith('fe80')) return true // link-local
  if (lower.startsWith('ff')) return true // multicast
  return false
}
