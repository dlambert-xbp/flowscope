import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, type ReactNode } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { api, fmt } from '../api'
import type { InterfaceTimeseriesPoint } from '../api'
import { useTheme } from '../theme'
import {
  DEFAULT_TIME_RANGE,
  rangeLabel,
  toApi,
  type TimeRange,
} from '../timeRange'

// uPlot reads plain strings, not CSS vars, so we resolve the live design
// tokens from the document at construction time and rebuild the plot when
// the theme changes.
type PlotColors = {
  line: string
  lineSoft: string
  faint: string
  accent: string
  ok: string
}
function readPlotColors(): PlotColors {
  const cs = getComputedStyle(document.documentElement)
  const v = (name: string) => cs.getPropertyValue(name).trim()
  return {
    line: v('--color-line'),
    lineSoft: v('--color-line-soft'),
    faint: v('--color-faint'),
    accent: v('--color-accent'),
    ok: v('--color-ok'),
  }
}

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
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const points = ts.data?.points ?? []
  const meta = ts.data
  const exporterLabel =
    meta?.sys_name ? `${meta.sys_name} · ${meta.exporter}` : (meta?.exporter ?? exporter)
  const ifaceLabel = meta?.if_descr ? meta.if_descr : `ifindex ${ifindex}`
  const aliasLabel = meta?.if_alias ?? ''
  return (
    <div className="border-b border-line">
      <SectionHead
        title={`${ifaceLabel} · ${exporterLabel}`}
        sub={aliasLabel ? aliasLabel : `counter timeseries · ${winLabel}`}
        right={<SourceBadge>SOURCE · COUNTERS · 1s SAMPLE</SourceBadge>}
      />
      <div className="px-4 py-3 bg-surface">
        <UPlotChart points={points} loading={ts.isLoading} error={ts.error as Error | undefined} />
        <Legend points={points} />
      </div>
    </div>
  )
}

function UPlotChart({
  points,
  loading,
  error,
}: {
  points: InterfaceTimeseriesPoint[]
  loading: boolean
  error?: Error
}) {
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const plotRef = useRef<uPlot | null>(null)
  const themeRef = useRef<string>('')
  const { resolved } = useTheme()

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return

    const xs = points.map((p) => Math.floor(new Date(p.ts).getTime() / 1000))
    const ins = points.map((p) => p.in_bps)
    const outs = points.map((p) => p.out_bps)
    const data: uPlot.AlignedData = [xs, ins, outs]

    const width = wrap.clientWidth || 800
    const height = 200

    // Tear down + rebuild on theme change so uPlot picks up the new
    // design tokens (its color strings are baked at construction).
    if (plotRef.current && themeRef.current !== resolved) {
      plotRef.current.destroy()
      plotRef.current = null
    }
    themeRef.current = resolved

    if (!plotRef.current) {
      plotRef.current = new uPlot(buildOpts(width, height, readPlotColors()), data, wrap)
    } else {
      plotRef.current.setSize({ width, height })
      plotRef.current.setData(data)
    }
  }, [points, resolved])

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return
    const ro = new ResizeObserver(() => {
      if (plotRef.current && wrap.clientWidth > 0) {
        plotRef.current.setSize({ width: wrap.clientWidth, height: 200 })
      }
    })
    ro.observe(wrap)
    return () => {
      ro.disconnect()
      plotRef.current?.destroy()
      plotRef.current = null
    }
  }, [])

  return (
    <div className="relative">
      <div
        ref={wrapRef}
        className="w-full h-[200px] bg-ink border border-line uplot-host"
      />
      {loading && points.length === 0 && (
        <Overlay>loading…</Overlay>
      )}
      {error && (
        <Overlay tone="error">timeseries: {error.message}</Overlay>
      )}
      {!loading && !error && points.length === 0 && (
        <Overlay>no points yet · waiting for the next sample interval</Overlay>
      )}
    </div>
  )
}

function Overlay({
  children,
  tone,
}: {
  children: ReactNode
  tone?: 'error'
}) {
  return (
    <div
      className={`absolute inset-0 flex items-center justify-center font-mono text-[11px] pointer-events-none ${
        tone === 'error' ? 'text-crit' : 'text-dim'
      }`}
    >
      {children}
    </div>
  )
}

function buildOpts(width: number, height: number, c: PlotColors): uPlot.Options {
  return {
    width,
    height,
    padding: [12, 16, 6, 8],
    cursor: {
      drag: { x: false, y: false },
      points: { size: 6 },
    },
    legend: { show: false },
    scales: {
      x: { time: true },
      y: {
        range: (_u, _min, max) => [0, Math.max(1, max)],
      },
    },
    axes: [
      {
        stroke: c.faint,
        grid: { stroke: c.lineSoft, width: 1 },
        ticks: { stroke: c.line, width: 1, size: 4 },
        font: '10px ui-monospace, "IBM Plex Mono", monospace',
        size: 28,
      },
      {
        stroke: c.faint,
        grid: { stroke: c.lineSoft, width: 1 },
        ticks: { stroke: c.line, width: 1, size: 4 },
        font: '10px ui-monospace, "IBM Plex Mono", monospace',
        size: 56,
        values: (_u, splits) => splits.map((v) => fmt.bps(v)),
      },
    ],
    series: [
      {
        value: (_u, v) => (v == null ? '—' : new Date(v * 1000).toLocaleTimeString()),
      },
      {
        label: 'in',
        stroke: c.accent,
        width: 1.5,
        points: { show: false },
        value: (_u, v) => (v == null ? '—' : fmt.bps(v)),
      },
      {
        label: 'out',
        stroke: c.ok,
        width: 1.5,
        points: { show: false },
        value: (_u, v) => (v == null ? '—' : fmt.bps(v)),
      },
    ],
  }
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
