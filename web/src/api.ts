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
  src_as: number
  dst_as: number
  tcp_flags: number
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

export type DeviceResourceKind =
  | 'cpu'
  | 'memory'
  | 'storage'
  | 'temperature'
  | 'fan'
  | 'power'
  | 'voltage'
  | 'current'

export type DeviceResourcePoint = {
  ts: string
  value_percent: number
  value_numeric: number
}

export type DeviceResource = {
  kind: DeviceResourceKind
  component: string
  source: string
  latest_ts: string
  latest_percent: number
  latest_bytes: number
  max_bytes: number
  latest_numeric: number
  unit: string
  points: DeviceResourcePoint[]
}

export type DeviceResourcesResponse = {
  exporter: string
  count: number
  rows: DeviceResource[]
  window: string
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

// AlertEvent is one row from the append-only alert_events ledger,
// as returned by /api/alerts/{id}.timeline. The detail modal
// renders these chronologically. Heartbeats are samples; opened /
// acknowledged / closed are state transitions worth highlighting.
export type AlertEvent = {
  ts: string
  state: 'opened' | 'heartbeat' | 'closed' | 'acknowledged'
  severity: 'critical' | 'warning' | 'info'
  title: string
  body: string
  actor: string
  labels: Record<string, string>
}

// AlertDetail is the full payload behind /api/alerts/{id}: the
// summary the row already shows, the event timeline, and a short
// list of linked flows derived from the alert's labels. flows_source
// explains how those flows were filtered:
//   "labels"   — src/dst labels narrowed the query meaningfully
//   "exporter" — only the exporter IP was usable
//   "none"     — no labels carried scope; flows array is empty
//   "error: …" — backend hit an error fetching flows; timeline still valid
export type AlertDetail = {
  alert: Alert
  timeline: AlertEvent[]
  flows: RecentFlow[]
  flows_source: string
}

export type StreamRow = {
  source: string
  flows: number
  bytes: number
  packets: number
  exporters: number
}

export type DNSLookupResult = {
  ip: string
  hostname: string
  err?: string
  skipped: boolean
  at: string
}

export type DNSLookupResponse = {
  count: number
  results: Record<string, DNSLookupResult>
}

export type IngestHealth = {
  started_at: string
  uptime_seconds: number
  udp_received_total: Record<string, number>
  parse_records_total: { protocol: string; label: string; value: number }[]
  parse_errors_total: { protocol: string; label: string; value: number }[]
  emit_errors_total: Record<string, number>
  template_cache: { hits: number; misses: number; size: number }
  ring_size: number
}

export type ExporterHealthRow = {
  exporter: string
  sys_name: string
  source: string
  datagrams: number
  seq_gaps: number
  loss_pct: number
  last_seen: string
}

export type StorageHealth = {
  insert_lag_seconds: number
  rows_per_sec_recent: number
  rows_last_60s: number
  newest_observed: string
  oldest_observed: string
  flows_rows_estimate: number
  iface_counter_samples_rows_estimate: number
  device_inventory_rows_estimate: number
}

export type FlowsListSort = 'observed' | 'bytes' | 'packets'
export type FlowsListDir = 'asc' | 'desc'

export type FlowTimeseriesPoint = {
  ts: string
  bytes: number
  packets: number
  flows: number
}

export type FlowsTimeseriesResponse = {
  count: number
  points: FlowTimeseriesPoint[]
  bucket_seconds: number
  window: string
}

export type FlagsBucket = {
  ts: string
  syn: number
  syn_ack: number
  fin: number
  rst: number
  ack_only: number
  psh: number
  urg: number
  total: number
}

export type FlowsFlagsTimeseriesResponse = {
  count: number
  buckets: FlagsBucket[]
  bucket_seconds: number
  window: string
}

export type FlowsListResponse = {
  count: number
  flows: RecentFlow[]
  limit: number
  offset: number
  sort: FlowsListSort
  dir: FlowsListDir
  window: string
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
  packets: number
  flows: number
}

export type TopNSort = 'bytes' | 'packets' | 'flows'

export type TopProtocol = {
  proto: number
  bytes: number
  packets: number
  flows: number
}

export type TopASN = {
  src_as: number
  dst_as: number
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

export type TopInterface = {
  exporter: string
  sys_name: string
  ifindex: number
  if_descr: string
  if_alias: string
  in_bytes: number
  out_bytes: number
  in_packets: number
  out_packets: number
  in_flows: number
  out_flows: number
  bytes: number
  packets: number
  flows: number
}

export type TopResponse<T> = {
  count: number
  rows: T[]
  source: string
  sort?: TopNSort
  window: string
}

/* ----------------------------- Settings & Services ----------------------------- */

export type ServiceSource = 'nmap' | 'iana' | 'both' | 'custom'

export type ServiceEntry = {
  name: string
  description?: string
  proto: string
  port: number
  port_lo?: number
  port_hi?: number
  group?: string
  source: ServiceSource
  frequency?: number
}

export type ServiceLookup = {
  found: boolean
  primary: ServiceEntry
  alternatives?: ServiceEntry[]
  multi: boolean
}

export type LibraryRow = {
  name: string
  proto: string
  port: number
  description?: string
  source: ServiceSource
  multi: boolean
  frequency?: number
}

export type LibraryResponse = {
  total: number
  limit: number
  offset: number
  rows: LibraryRow[]
  counts: { built_in: number }
}

export type CustomService = {
  id: string
  proto: string
  port_lo: number
  port_hi: number
  name: string
  description?: string
  group?: string
  owner?: string
  updated_at?: string
  updated_by?: string
}

export type APIToken = {
  id: string
  name: string
  prefix: string
  plaintext?: string
  scope: 'read' | 'write' | 'admin'
  created_at: string
  created_by?: string
  last_used_at?: string
  expires_at?: string
  revoked_at?: string
}

export type ExporterAllowlistEntry = {
  exporter: string
  label?: string
  enabled: boolean
  notes?: string
  updated_at?: string
  updated_by?: string
}

export type AppSettingValue = {
  name: string
  value: unknown
  updated_at?: string
  updated_by?: string
}

export type AlertRuleParamSpec = {
  name: string
  kind: 'int' | 'float' | 'string' | 'bool'
  default: number | string | boolean
  min?: number
  max?: number
}

export type AlertRuleAvailable = {
  rule_id: string
  label: string
  description: string
  params: AlertRuleParamSpec[]
  default_severity: 'critical' | 'warning' | 'info'
}

export type AlertRuleSetting = {
  rule_id: string
  enabled: boolean
  severity?: string
  params?: Record<string, unknown>
  runbook?: string
  channels?: string[]
  updated_at?: string
  updated_by?: string
}

export type Webhook = {
  id: string
  name: string
  kind: 'slack' | 'teams' | 'pagerduty' | 'http'
  url: string
  secret?: string
  has_secret: boolean
  header_template?: Record<string, string>
  enabled: boolean
  severity_filter?: string[]
  updated_at?: string
  updated_by?: string
}

// WebhookTestResult is the response from POST /api/settings/integrations/
// webhooks/{id}/test. OK = endpoint accepted the synthetic alert
// (HTTP 2xx). Skipped = severity_filter excluded the test (rare). On
// failure http_status + error carry the diagnostic so the UI can
// render "401 Unauthorized — check the token".
export type WebhookTestResult = {
  ok: boolean
  skipped?: boolean
  reason?: string
  http_status?: number
  duration_ms?: number
  error?: string
}

export type OIDCConfig = {
  enabled: boolean
  issuer?: string
  client_id?: string
  client_secret?: string
  has_secret: boolean
  redirect_uri?: string
  scopes?: string
  updated_at?: string
  updated_by?: string
  login_flow_status: string
}

export type AdvancedField = {
  name: string
  service: string
  env_var?: string
  description: string
  reload: 'live' | 'restart'
  default_text?: string
}

export type AuditEntry = {
  ts: string
  actor: string
  action: string
  resource: string
  target: string
  before_json?: string
  after_json?: string
  request_id?: string
  source_ip?: string
}

// AuthMe is the payload from GET /auth/me — the signed-in user's
// identity as carried in the OIDC session cookie. Used by the brand
// bar to show "alice@example.com" and by the Settings page to render
// the active session card.
export type AuthMe = {
  subject: string
  email: string
  scope: 'read' | 'write' | 'admin'
  id: string
  expires_at: string
}

// maybeRedirectToLogin checks the response for the Phase 2 OIDC
// auto-redirect signal: a 401 with WWW-Authenticate: oidc. Backend
// sets this when a session cookie was present but expired or
// otherwise rejected by the session source. We use it as the cue to
// send the user through /auth/login rather than leaving them on a
// dashboard full of failed fetches.
//
// Returns true if a redirect was triggered (caller should treat this
// as a terminal state — no further parsing).
function maybeRedirectToLogin(r: Response): boolean {
  if (r.status !== 401) return false
  const wa = r.headers.get('WWW-Authenticate') ?? ''
  if (!wa.toLowerCase().includes('oidc')) return false
  // Don't bounce if we're already on the /auth/ path or if the
  // operator explicitly opted out (used during e2e / scripted tests).
  if (window.location.pathname.startsWith('/auth/')) return false
  // Stash the current URL so /auth/callback can return to it. The
  // backend redirect lands at /, but a richer UI could read this on
  // boot and restore the route.
  try {
    sessionStorage.setItem('flowscope:post-login-return', window.location.pathname + window.location.search)
  } catch {
    /* noop */
  }
  window.location.assign('/auth/login')
  return true
}

async function getJSON<T>(url: string): Promise<T> {
  // Phase 1 auth: when the operator has saved an X-Auth-Token in
  // localStorage (Settings page), attach it to every read so the
  // backend's RequireRead middleware lets the request through. When
  // no token is saved we still send the request — the backend allows
  // unauth-bypass when no auth is configured server-side, and returns
  // 401 when it is. The caller's existing error path surfaces that.
  //
  // Phase 2: if the backend returns 401 with WWW-Authenticate: oidc,
  // the browser auto-redirects to /auth/login. credentials: 'same-
  // origin' carries the session cookie when one is present.
  const headers: Record<string, string> = {}
  const tok = settingsAuthToken()
  if (tok) headers['X-Auth-Token'] = tok
  const r = await fetch(url, { cache: 'no-store', headers, credentials: 'same-origin' })
  if (!r.ok) {
    if (maybeRedirectToLogin(r)) {
      // Reject with an Error so React Query treats this as a failed
      // request; the location.assign above will navigate away.
      throw new Error(`${url} → 401 (redirecting to /auth/login)`)
    }
    throw new Error(`${url} → ${r.status} ${r.statusText}`)
  }
  return (await r.json()) as T
}

// authHeaders builds the headers a read-tier POST (alert ack/close,
// SNMP test/walk) needs: optional Content-Type plus the saved
// X-Auth-Token if present. Centralised so every fetch in this file
// behaves the same way after the Phase 1 read gate landed.
function authHeaders(hasBody: boolean): Record<string, string> {
  const headers: Record<string, string> = {}
  if (hasBody) headers['Content-Type'] = 'application/json'
  const tok = settingsAuthToken()
  if (tok) headers['X-Auth-Token'] = tok
  return headers
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
  healthStreams: (range?: TimeRangeArg) =>
    getJSON<{ count: number; rows: StreamRow[]; window: string }>(
      `/api/health/streams?${timeQuery(range)}`,
    ),
  healthStorage: () => getJSON<StorageHealth>(`/api/health/storage`),
  healthIngest: () => getJSON<IngestHealth>(`/api/health/ingest`),
  dnsLookup: (ips: string[]) => {
    const sp = new URLSearchParams()
    for (const ip of ips) sp.append('ip', ip)
    return getJSON<DNSLookupResponse>(`/api/dns/lookup?${sp.toString()}`)
  },
  healthExporters: (range?: TimeRangeArg) =>
    getJSON<{ count: number; rows: ExporterHealthRow[]; window: string }>(
      `/api/health/exporters?${timeQuery(range)}`,
    ),
  recentFlows: (limit = 20, exporter?: string) =>
    getJSON<{ count: number; flows: RecentFlow[] }>(
      exporter
        ? `/api/flows/recent?limit=${limit}&exporter=${encodeURIComponent(exporter)}`
        : `/api/flows/recent?limit=${limit}`,
    ),
  flowsList: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    {
      limit = 50,
      offset = 0,
      sort = 'observed',
      dir = 'desc',
    }: {
      limit?: number
      offset?: number
      sort?: FlowsListSort
      dir?: FlowsListDir
    } = {},
  ) =>
    getJSON<FlowsListResponse>(
      withFilters(
        `/api/flows/list?${timeQuery(range)}&limit=${limit}&offset=${offset}&sort=${sort}&dir=${dir}`,
        filters,
      ),
    ),
  flowsTimeseries: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    bucketSeconds?: number,
  ) =>
    getJSON<FlowsTimeseriesResponse>(
      withFilters(
        `/api/flows/timeseries?${timeQuery(range)}${
          bucketSeconds ? `&bucket=${bucketSeconds}` : ''
        }`,
        filters,
      ),
    ),
  flowsFlagsTimeseries: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    bucketSeconds?: number,
  ) =>
    getJSON<FlowsFlagsTimeseriesResponse>(
      withFilters(
        `/api/flows/flags-timeseries?${timeQuery(range)}${
          bucketSeconds ? `&bucket=${bucketSeconds}` : ''
        }`,
        filters,
      ),
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
  deviceResources: (exporter: string, range?: TimeRangeArg) =>
    getJSON<DeviceResourcesResponse>(
      `/api/devices/${encodeURIComponent(exporter)}/resources?${timeQuery(range, 86400)}`,
    ),
  alerts: (state?: 'open' | 'acknowledged' | 'closed') =>
    getJSON<{ count: number; alerts: Alert[]; state: string }>(
      state ? `/api/alerts?state=${state}` : `/api/alerts`,
    ),
  alertSummary: () => getJSON<AlertSummary>(`/api/alerts/summary`),
  alertDetail: (id: string) =>
    getJSON<AlertDetail>(`/api/alerts/${encodeURIComponent(id)}`),
  ackAlert: (id: string) =>
    fetch(`/api/alerts/${encodeURIComponent(id)}/ack`, {
      method: 'POST',
      headers: authHeaders(false),
    }).then((r) => {
      if (!r.ok) throw new Error(`ack ${id} → ${r.status}`)
      return r.json()
    }),
  closeAlert: (id: string) =>
    fetch(`/api/alerts/${encodeURIComponent(id)}/close`, {
      method: 'POST',
      headers: authHeaders(false),
    }).then((r) => {
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
  requestSnmpWalk: async (exporter: string) => {
    const r = await fetch(
      `/api/devices/${encodeURIComponent(exporter)}/snmp/walk`,
      { method: 'POST' },
    )
    if (!r.ok) throw new Error(`walk ${exporter} → ${r.status}`)
    return r.json()
  },
  topTalkers: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    limit = 20,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopTalker>>(
      withFilters(
        `/api/top/talkers?${timeQuery(range)}&limit=${limit}&sort=${sort}`,
        filters,
      ),
    ),
  topServices: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    limit = 20,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopService>>(
      withFilters(
        `/api/top/services?${timeQuery(range)}&limit=${limit}&sort=${sort}`,
        filters,
      ),
    ),
  topProtocols: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopProtocol>>(
      withFilters(
        `/api/top/protocols?${timeQuery(range)}&sort=${sort}`,
        filters,
      ),
    ),
  topConversations: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    limit = 20,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopConversation>>(
      withFilters(
        `/api/top/conversations?${timeQuery(range)}&limit=${limit}&sort=${sort}`,
        filters,
      ),
    ),
  topASN: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    limit = 20,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopASN>>(
      withFilters(
        `/api/top/asn?${timeQuery(range)}&limit=${limit}&sort=${sort}`,
        filters,
      ),
    ),
  topInterfaces: (
    filters: URLSearchParams,
    range?: TimeRangeArg,
    limit = 20,
    sort: TopNSort = 'bytes',
  ) =>
    getJSON<TopResponse<TopInterface>>(
      withFilters(
        `/api/top/interfaces?${timeQuery(range)}&limit=${limit}&sort=${sort}`,
        filters,
      ),
    ),

  /* --------------------- Services --------------------- */
  serviceLookup: (proto: string, port: number) =>
    getJSON<ServiceLookup>(
      `/api/services/lookup?proto=${encodeURIComponent(proto)}&port=${port}`,
    ),
  serviceLibrary: (q: string, proto: string, limit = 200, offset = 0) => {
    const params = new URLSearchParams()
    if (q) params.set('q', q)
    if (proto) params.set('proto', proto)
    params.set('limit', String(limit))
    params.set('offset', String(offset))
    return getJSON<LibraryResponse>(`/api/services/library?${params.toString()}`)
  },
  listCustomServices: () =>
    getJSON<{ count: number; rows: CustomService[] }>(`/api/services/custom`),
  putCustomService: (c: Partial<CustomService>) =>
    settingsWrite(c.id ? `/api/services/custom/${encodeURIComponent(c.id)}` : `/api/services/custom`, 'PUT', c),
  deleteCustomService: (id: string) =>
    settingsWrite(`/api/services/custom/${encodeURIComponent(id)}`, 'DELETE'),

  /* --------------------- General settings --------------------- */
  listGeneralSettings: () =>
    getJSON<{ rows: AppSettingValue[] }>(`/api/settings/general`),
  putGeneralSetting: (name: string, value: unknown) =>
    settingsWrite(`/api/settings/general/${encodeURIComponent(name)}`, 'PUT', { value }),

  /* --------------------- Allowlist --------------------- */
  listAllowlist: () =>
    getJSON<{ count: number; rows: ExporterAllowlistEntry[] }>(`/api/settings/exporters/allowlist`),
  putAllowlist: (e: ExporterAllowlistEntry) =>
    settingsWrite(`/api/settings/exporters/allowlist/${encodeURIComponent(e.exporter)}`, 'PUT', e),
  deleteAllowlist: (exporter: string) =>
    settingsWrite(`/api/settings/exporters/allowlist/${encodeURIComponent(exporter)}`, 'DELETE'),

  /* --------------------- API tokens --------------------- */
  listTokens: () =>
    getJSON<{ count: number; rows: APIToken[] }>(`/api/settings/tokens`),
  createToken: (name: string, scope: APIToken['scope']) =>
    settingsWrite<APIToken>(`/api/settings/tokens`, 'POST', { name, scope }),
  revokeToken: (id: string) =>
    settingsWrite(`/api/settings/tokens/${encodeURIComponent(id)}`, 'DELETE'),

  /* --------------------- Audit --------------------- */
  listAudit: (filters: { resource?: string; actor?: string; action?: string; limit?: number; offset?: number } = {}) => {
    const p = new URLSearchParams()
    if (filters.resource) p.set('resource', filters.resource)
    if (filters.actor) p.set('actor', filters.actor)
    if (filters.action) p.set('action', filters.action)
    p.set('limit', String(filters.limit ?? 100))
    p.set('offset', String(filters.offset ?? 0))
    return getJSON<{ count: number; rows: AuditEntry[] }>(`/api/settings/audit?${p.toString()}`)
  },

  /* --------------------- Alert rule tunables --------------------- */
  listAlertRules: () =>
    getJSON<{ count: number; rows: AlertRuleSetting[]; available: AlertRuleAvailable[] }>(
      `/api/settings/alert-rules`,
    ),
  putAlertRule: (s: AlertRuleSetting) =>
    settingsWrite(`/api/settings/alert-rules/${encodeURIComponent(s.rule_id)}`, 'PUT', s),

  /* --------------------- Webhooks --------------------- */
  listWebhooks: () =>
    getJSON<{ count: number; rows: Webhook[] }>(`/api/settings/integrations/webhooks`),
  putWebhook: (w: Partial<Webhook>) =>
    settingsWrite<Webhook>(
      w.id
        ? `/api/settings/integrations/webhooks/${encodeURIComponent(w.id)}`
        : `/api/settings/integrations/webhooks`,
      'PUT',
      w,
    ),
  deleteWebhook: (id: string) =>
    settingsWrite(`/api/settings/integrations/webhooks/${encodeURIComponent(id)}`, 'DELETE'),
  testWebhook: (id: string) =>
    settingsWrite<WebhookTestResult>(
      `/api/settings/integrations/webhooks/${encodeURIComponent(id)}/test`,
      'POST',
    ),

  /* --------------------- OIDC --------------------- */
  getOIDC: () => getJSON<OIDCConfig>(`/api/settings/oidc`),
  putOIDC: (c: OIDCConfig) => settingsWrite<OIDCConfig>(`/api/settings/oidc`, 'PUT', c),

  /* --------------------- Advanced --------------------- */
  listAdvanced: () => getJSON<{ fields: AdvancedField[] }>(`/api/settings/advanced`),

  /* --------------------- Auth (Phase 2 OIDC) --------------------- */
  authMe: async (): Promise<AuthMe | null> => {
    // /auth/me returns 401 when no session — we treat that as "signed
    // out" without throwing. Anything else (200 with a body, or a
    // genuine error like 500) propagates the usual way.
    const r = await fetch('/auth/me', { cache: 'no-store', credentials: 'same-origin' })
    if (r.status === 401) return null
    if (!r.ok) throw new Error(`/auth/me → ${r.status} ${r.statusText}`)
    return (await r.json()) as AuthMe
  },
  authLogout: async (): Promise<{ ok: boolean }> => {
    const r = await fetch('/auth/logout', {
      method: 'POST',
      credentials: 'same-origin',
    })
    if (!r.ok) throw new Error(`/auth/logout → ${r.status}`)
    return (await r.json()) as { ok: boolean }
  },
}

// settingsAuthToken is read once per page-load. The Settings UI keeps
// it in localStorage so the operator only has to paste it once. The
// "store nothing" mode is the default when no token is set: requests
// go through, and the api treats them as unauth-bypass when no auth
// is configured anywhere.
function settingsAuthToken(): string {
  try {
    return localStorage.getItem('flowscope:auth-token') ?? ''
  } catch {
    return ''
  }
}

export function setSettingsAuthToken(t: string) {
  try {
    if (t) localStorage.setItem('flowscope:auth-token', t)
    else localStorage.removeItem('flowscope:auth-token')
  } catch {
    /* noop */
  }
}

async function settingsWrite<T = unknown>(
  url: string,
  method: 'PUT' | 'POST' | 'DELETE',
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const tok = settingsAuthToken()
  if (tok) headers['X-Auth-Token'] = tok
  const r = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'same-origin',
  })
  if (!r.ok) {
    if (maybeRedirectToLogin(r)) {
      throw new Error(`${method} ${url} → 401 (redirecting to /auth/login)`)
    }
    const text = await r.text()
    throw new Error(`${method} ${url} → ${r.status}: ${text}`)
  }
  return (await r.json()) as T
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
