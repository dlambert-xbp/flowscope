import { useQuery } from '@tanstack/react-query'
import { api, type ServiceLookup } from '../api'

// protoNumberToString maps the IP-protocol numbers FlowScope's data
// model carries (6, 17, 132) to the transport names the resolver
// expects ("tcp", "udp", "sctp"). Returns '' for IP-layer protocols
// that don't have ports.
export function protoNumberToString(proto: number): string {
  switch (proto) {
    case 6: return 'tcp'
    case 17: return 'udp'
    case 132: return 'sctp'
    case 33: return 'dccp'
    default: return ''
  }
}

// useServiceName resolves a (proto, port) tuple via /api/services/lookup
// and caches the answer indefinitely — service names don't change
// during a session, so refetching every 2 seconds (the project default)
// would only burn requests. The override here disables polling and
// declares the value never stale.
export function useServiceName(proto: number, port: number) {
  const protoStr = protoNumberToString(proto)
  return useQuery<ServiceLookup>({
    queryKey: ['service-name', protoStr, port],
    queryFn: () => api.serviceLookup(protoStr, port),
    enabled: !!protoStr && port > 0 && port <= 65535,
    staleTime: Number.POSITIVE_INFINITY,
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
  })
}

// ServiceLabel renders the resolved service name for (proto, port),
// falling back to "port N" while the lookup resolves or when no
// well-known meaning exists. The "*" suffix flags ports with more
// than one known meaning so an operator notices the ambiguity.
export function ServiceLabel({
  proto,
  port,
  fallback,
}: {
  proto: number
  port: number
  fallback?: string
}) {
  const q = useServiceName(proto, port)
  if (!q.data || !q.data.found) {
    return <>{fallback ?? `port ${port}`}</>
  }
  return (
    <>
      {q.data.primary.name}
      {q.data.multi && <span className="text-warn ml-0.5" title="more than one known meaning">*</span>}
    </>
  )
}
