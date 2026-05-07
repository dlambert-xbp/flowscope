// Typed wrappers around the FlowScope JSON API. Mirrors the Go types
// in internal/store/queries.go. When api/openapi.yaml lands these
// types should be regenerated from it instead of hand-maintained.

export type Summary = {
  window: number // nanoseconds (Go time.Duration JSON shape)
  flows: number
  bytes: number
  packets: number
  exporters: number
  newest: string
  oldest: string
}

export type RecentFlow = {
  observed: string
  exporter: string
  src_addr: string
  dst_addr: string
  src_port: number
  dst_port: number
  proto: number
  bytes: number
  packets: number
  input_ifindex: number
  output_ifindex: number
  source: string
}

export type InterfaceRow = {
  exporter: string
  ifindex: number
  last_seen: string
  in_bps_latest: number
  out_bps_latest: number
  in_bps_peak: number
  out_bps_peak: number
  source: string
}

export type InterfaceTimeseriesPoint = {
  ts: string
  in_bps: number
  out_bps: number
}

export type InterfaceTimeseries = {
  exporter: string
  ifindex: number
  window_seconds: number
  source: string
  points: InterfaceTimeseriesPoint[]
}

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url, { cache: 'no-store' })
  if (!r.ok) throw new Error(`${url} → ${r.status} ${r.statusText}`)
  return (await r.json()) as T
}

export const api = {
  summary: (windowSec = 300) =>
    getJSON<Summary>(`/api/summary?window=${windowSec}s`),
  recentFlows: (limit = 20) =>
    getJSON<{ count: number; flows: RecentFlow[] }>(
      `/api/flows/recent?limit=${limit}`,
    ),
  interfaces: (windowSec = 300) =>
    getJSON<{ count: number; interfaces: InterfaceRow[]; source: string; window: string }>(
      `/api/interfaces?window=${windowSec}s`,
    ),
  interfaceTimeseries: (exporter: string, ifindex: number, seconds = 300) =>
    getJSON<InterfaceTimeseries>(
      `/api/interfaces/${exporter}/${ifindex}/timeseries?seconds=${seconds}`,
    ),
}

// Formatting helpers used across components.
export const fmt = {
  num: (n: number | null | undefined) =>
    n == null ? '—' : new Intl.NumberFormat('en-US').format(n),
  bytes: (n: number | null | undefined) => {
    if (n == null) return '—'
    const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
    let i = 0
    let v = Number(n)
    while (v >= 1024 && i < u.length - 1) {
      v /= 1024
      i++
    }
    return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`
  },
  bps: (n: number | null | undefined) => {
    if (n == null) return '—'
    const u = ['bps', 'kbps', 'Mbps', 'Gbps', 'Tbps']
    let i = 0
    let v = Number(n)
    while (v >= 1000 && i < u.length - 1) {
      v /= 1000
      i++
    }
    return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${u[i]}`
  },
  time: (iso: string | null | undefined) => {
    if (!iso) return '—'
    try {
      return new Date(iso).toISOString().replace('T', ' ').slice(0, 19) + 'Z'
    } catch {
      return iso
    }
  },
  proto: (n: number) =>
    ({ 1: 'icmp', 6: 'tcp', 17: 'udp', 47: 'gre', 50: 'esp' } as Record<number, string>)[n] ??
    `p${n}`,
}
