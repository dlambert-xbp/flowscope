import { useQuery } from '@tanstack/react-query'
import { type ReactNode } from 'react'
import { api, fmt } from '../api'
import type { InterfaceTimeseriesPoint } from '../api'
import {
  DEFAULT_TIME_RANGE,
  rangeLabel,
  toApi,
  useLiveInterval,
  type TimeRange,
} from '../timeRange'
import { TimeseriesChart, resolveColor } from './TimeseriesChart'

// Per-interface ingress/egress line chart on the Devices tab. The
// data shaping + section chrome stays here; the actual rendering
// (uPlot wrapper, drag-to-zoom, theme awareness, reset affordance)
// lives in the shared TimeseriesChart primitive.
export function InterfaceChart({
  exporter,
  ifindex,
  range = DEFAULT_TIME_RANGE,
}: {
  exporter: string
  ifindex: number
  range?: TimeRange
}) {
  const apiRange = toApi(range)
  const winLabel = rangeLabel(range)
  const rangeKey =
    range.kind === 'preset'
      ? ['preset', range.window]
      : ['absolute', range.from.toISOString(), range.to.toISOString()]
  const ts = useQuery({
    queryKey: ['iface_ts', exporter, ifindex, rangeKey],
    queryFn: () => api.interfaceTimeseries(exporter, ifindex, apiRange),
    refetchInterval: useLiveInterval(5000),
  })
  const points = ts.data?.points ?? []
  const meta = ts.data
  const exporterLabel =
    meta?.sys_name ? `${meta.sys_name} · ${meta.exporter}` : (meta?.exporter ?? exporter)
  const ifaceLabel = meta?.if_descr ? meta.if_descr : `ifindex ${ifindex}`
  const aliasLabel = meta?.if_alias ?? ''

  const xs = points.map((p) => Math.floor(new Date(p.ts).getTime() / 1000))
  const inSeries = {
    label: 'in',
    color: resolveColor('--color-accent', '#8aa8c8'),
    values: points.map((p) => p.in_bps),
    format: (v: number) => fmt.bps(v),
  }
  const outSeries = {
    label: 'out',
    color: resolveColor('--color-ok', '#7fa67f'),
    values: points.map((p) => p.out_bps),
    format: (v: number) => fmt.bps(v),
  }

  return (
    <div className="border-b border-line">
      <SectionHead
        title={`${ifaceLabel} · ${exporterLabel}`}
        sub={aliasLabel ? aliasLabel : `counter timeseries · ${winLabel}`}
        right={<SourceBadge>SOURCE · COUNTERS · 1s SAMPLE</SourceBadge>}
      />
      <div className="px-4 py-3 bg-surface">
        <TimeseriesChart
          xs={xs}
          series={[inSeries, outSeries]}
          height={200}
          yMin={0}
          yFormat={(v) => fmt.bps(v)}
          loading={ts.isLoading}
          error={ts.error as Error | undefined}
          emptyLabel="no points yet · waiting for the next sample interval"
        />
        <Legend points={points} />
      </div>
    </div>
  )
}

function Legend({ points }: { points: InterfaceTimeseriesPoint[] }) {
  const last = points[points.length - 1]
  return (
    <div className="mt-2 flex gap-5 font-mono text-[11px] text-dim tabular">
      <span className="flex items-center gap-2">
        <span className="inline-block w-3 h-0.5 bg-accent" />
        in · <strong className="text-text">{last ? fmt.bps(last.in_bps) : '—'}</strong>
      </span>
      <span className="flex items-center gap-2">
        <span className="inline-block w-3 h-0.5 bg-ok" />
        out · <strong className="text-text">{last ? fmt.bps(last.out_bps) : '—'}</strong>
      </span>
    </div>
  )
}

function SectionHead({
  title,
  sub,
  right,
}: {
  title: string
  sub?: string
  right?: ReactNode
}) {
  return (
    <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
      <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
      {sub && <span className="font-mono text-[11px] text-faint tabular">{sub}</span>}
      {right && <span className="ml-auto">{right}</span>}
    </div>
  )
}

function SourceBadge({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-[10px] tracking-[0.06em] text-accent">{children}</span>
  )
}
