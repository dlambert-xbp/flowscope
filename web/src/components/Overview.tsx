import { useQuery } from '@tanstack/react-query'
import { api, fmt } from '../api'
import type { Summary } from '../api'
import { Interfaces } from './Interfaces'
import { LiveTail } from './LiveTail'
import { useTimeRange, rangeLabel, toApi, type TimeRange } from '../timeRange'
import { TimeRangeSelector } from './TimeRangeSelector'

export function Overview() {
  const tr = useTimeRange('ov')
  const apiRange = toApi(tr.range)
  const summary = useQuery({
    queryKey: ['summary', tr.queryKey],
    queryFn: () => api.summary(apiRange),
    refetchInterval: tr.range.kind === 'preset' ? 2000 : false,
  })
  const ifaces = useQuery({
    queryKey: ['interfaces', tr.queryKey],
    queryFn: () => api.interfaces(apiRange),
    refetchInterval: tr.range.kind === 'preset' ? 5000 : false,
  })

  return (
    <div>
      <Banner
        summary={summary.data}
        loading={summary.isLoading}
        error={summary.error as Error | undefined}
        range={tr.range}
        onRangeChange={tr.set}
      />
      <KpiGrid
        summary={summary.data}
        interfaceCount={ifaces.data?.count ?? 0}
        hasIfaces={!!ifaces.data}
        range={tr.range}
      />
      <Interfaces rows={ifaces.data?.interfaces ?? []} loading={ifaces.isLoading} range={tr.range} />
      <LiveTail />
    </div>
  )
}

/* ------------------------------- Banner ------------------------------- */

function Banner({
  summary,
  loading,
  error,
  range,
  onRangeChange,
}: {
  summary?: Summary
  loading: boolean
  error?: Error
  range: TimeRange
  onRangeChange: (r: TimeRange) => void
}) {
  const winLabel = rangeLabel(range)
  const standfirstKind = range.kind === 'preset' ? `trailing ${winLabel}` : winLabel
  const refreshLabel = range.kind === 'preset' ? 'refresh · 2s' : 'absolute · static'
  return (
    <div className="flex items-stretch border-b border-line bg-surface">
      <div className="flex-1 p-4 border-r border-line flex flex-col gap-1">
        <div className="flex items-center gap-3 text-[10.5px] uppercase tracking-[0.1em] font-semibold text-dim">
          <span>standfirst · {standfirstKind}</span>
          <span className="font-mono text-[10px] text-faint normal-case tracking-[0.02em]">
            {refreshLabel}
          </span>
          <span className="ml-auto normal-case">
            <TimeRangeSelector range={range} onChange={onRangeChange} />
          </span>
        </div>
        <Standfirst summary={summary} loading={loading} error={error} range={range} />
      </div>
      <div className="flex">
        <BannerCol label="window" value={winLabel} mono />
        <BannerCol label="newest" value={summary ? fmt.time(summary.newest).slice(11, 19) + 'Z' : '—'} mono />
        <BannerCol label="oldest" value={summary ? fmt.time(summary.oldest).slice(11, 19) + 'Z' : '—'} mono />
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
  state,
}: {
  label: string
  value: string
  mono?: boolean
  state?: 'crit' | 'warn' | 'ok' | 'accent'
}) {
  const stateClass =
    state === 'crit'
      ? 'text-crit'
      : state === 'warn'
        ? 'text-warn'
        : state === 'ok'
          ? 'text-ok'
          : state === 'accent'
            ? 'text-accent'
            : 'text-text'
  return (
    <div className="p-4 min-w-[160px] border-r border-line last:border-r-0 flex flex-col gap-0.5 justify-center">
      <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold">{label}</div>
      <div
        className={`${mono ? 'font-mono' : ''} text-[22px] font-medium tabular leading-[1.1] tracking-[-0.01em] ${stateClass}`}
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

function KpiGrid({
  summary,
  interfaceCount,
  hasIfaces,
  range,
}: {
  summary?: Summary
  interfaceCount: number
  hasIfaces: boolean
  range: TimeRange
}) {
  // Exception bias: tiles that represent abnormal state get a tinted
  // wash + colored numeric. Healthy tiles stay neutral. Operator's eye
  // lands on the exception in <300ms.
  const empty = !summary || summary.flows === 0
  const winLabel = rangeLabel(range)
  const tiles: Tile[] = [
    {
      label: 'flow rate',
      value: summary ? fmt.num(summary.flows) : '—',
      unit: `flows · ${winLabel}`,
      state: empty ? undefined : 'accent',
      status: empty ? { text: 'idle', tone: 'dim' } : { text: 'live', tone: 'ok' },
    },
    {
      label: 'volume',
      value: summary ? fmt.bytes(summary.bytes) : '—',
      unit: `bytes · ${winLabel}`,
      micro: summary ? [`${fmt.num(summary.packets)} packets`] : [],
    },
    {
      label: 'exporters',
      value: summary ? fmt.num(summary.exporters) : '—',
      unit: 'unique sources',
      status: empty
        ? { text: 'none seen', tone: 'dim' }
        : { text: 'reporting', tone: 'ok' },
    },
    {
      label: 'interfaces',
      value: hasIfaces ? fmt.num(interfaceCount) : '—',
      unit: 'with counter samples',
      state: hasIfaces && interfaceCount === 0 ? 'warn' : undefined,
      status:
        hasIfaces && interfaceCount === 0
          ? { text: 'no sFlow yet', tone: 'warn' }
          : { text: 'authoritative', tone: 'ok' },
    },
    {
      label: 'storage',
      value: 'ok',
      unit: 'clickhouse',
      state: 'ok',
      status: { text: 'inserts flowing', tone: 'ok' },
    },
    {
      label: 'alerts',
      value: '0',
      unit: 'open',
      state: 'ok',
      status: { text: 'no rules yet', tone: 'dim' },
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
      <div className={`font-mono text-[24px] font-medium tabular leading-[1.1] tracking-[-0.02em] ${valueColor}`}>
        {tile.value}
        {tile.unit && <span className="text-faint text-[11px] font-mono ml-1.5">{tile.unit}</span>}
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

