import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { api, fmt } from '../api'
import type {
  Alert,
  AlertSummary,
  Device,
  StreamRow,
  Summary,
} from '../api'
import { rangeLabel, rangeSeconds, toApi, type TimeRange } from '../timeRange'

// Overview — a glance-and-walk-away system-health dashboard.
//
// Five rails, in priority order for an operator's eye:
//   1. Ingest pipeline (flow rate, volume, lag).
//   2. Exporter freshness (online / silent / offline counts).
//   3. Streams (per-source breakdown — NetFlow / sFlow / gNMI).
//   4. Alerts (open by severity + most recent).
//   5. Storage / retention span (oldest / newest / window size).
//
// No flow rows here — that's deliberate. Live tail and per-flow
// inspection live on the Flows tab. This view answers "is my
// collector healthy and collecting accurate information."

export function Overview({
  range,
  rangeKey,
}: {
  range: TimeRange
  rangeKey: unknown
}) {
  const apiRange = toApi(range)
  const summary = useQuery({
    queryKey: ['summary', rangeKey],
    queryFn: () => api.summary(apiRange),
    refetchInterval: range.kind === 'preset' ? 2000 : false,
  })
  const streams = useQuery({
    queryKey: ['health-streams', rangeKey],
    queryFn: () => api.healthStreams(apiRange),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const devices = useQuery({
    queryKey: ['devices', rangeKey],
    queryFn: () => api.devices(apiRange),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const alertSummary = useQuery({
    queryKey: ['alert-summary'],
    queryFn: () => api.alertSummary(),
    refetchInterval: 5000,
  })
  const openAlerts = useQuery({
    queryKey: ['alerts-open'],
    queryFn: () => api.alerts('open'),
    refetchInterval: 5000,
  })

  const deviceList = devices.data?.devices ?? []
  const exporterStatus = classifyExporters(deviceList)
  const enriched = deviceList.filter((d) => d.sys_name).length

  return (
    <div>
      <Banner
        summary={summary.data}
        loading={summary.isLoading}
        error={summary.error as Error | undefined}
        range={range}
      />
      <KpiGrid
        summary={summary.data}
        range={range}
        streams={streams.data?.rows ?? []}
        exporters={exporterStatus}
        enriched={enriched}
        totalDevices={deviceList.length}
        alertSummary={alertSummary.data}
      />
      <div className="grid grid-cols-1 lg:grid-cols-2 border-b border-line">
        <StreamsPanel rows={streams.data?.rows ?? []} loading={streams.isLoading} range={range} />
        <ExportersPanel
          devices={deviceList}
          loading={devices.isLoading}
          status={exporterStatus}
        />
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-2 border-b border-line">
        <AlertsPanel
          alerts={openAlerts.data?.alerts ?? []}
          summary={alertSummary.data}
          loading={openAlerts.isLoading}
        />
        <StoragePanel summary={summary.data} range={range} />
      </div>
    </div>
  )
}

/* ------------------------------- Banner ------------------------------- */

function Banner({
  summary,
  loading,
  error,
  range,
}: {
  summary?: Summary
  loading: boolean
  error?: Error
  range: TimeRange
}) {
  const winLabel = rangeLabel(range)
  const standfirstKind = range.kind === 'preset' ? `trailing ${winLabel}` : winLabel
  const refreshLabel = range.kind === 'preset' ? 'refresh · 2s' : 'absolute · static'
  return (
    <div className="flex items-stretch border-b border-line bg-surface">
      <div className="flex-1 p-4 border-r border-line flex flex-col gap-1">
        <div className="flex items-center gap-3 text-[10.5px] uppercase tracking-[0.1em] font-semibold text-dim">
          <span>system health · {standfirstKind}</span>
          <span className="font-mono text-[10px] text-faint normal-case tracking-[0.02em]">
            {refreshLabel}
          </span>
        </div>
        <Standfirst summary={summary} loading={loading} error={error} range={range} />
      </div>
      <div className="flex">
        <BannerCol label="window" value={winLabel} mono />
        <BannerCol
          label="newest"
          value={summary ? fmt.time(summary.newest).slice(11, 19) + 'Z' : '—'}
          mono
        />
        <BannerCol
          label="oldest"
          value={summary ? fmt.time(summary.oldest).slice(11, 19) + 'Z' : '—'}
          mono
        />
      </div>
    </div>
  )
}

function Standfirst({
  summary,
  loading,
  error,
  range,
}: {
  summary?: Summary
  loading: boolean
  error?: Error
  range: TimeRange
}) {
  if (loading) return <p className="text-[14px] text-dim">Connecting…</p>
  if (error) {
    return (
      <p className="text-[14px] text-text leading-[1.5] max-w-[78ch]">
        <span className="text-crit font-semibold">Connection error.</span>{' '}
        <span className="text-dim font-mono text-[11px]">{error.message}</span>
      </p>
    )
  }
  const lead = range.kind === 'preset' ? `Trailing ${rangeLabel(range)}` : rangeLabel(range)
  if (!summary || summary.flows === 0) {
    return (
      <p className="text-[14px] text-text leading-[1.5] max-w-[78ch]">
        Connected to ClickHouse but the <span className="text-accent font-semibold">flows</span> table is empty.
        Drive synthetic traffic with{' '}
        <code className="bg-raise px-1.5 py-0.5 text-[12px] font-mono">
          go run ./cmd/synth -- --target localhost:2055 --rate 5000
        </code>
        {' '}to populate it.
      </p>
    )
  }
  return (
    <p className="text-[14px] text-text leading-[1.5] max-w-[78ch]">
      {lead}: <span className="text-accent font-semibold tabular">{fmt.num(summary.flows)}</span> flows from{' '}
      <span className="text-accent font-semibold tabular">{summary.exporters}</span> exporters carrying{' '}
      <span className="text-accent font-semibold">{fmt.bytes(summary.bytes)}</span> across{' '}
      <span className="text-accent font-semibold tabular">{fmt.num(summary.packets)}</span> packets.{' '}
      <span className="text-ok">Pipeline nominal.</span>
    </p>
  )
}

function BannerCol({
  label,
  value,
  mono,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div className="p-4 min-w-[160px] border-r border-line last:border-r-0 flex flex-col gap-0.5 justify-center">
      <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold">{label}</div>
      <div
        className={`${mono ? 'font-mono' : ''} text-[22px] font-medium tabular leading-[1.1] tracking-[-0.01em] text-text`}
      >
        {value}
      </div>
    </div>
  )
}

/* ------------------------------ KPI grid ------------------------------ */

type Tile = {
  label: string
  value: string
  unit?: string
  state?: 'crit' | 'warn' | 'ok' | 'accent'
  micro?: string[]
  status?: { text: string; tone: 'crit' | 'warn' | 'ok' | 'dim' }
}

type ExporterStatus = {
  online: number
  silent: number
  offline: number
}

function KpiGrid({
  summary,
  range,
  streams,
  exporters,
  enriched,
  totalDevices,
  alertSummary,
}: {
  summary?: Summary
  range: TimeRange
  streams: StreamRow[]
  exporters: ExporterStatus
  enriched: number
  totalDevices: number
  alertSummary?: AlertSummary
}) {
  const empty = !summary || summary.flows === 0
  const seconds = Math.max(1, rangeSeconds(range))
  const flowRate = summary ? summary.flows / seconds : 0
  const ingestState =
    empty
      ? undefined
      : flowRate > 1
        ? 'accent'
        : 'warn'
  const exporterTone: Tile['status'] =
    exporters.offline > 0
      ? { text: `${exporters.offline} offline`, tone: 'crit' }
      : exporters.silent > 0
        ? { text: `${exporters.silent} silent`, tone: 'warn' }
        : exporters.online > 0
          ? { text: 'all reporting', tone: 'ok' }
          : { text: 'none seen', tone: 'dim' }
  const exporterState: Tile['state'] =
    exporters.offline > 0 ? 'crit' : exporters.silent > 0 ? 'warn' : undefined
  const enrichedPct =
    totalDevices > 0 ? Math.round((enriched / totalDevices) * 100) : 0
  const snmpStatus: Tile['status'] =
    totalDevices === 0
      ? { text: 'no exporters', tone: 'dim' }
      : enriched === 0
        ? { text: 'awaiting first walk', tone: 'warn' }
        : enriched < totalDevices
          ? { text: `${enriched}/${totalDevices} walked`, tone: 'warn' }
          : { text: 'all walked', tone: 'ok' }
  const alertOpen =
    (alertSummary?.open_critical ?? 0) +
    (alertSummary?.open_warning ?? 0) +
    (alertSummary?.open_info ?? 0)
  const alertState: Tile['state'] =
    (alertSummary?.open_critical ?? 0) > 0
      ? 'crit'
      : (alertSummary?.open_warning ?? 0) > 0
        ? 'warn'
        : 'ok'
  const alertStatus: Tile['status'] =
    alertOpen === 0
      ? { text: 'all clear', tone: 'ok' }
      : (alertSummary?.open_critical ?? 0) > 0
        ? { text: `${alertSummary?.open_critical} critical`, tone: 'crit' }
        : { text: `${alertSummary?.open_warning} warning`, tone: 'warn' }

  const tiles: Tile[] = [
    {
      label: 'ingest rate',
      value: summary ? fmt.num(Math.round(flowRate)) : '—',
      unit: 'flows / sec',
      state: ingestState,
      status: empty
        ? { text: 'idle', tone: 'dim' }
        : { text: 'live', tone: 'ok' },
      micro: summary
        ? [
            `${fmt.bytes(summary.bytes)} bytes`,
            `${fmt.num(summary.packets)} packets`,
          ]
        : [],
    },
    {
      label: 'exporters',
      value: fmt.num(exporters.online + exporters.silent + exporters.offline),
      unit: 'reporting',
      state: exporterState,
      status: exporterTone,
      micro: [
        `${exporters.online} online`,
        ...(exporters.silent ? [`${exporters.silent} silent`] : []),
        ...(exporters.offline ? [`${exporters.offline} offline`] : []),
      ],
    },
    {
      label: 'sources',
      value: fmt.num(streams.length),
      unit: 'active',
      state: streams.length === 0 ? 'warn' : undefined,
      status:
        streams.length === 0
          ? { text: 'no streams', tone: 'dim' }
          : { text: 'streaming', tone: 'ok' },
      micro: streams.slice(0, 3).map((s) => s.source),
    },
    {
      label: 'snmp enrichment',
      value: totalDevices > 0 ? `${enrichedPct}%` : '—',
      unit: 'walked',
      state:
        totalDevices > 0 && enriched < totalDevices ? 'warn' : undefined,
      status: snmpStatus,
      micro: totalDevices > 0 ? [`${enriched}/${totalDevices} devices`] : [],
    },
    {
      label: 'open alerts',
      value: fmt.num(alertOpen),
      unit: 'unresolved',
      state: alertState,
      status: alertStatus,
      micro: alertSummary
        ? [
            ...(alertSummary.open_critical
              ? [`${alertSummary.open_critical} crit`]
              : []),
            ...(alertSummary.open_warning
              ? [`${alertSummary.open_warning} warn`]
              : []),
            ...(alertSummary.open_info
              ? [`${alertSummary.open_info} info`]
              : []),
            ...(alertSummary.acknowledged
              ? [`${alertSummary.acknowledged} ack'd`]
              : []),
          ]
        : [],
    },
    {
      label: 'retention',
      value: summary ? rangeSpan(summary) : '—',
      unit: 'flow span in window',
      state: 'ok',
      status: { text: 'clickhouse ok', tone: 'ok' },
    },
  ]
  return (
    <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 border-b border-line">
      {tiles.map((t, i) => (
        <Kpi key={i} tile={t} />
      ))}
    </div>
  )
}

function Kpi({ tile }: { tile: Tile }) {
  const wash =
    tile.state === 'crit'
      ? 'bg-crit-wash'
      : tile.state === 'warn'
        ? 'bg-warn-wash'
        : tile.state === 'ok'
          ? 'bg-ok-wash'
          : ''
  const valueColor =
    tile.state === 'crit'
      ? 'text-crit'
      : tile.state === 'warn'
        ? 'text-warn'
        : tile.state === 'ok'
          ? 'text-ok'
          : tile.state === 'accent'
            ? 'text-accent'
            : 'text-text'
  return (
    <div
      className={`relative px-4 py-3.5 border-r border-line last:border-r-0 flex flex-col gap-1.5 min-h-[110px] ${wash}`}
    >
      <div className="flex items-center gap-2">
        <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold">
          {tile.label}
        </span>
        {tile.status && <Status status={tile.status} />}
      </div>
      <div
        className={`font-mono text-[24px] font-medium tabular leading-[1.1] tracking-[-0.02em] ${valueColor}`}
      >
        {tile.value}
        {tile.unit && (
          <span className="text-faint text-[11px] font-mono ml-1.5">{tile.unit}</span>
        )}
      </div>
      {tile.micro && tile.micro.length > 0 && (
        <div className="font-mono text-[11px] text-dim flex gap-2 flex-wrap mt-auto">
          {tile.micro.map((m, i) => (
            <span key={i}>{m}</span>
          ))}
        </div>
      )}
    </div>
  )
}

function Status({ status }: { status: NonNullable<Tile['status']> }) {
  const dotColor =
    status.tone === 'ok'
      ? 'bg-ok'
      : status.tone === 'crit'
        ? 'bg-crit'
        : status.tone === 'warn'
          ? 'bg-warn'
          : 'bg-faint'
  const textColor =
    status.tone === 'ok'
      ? 'text-ok'
      : status.tone === 'crit'
        ? 'text-crit'
        : status.tone === 'warn'
          ? 'text-warn'
          : 'text-faint'
  return (
    <span className="flex items-center gap-1.5">
      <span className={`w-1.5 h-1.5 rounded-full ${dotColor}`} />
      <span className={`text-[10px] uppercase tracking-[0.06em] ${textColor}`}>{status.text}</span>
    </span>
  )
}

/* ----------------------------- Streams panel ---------------------------- */

function StreamsPanel({
  rows,
  loading,
  range,
}: {
  rows: StreamRow[]
  loading: boolean
  range: TimeRange
}) {
  const seconds = Math.max(1, rangeSeconds(range))
  const totalFlows = rows.reduce((a, r) => a + r.flows, 0)
  return (
    <PanelShell
      title="Streams"
      sub={`per source · ${rangeLabel(range)}`}
      right="SOURCE · FLOWS"
      borderRight
    >
      {loading ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty>no ingest activity in window</Empty>
      ) : (
        <ul>
          {rows.map((r) => {
            const pct = totalFlows > 0 ? (r.flows / totalFlows) * 100 : 0
            const rate = Math.round(r.flows / seconds)
            return (
              <li
                key={r.source}
                className="px-4 py-2.5 border-b border-line-soft last:border-b-0 hover:bg-surface"
              >
                <div className="flex items-baseline justify-between gap-3">
                  <div className="min-w-0 flex items-baseline gap-2">
                    <span className="w-1.5 h-1.5 rounded-full bg-ok shrink-0" />
                    <span className="font-mono text-[12.5px] text-text">
                      {prettySource(r.source)}
                    </span>
                    <span className="font-mono text-[10.5px] text-faint">
                      {r.exporters} exporter{r.exporters === 1 ? '' : 's'}
                    </span>
                  </div>
                  <div className="font-mono text-[12px] tabular text-text shrink-0 flex gap-3">
                    <span>{fmt.num(rate)}/s</span>
                    <span className="text-dim">{fmt.bytes(r.bytes)}</span>
                  </div>
                </div>
                <div className="mt-1.5 h-px bg-line w-full overflow-hidden">
                  <div
                    className="h-full bg-accent"
                    style={{ width: `${Math.min(100, Math.max(0, pct))}%` }}
                  />
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </PanelShell>
  )
}

function prettySource(s: string): string {
  switch (s) {
    case 'netflow_v5':
      return 'NetFlow v5'
    case 'netflow_v9':
      return 'NetFlow v9'
    case 'ipfix':
      return 'IPFIX'
    case 'sflow':
      return 'sFlow'
    case 'gnmi':
      return 'gNMI'
    default:
      return s
  }
}

/* ---------------------------- Exporters panel --------------------------- */

function ExportersPanel({
  devices,
  loading,
  status,
}: {
  devices: Device[]
  loading: boolean
  status: ExporterStatus
}) {
  const sorted = [...devices].sort(
    (a, b) => secondsSince(b.last_seen) - secondsSince(a.last_seen) * -1,
  )
  const top = sorted.slice(0, 8)
  return (
    <PanelShell
      title="Exporter status"
      sub="last-seen freshness · click for details on Devices"
      right={
        <span className="font-mono text-[10px] tracking-[0.06em] flex gap-3">
          <span className="text-ok">{status.online} online</span>
          {status.silent > 0 && <span className="text-warn">{status.silent} silent</span>}
          {status.offline > 0 && <span className="text-crit">{status.offline} offline</span>}
        </span>
      }
    >
      {loading ? (
        <Loading />
      ) : devices.length === 0 ? (
        <Empty>no exporters seen in window</Empty>
      ) : (
        <ul>
          {top.map((d) => {
            const since = secondsSince(d.last_seen)
            const tone = freshnessTone(since)
            return (
              <li
                key={d.exporter}
                className="px-4 py-2 border-b border-line-soft last:border-b-0 hover:bg-surface flex items-center gap-3"
              >
                <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${tone.dot}`} />
                <div className="min-w-0 flex-1">
                  <div className="font-mono text-[12.5px] truncate">
                    {d.sys_name || d.exporter}
                  </div>
                  {d.sys_name && (
                    <div className="font-mono text-[10.5px] text-faint truncate">
                      {d.exporter}
                    </div>
                  )}
                </div>
                <span
                  className={`font-mono text-[10.5px] tabular shrink-0 ${tone.text}`}
                >
                  {formatSince(since)}
                </span>
              </li>
            )
          })}
          {devices.length > top.length && (
            <li className="px-4 py-2 text-[11px] font-mono text-faint border-b border-line-soft last:border-b-0">
              + {devices.length - top.length} more on Devices
            </li>
          )}
        </ul>
      )}
    </PanelShell>
  )
}

/* ----------------------------- Alerts panel ----------------------------- */

function AlertsPanel({
  alerts,
  summary,
  loading,
}: {
  alerts: Alert[]
  summary?: AlertSummary
  loading: boolean
}) {
  const top = alerts.slice(0, 5)
  const open =
    (summary?.open_critical ?? 0) +
    (summary?.open_warning ?? 0) +
    (summary?.open_info ?? 0)
  return (
    <PanelShell
      title="Open alerts"
      sub={open === 0 ? 'all clear' : `${open} unresolved`}
      right={
        summary && (
          <span className="font-mono text-[10px] tracking-[0.06em] flex gap-3">
            {summary.open_critical > 0 && (
              <span className="text-crit">{summary.open_critical} crit</span>
            )}
            {summary.open_warning > 0 && (
              <span className="text-warn">{summary.open_warning} warn</span>
            )}
            {summary.open_info > 0 && (
              <span className="text-dim">{summary.open_info} info</span>
            )}
            {!summary.open_critical && !summary.open_warning && !summary.open_info && (
              <span className="text-ok">no open alerts</span>
            )}
          </span>
        )
      }
      borderRight
    >
      {loading ? (
        <Loading />
      ) : top.length === 0 ? (
        <Empty>no open alerts · pipeline nominal</Empty>
      ) : (
        <ul>
          {top.map((a) => (
            <li
              key={a.id}
              className="px-4 py-2 border-b border-line-soft last:border-b-0 hover:bg-surface"
            >
              <div className="flex items-baseline gap-2.5">
                <SeverityBadge sev={a.severity} />
                <span className="font-mono text-[12.5px] text-text truncate flex-1">
                  {a.title}
                </span>
                <span className="font-mono text-[10.5px] text-faint shrink-0 tabular">
                  {formatSince(secondsSince(a.opened_at))}
                </span>
              </div>
              <div className="font-mono text-[11px] text-dim truncate ml-[58px]">
                {a.scope_display || a.scope}
              </div>
            </li>
          ))}
        </ul>
      )}
    </PanelShell>
  )
}

function SeverityBadge({ sev }: { sev: Alert['severity'] }) {
  const cls =
    sev === 'critical'
      ? 'text-crit'
      : sev === 'warning'
        ? 'text-warn'
        : 'text-dim'
  return (
    <span
      className={`font-mono text-[10px] uppercase tracking-[0.08em] font-semibold w-[48px] shrink-0 ${cls}`}
    >
      {sev}
    </span>
  )
}

/* ----------------------------- Storage panel ---------------------------- */

function StoragePanel({
  summary,
  range,
}: {
  summary?: Summary
  range: TimeRange
}) {
  const winLabel = rangeLabel(range)
  const newest = summary ? fmt.time(summary.newest) : '—'
  const oldest = summary ? fmt.time(summary.oldest) : '—'
  const span = summary ? rangeSpan(summary) : '—'
  return (
    <PanelShell
      title="Storage / retention"
      sub={`flow span across the ${winLabel}`}
      right={<span className="font-mono text-[10px] tracking-[0.06em] text-ok">CLICKHOUSE OK</span>}
    >
      <div className="grid grid-cols-3 border-l border-t border-line-soft">
        <Cell k="newest" v={newest.slice(0, 19) + 'Z'} />
        <Cell k="oldest" v={oldest.slice(0, 19) + 'Z'} />
        <Cell k="span" v={span} />
      </div>
      <div className="px-4 py-3 text-[11.5px] text-faint border-t border-line-soft leading-[1.5]">
        Insert lag, batcher queue depth, and per-shard write health are exposed via the api service's{' '}
        <code className="bg-raise px-1 font-mono text-text">/metrics</code> endpoint. A first-class
        storage panel here lands once <code className="bg-raise px-1 font-mono text-text">/api/health/storage</code>{' '}
        is wired.
      </div>
    </PanelShell>
  )
}

function Cell({ k, v }: { k: string; v: string }) {
  return (
    <div className="px-3 py-2.5 border-r border-b border-line-soft min-w-0 overflow-hidden">
      <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-0.5">
        {k}
      </div>
      <div title={v} className="font-mono text-[13px] text-text truncate tabular">
        {v}
      </div>
    </div>
  )
}

/* ----------------------------- Panel chrome ----------------------------- */

function PanelShell({
  title,
  sub,
  right,
  borderRight,
  children,
}: {
  title: string
  sub?: string
  right?: ReactNode
  borderRight?: boolean
  children: ReactNode
}) {
  return (
    <section className={borderRight ? 'lg:border-r lg:border-line' : ''}>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line bg-surface">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
        {sub && <span className="font-mono text-[11px] text-faint">{sub}</span>}
        {right && <span className="ml-auto">{right}</span>}
      </div>
      <div>{children}</div>
    </section>
  )
}

function Loading() {
  return <div className="px-4 py-6 text-faint font-mono text-[12px]">loading…</div>
}

function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="px-4 py-8 text-center text-[12px] font-mono text-dim">{children}</div>
  )
}

/* ----------------------------- Helpers ----------------------------- */

function secondsSince(iso: string): number {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return Infinity
  return Math.max(0, (Date.now() - t) / 1000)
}

function classifyExporters(devices: Device[]): ExporterStatus {
  let online = 0
  let silent = 0
  let offline = 0
  for (const d of devices) {
    const since = secondsSince(d.last_seen)
    if (since < 60) online++
    else if (since < 300) silent++
    else offline++
  }
  return { online, silent, offline }
}

function freshnessTone(since: number): { dot: string; text: string } {
  if (since < 60) return { dot: 'bg-ok', text: 'text-faint' }
  if (since < 300) return { dot: 'bg-warn', text: 'text-warn' }
  return { dot: 'bg-crit', text: 'text-crit' }
}

function formatSince(seconds: number): string {
  if (!Number.isFinite(seconds)) return '—'
  if (seconds < 60) return `${Math.floor(seconds)}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

function rangeSpan(summary: Summary): string {
  const a = new Date(summary.oldest).getTime()
  const b = new Date(summary.newest).getTime()
  if (!Number.isFinite(a) || !Number.isFinite(b) || b <= a) return '—'
  const sec = (b - a) / 1000
  if (sec < 60) return `${Math.round(sec)}s`
  if (sec < 3600) return `${Math.round(sec / 60)}m`
  if (sec < 86400) return `${(sec / 3600).toFixed(1)}h`
  return `${(sec / 86400).toFixed(1)}d`
}
