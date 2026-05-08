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
  exporter_name: string // SNMP-resolved sys_name; empty when not yet walked
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
  sys_name: string // SNMP-resolved hostname; empty when not yet walked
  ifindex: number
  if_descr: string // SNMP-resolved interface name (e.g. Te1/0/47)
  if_alias: string // operator-set description, optional
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
  sys_name: string
  ifindex: number
  if_descr: string
  if_alias: string
  window_seconds: number
  source: string
  points: InterfaceTimeseriesPoint[]
}

export type Device = {
  exporter: string
  sys_name: string // SNMP-resolved hostname; empty when not yet walked
  flows: number
  bytes: number
  packets: number
  first_seen: string
  last_seen: string
  iface_count: number
}

export type SNMPInterface = {
  ifindex: number
  if_descr: string
  if_alias: string
  if_type: number
  if_speed_bps: number
  if_mtu: number
  admin_status: string
  oper_status: string
  in_errors: number
  out_errors: number
  in_discards: number
  out_discards: number
}

export type DeviceInventory = {
  polled_at: string
  exporter: string
  sys_descr: string
  sys_object_id: string
  sys_uptime_ms: number
  sys_name: string
  sys_location: string
  sys_contact: string
  iface_count: number
  poll_duration_ms: number
  poll_status: string
  interfaces: SNMPInterface[]
}

export type Alert = {
  id: string
  rule_id: string
  severity: 'critical' | 'warning' | 'info'
  state: 'opened' | 'heartbeat' | 'closed' | 'acknowledged'
  scope: string
  scope_display: string // SNMP-enriched human form; falls back to scope when empty
  group_key: string
  title: string
  body: string
  runbook: string
  actor: string
  opened_at: string
  last_active_at: string
  labels: Record<string, string>
}

export type AlertSummary = {
  open_critical: number
  open_warning: number
  open_info: number
  acknowledged: number
  closed_last_24h: number
}

export type SNMPCredential = {
  exporter: string
  version: 'v2c' | 'v3'
  port: number
  interval_sec: number
  community?: string
  v3_username?: string
  v3_auth_proto?: string
  v3_auth_pass?: string
  v3_priv_proto?: string
  v3_priv_pass?: string
  v3_context?: string
  has_community: boolean
  has_auth_pass: boolean
  has_priv_pass: boolean
  updated_at?: string
  updated_by?: string
}

export type SNMPTestResult = {
  ok: boolean
  error?: string
  sys_descr?: string
  sys_name?: string
  interfaces?: number
  poll_duration_ms?: number
}

export type TopTalker = {
  src_addr: string
  dst_addr: string
  bytes: number
  packets: number
  flows: number
}

export type TopService = {
  dst_port: number
  proto: number
  bytes: number
  flows: number
}

export type TopProtocol = {
  proto: number
  bytes: number
  packets: number
  flows: number
}

export type TopConversation = {
  src_addr: string
  dst_addr: string
  src_port: number
  dst_port: number
  proto: number
  bytes: number
  packets: number
  flows: number
  last_seen: string
}

export type TopResponse<T> = {
  count: number
  rows: T[]
  source: string
  window: string
}

async function getJSON<T>(url: string): Promise<T> {
  const r = await fetch(url, { cache: 'no-store' })
  if (!r.ok) throw new Error(`${url} → ${r.status} ${r.statusText}`)
  return (await r.json()) as T
}

// TimeRangeArg is the API-facing slice of a TimeRange — either a
// preset window string ("5m") or a number of seconds, or an explicit
// {from, to} pair. Pass either to any api wrapper that supports it.
export type TimeRangeArg =
  | number
  | string
  | { from: Date; to: Date }

function timeQuery(t: TimeRangeArg | undefined, defaultSec = 300): string {
  if (t === undefined) return `window=${defaultSec}s`
  if (typeof t === 'number') return `window=${t}s`
  if (typeof t === 'string') return `window=${encodeURIComponent(t)}`
  const from = encodeURIComponent(t.from.toISOString())
  const to = encodeURIComponent(t.to.toISOString())
  return `from=${from}&to=${to}`
}

// timeQuerySeconds emits the legacy `seconds=N` form for endpoints that
// only accept it (interface timeseries on older clients). Falls back to
// the absolute form when {from,to} is given — the backend handles both.
function timeQuerySeconds(t: TimeRangeArg | undefined, defaultSec = 300): string {
  if (t === undefined) return `seconds=${defaultSec}`
  if (typeof t === 'number') return `seconds=${t}`
  if (typeof t === 'string') {
    // Map preset to seconds.
    const sec = presetToSeconds(t)
    return `seconds=${sec}`
  }
  const from = encodeURIComponent(t.from.toISOString())
  const to = encodeURIComponent(t.to.toISOString())
  return `from=${from}&to=${to}`
}

function presetToSeconds(p: string): number {
  switch (p) {
    case '5m': return 300
    case '15m': return 900
    case '1h': return 3600
    case '6h': return 21600
    case '24h': return 86400
    default: {
      const m = /^(\d+)([smhd])$/.exec(p)
      if (!m) return 300
      const n = Number(m[1])
      const u = m[2]
      const mul = u === 's' ? 1 : u === 'm' ? 60 : u === 'h' ? 3600 : 86400
      return n * mul
    }
  }
}

export const api = {
  summary: (range?: TimeRangeArg) =>
    getJSON<Summary>(`/api/summary?${timeQuery(range)}`),
  recentFlows: (limit = 20, exporter?: string) =>
    getJSON<{ count: number; flows: RecentFlow[] }>(
      exporter
        ? `/api/flows/recent?limit=${limit}&exporter=${encodeURIComponent(exporter)}`
        : `/api/flows/recent?limit=${limit}`,
    ),
  interfaces: (range?: TimeRangeArg, exporter?: string) =>
    getJSON<{ count: number; interfaces: InterfaceRow[]; source: string; window: string }>(
      exporter
        ? `/api/interfaces?${timeQuery(range)}&exporter=${encodeURIComponent(exporter)}`
        : `/api/interfaces?${timeQuery(range)}`,
    ),
  interfaceTimeseries: (exporter: string, ifindex: number, range?: TimeRangeArg) =>
    getJSON<InterfaceTimeseries>(
      `/api/interfaces/${exporter}/${ifindex}/timeseries?${timeQuerySeconds(range)}`,
    ),
  devices: (range?: TimeRangeArg) =>
    getJSON<{ count: number; devices: Device[]; window: string }>(
      `/api/devices?${timeQuery(range)}`,
    ),
  device: (exporter: string, range?: TimeRangeArg) =>
    getJSON<Device>(
      `/api/devices/${encodeURIComponent(exporter)}?${timeQuery(range)}`,
    ),
  deviceInventory: (exporter: string) =>
    getJSON<DeviceInventory>(
      `/api/devices/${encodeURIComponent(exporter)}/inventory`,
    ),
  alerts: (state?: 'open' | 'acknowledged' | 'closed') =>
    getJSON<{ count: number; alerts: Alert[]; state: string }>(
      state ? `/api/alerts?state=${state}` : `/api/alerts`,
    ),
  alertSummary: () => getJSON<AlertSummary>(`/api/alerts/summary`),
  ackAlert: (id: string) =>
    fetch(`/api/alerts/${encodeURIComponent(id)}/ack`, { method: 'POST' }).then((r) => {
      if (!r.ok) throw new Error(`ack ${id} → ${r.status}`)
      return r.json()
    }),
  closeAlert: (id: string) =>
    fetch(`/api/alerts/${encodeURIComponent(id)}/close`, { method: 'POST' }).then((r) => {
      if (!r.ok) throw new Error(`close ${id} → ${r.status}`)
      return r.json()
    }),
  listCredentials: () =>
    getJSON<{ count: number; credentials: SNMPCredential[] }>(
      `/api/snmp/credentials`,
    ),
  getCredential: (exporter: string) =>
    getJSON<SNMPCredential>(
      `/api/snmp/credentials/${encodeURIComponent(exporter)}`,
    ),
  putCredential: async (c: SNMPCredential) => {
    const r = await fetch(
      `/api/snmp/credentials/${encodeURIComponent(c.exporter)}`,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(c),
      },
    )
    if (!r.ok) {
      const txt = await r.text()
      throw new Error(`PUT ${c.exporter} → ${r.status}: ${txt}`)
    }
    return r.json()
  },
  deleteCredential: async (exporter: string) => {
    const r = await fetch(`/api/snmp/credentials/${encodeURIComponent(exporter)}`, {
      method: 'DELETE',
    })
    if (!r.ok) throw new Error(`DELETE ${exporter} → ${r.status}`)
    return r.json()
  },
  testCredential: async (exporter: string): Promise<SNMPTestResult> => {
    const r = await fetch(
      `/api/snmp/credentials/${encodeURIComponent(exporter)}/test`,
      { method: 'POST' },
    )
    if (!r.ok) throw new Error(`test ${exporter} → ${r.status}`)
    return r.json()
  },
  topTalkers: (filters: URLSearchParams, range?: TimeRangeArg, limit = 20) =>
    getJSON<TopResponse<TopTalker>>(
      withFilters(`/api/top/talkers?${timeQuery(range)}&limit=${limit}`, filters),
    ),
  topServices: (filters: URLSearchParams, range?: TimeRangeArg, limit = 20) =>
    getJSON<TopResponse<TopService>>(
      withFilters(`/api/top/services?${timeQuery(range)}&limit=${limit}`, filters),
    ),
  topProtocols: (filters: URLSearchParams, range?: TimeRangeArg) =>
    getJSON<TopResponse<TopProtocol>>(
      withFilters(`/api/top/protocols?${timeQuery(range)}`, filters),
    ),
  topConversations: (filters: URLSearchParams, range?: TimeRangeArg, limit = 20) =>
    getJSON<TopResponse<TopConversation>>(
      withFilters(`/api/top/conversations?${timeQuery(range)}&limit=${limit}`, filters),
    ),
}

function withFilters(url: string, filters: URLSearchParams): string {
  const tail = filters.toString()
  if (!tail) return url
  return `${url}&${tail}`
}

// Formatting helpers used across components.
export const fmt = {
  num: (n: number | null | undefined) =>
    n == null ? '—' : new Intl.NumberFormat('en-US').format(n),
  compact: (n: number | null | undefined) => {
    if (n == null) return '—'
    if (n < 1000) return String(n)
    if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
    if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 0)}M`
    return `${(n / 1_000_000_000).toFixed(1)}B`
  },
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

// labelExporter and labelInterface are the two enrichment helpers.
// Backend joins SNMP inventory at query time; the UI just picks
// whichever fields are populated and renders them in the agreed
// stacked format. Long names truncate via the `truncate` Tailwind
// utility on the parent cell.
export const labelExporter = (e: { exporter: string; sys_name?: string }) => ({
  primary: e.sys_name && e.sys_name.length > 0 ? e.sys_name : e.exporter,
  secondary: e.sys_name && e.sys_name.length > 0 ? e.exporter : '',
})

export const labelInterface = (
  i: { ifindex: number; if_descr?: string; if_alias?: string },
) => ({
  primary: i.if_descr && i.if_descr.length > 0 ? i.if_descr : `ifindex ${i.ifindex}`,
  secondary: i.if_alias ?? '',
})
