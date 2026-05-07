import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, type ReactNode } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { api, fmt } from '../api'
import type { InterfaceTimeseriesPoint } from '../api'

// Theme tokens mirrored from index.css so uPlot's canvas matches the rest
// of the UI. Kept inline because uPlot reads plain strings, not CSS vars.
const COLOR_LINE = '#23262b'
const COLOR_LINE_SOFT = '#1a1c20'
const COLOR_FAINT = '#5a5b5e'
const COLOR_DIM = '#8b8a85'
const COLOR_ACCENT = '#ff5b1f'
const COLOR_OK = '#5a9c5f'

export function InterfaceChart({
  exporter,
  ifindex,
}: {
  exporter: string
  ifindex: number
}) {
  const ts = useQuery({
    queryKey: ['iface_ts', exporter, ifindex],
    queryFn: () => api.interfaceTimeseries(exporter, ifindex, 300),
    refetchInterval: 5000,
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
        sub={aliasLabel ? aliasLabel : 'counter timeseries · 5 min'}
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

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return

    const xs = points.map((p) => Math.floor(new Date(p.ts).getTime() / 1000))
    const ins = points.map((p) => p.in_bps)
    const outs = points.map((p) => p.out_bps)
    const data: uPlot.AlignedData = [xs, ins, outs]

    const width = wrap.clientWidth || 800
    const height = 200

    if (!plotRef.current) {
      plotRef.current = new uPlot(buildOpts(width, height), data, wrap)
    } else {
      plotRef.current.setSize({ width, height })
      plotRef.current.setData(data)
    }
  }, [points])

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
  const color = tone === 'error' ? '#e04646' : COLOR_DIM
  return (
    <div
      className="absolute inset-0 flex items-center justify-center font-mono text-[11px] pointer-events-none"
      style={{ color }}
    >
      {children}
    </div>
  )
}

function buildOpts(width: number, height: number): uPlot.Options {
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
        stroke: COLOR_FAINT,
        grid: { stroke: COLOR_LINE_SOFT, width: 1 },
        ticks: { stroke: COLOR_LINE, width: 1, size: 4 },
        font: '10px ui-monospace, "IBM Plex Mono", monospace',
        size: 28,
      },
      {
        stroke: COLOR_FAINT,
        grid: { stroke: COLOR_LINE_SOFT, width: 1 },
        ticks: { stroke: COLOR_LINE, width: 1, size: 4 },
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
        stroke: COLOR_ACCENT,
        width: 1.5,
        points: { show: false },
        value: (_u, v) => (v == null ? '—' : fmt.bps(v)),
      },
      {
        label: 'out',
        stroke: COLOR_OK,
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
