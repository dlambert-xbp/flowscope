import { useEffect, useRef, useState, type ReactNode } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { useTheme } from '../theme'

// TimeseriesChart is the shared uPlot wrapper for every chart in the
// product. One series per `series` entry, drag-to-zoom on the X axis
// out of the box, a "↺ reset zoom" affordance that appears when the
// scale diverges from the data extent, and theme-aware colours.
//
// Local zoom only — brushing this chart does not retroactively change
// the global URL time range. (The TimeRangeSelector is still the
// thing that decides what data each chart fetches.) When we want
// "apply this brush to all panels", add a button next to the reset
// affordance that pushes (from, to) into the URL.

export type ChartSeries = {
  label: string
  color: string // resolved hex or rgb; not a CSS var
  values: number[] // same length as xs; null/NaN are gaps
  format?: (v: number) => string
  width?: number
}

export type TimeseriesChartProps = {
  xs: number[] // unix seconds
  series: ChartSeries[]
  height?: number
  yMin?: number
  yMax?: number
  yFormat?: (v: number) => string
  loading?: boolean
  error?: Error
  emptyLabel?: string
}

type PlotColors = {
  line: string
  lineSoft: string
  faint: string
}

function readPlotColors(): PlotColors {
  const cs = getComputedStyle(document.documentElement)
  const v = (name: string) => cs.getPropertyValue(name).trim()
  return {
    line: v('--color-line') || '#2a2a2a',
    lineSoft: v('--color-line-soft') || '#222',
    faint: v('--color-faint') || '#888',
  }
}

export function TimeseriesChart({
  xs,
  series,
  height = 180,
  yMin,
  yMax,
  yFormat,
  loading,
  error,
  emptyLabel,
}: TimeseriesChartProps) {
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const plotRef = useRef<uPlot | null>(null)
  const themeRef = useRef<string>('')
  const dataExtentRef = useRef<[number, number] | null>(null)
  const { resolved } = useTheme()
  const [zoomed, setZoomed] = useState(false)

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return

    if (xs.length === 0) {
      // Tear down any existing plot when there's no data so the
      // overlay can take over the canvas cleanly.
      if (plotRef.current) {
        plotRef.current.destroy()
        plotRef.current = null
      }
      dataExtentRef.current = null
      setZoomed(false)
      return
    }

    const data: uPlot.AlignedData = [xs, ...series.map((s) => s.values)]
    dataExtentRef.current = [xs[0], xs[xs.length - 1]]

    const width = wrap.clientWidth || 600

    if (plotRef.current && themeRef.current !== resolved) {
      plotRef.current.destroy()
      plotRef.current = null
    }
    themeRef.current = resolved

    if (!plotRef.current) {
      plotRef.current = new uPlot(
        buildOpts({
          width,
          height,
          colors: readPlotColors(),
          series,
          yMin,
          yMax,
          yFormat,
          onScale: (min, max) => {
            const extent = dataExtentRef.current
            if (!extent) return
            const isFull = Math.abs(min - extent[0]) < 0.5 && Math.abs(max - extent[1]) < 0.5
            setZoomed(!isFull)
          },
        }),
        data,
        wrap,
      )
    } else {
      plotRef.current.setSize({ width, height })
      plotRef.current.setData(data)
    }
  }, [xs, series, height, yMin, yMax, yFormat, resolved])

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return
    const ro = new ResizeObserver(() => {
      if (plotRef.current && wrap.clientWidth > 0) {
        plotRef.current.setSize({ width: wrap.clientWidth, height })
      }
    })
    ro.observe(wrap)
    return () => {
      ro.disconnect()
      plotRef.current?.destroy()
      plotRef.current = null
    }
  }, [height])

  const resetZoom = () => {
    const u = plotRef.current
    const extent = dataExtentRef.current
    if (!u || !extent) return
    u.setScale('x', { min: extent[0], max: extent[1] })
  }

  return (
    <div className="relative">
      <div
        ref={wrapRef}
        className="w-full bg-ink border border-line uplot-host"
        style={{ height }}
      />
      {zoomed && (
        <button
          type="button"
          onClick={resetZoom}
          className="absolute top-1.5 right-1.5 font-mono text-[10px] uppercase tracking-[0.08em] px-2 py-0.5 border border-line bg-surface text-dim hover:border-accent hover:text-text"
          title="Reset zoom (show full window)"
        >
          ↺ reset zoom
        </button>
      )}
      {loading && xs.length === 0 && <Overlay>loading…</Overlay>}
      {error && <Overlay tone="error">{error.message}</Overlay>}
      {!loading && !error && xs.length === 0 && (
        <Overlay>{emptyLabel ?? 'no points yet'}</Overlay>
      )}
      {xs.length > 0 && (
        <div className="absolute bottom-1.5 left-2 font-mono text-[10px] text-faint pointer-events-none">
          drag to zoom
        </div>
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

function buildOpts({
  width,
  height,
  colors,
  series,
  yMin,
  yMax,
  yFormat,
  onScale,
}: {
  width: number
  height: number
  colors: PlotColors
  series: ChartSeries[]
  yMin?: number
  yMax?: number
  yFormat?: (v: number) => string
  onScale: (min: number, max: number) => void
}): uPlot.Options {
  return {
    width,
    height,
    padding: [12, 16, 6, 8],
    cursor: {
      // Drag-to-zoom on X. setScale=true makes uPlot do the zoom for
      // us; dist=4 prevents single-click drags from being treated as
      // brushes (which would crash the y-axis range when min===max).
      drag: { x: true, y: false, setScale: true, dist: 4 },
      points: { size: 6 },
    },
    legend: { show: false },
    scales: {
      x: { time: true },
      y: {
        range: (_u, dataMin, dataMax) => {
          const lo = yMin !== undefined ? yMin : dataMin
          const hi = yMax !== undefined ? yMax : Math.max(dataMax, lo + 1)
          return [lo, hi]
        },
      },
    },
    hooks: {
      setScale: [
        (u, key) => {
          if (key !== 'x') return
          const scale = u.scales.x
          if (scale.min == null || scale.max == null) return
          onScale(scale.min, scale.max)
        },
      ],
    },
    axes: [
      {
        stroke: colors.faint,
        grid: { stroke: colors.lineSoft, width: 1 },
        ticks: { stroke: colors.line, width: 1, size: 4 },
        font: '10px ui-monospace, "IBM Plex Mono", monospace',
        size: 28,
      },
      {
        stroke: colors.faint,
        grid: { stroke: colors.lineSoft, width: 1 },
        ticks: { stroke: colors.line, width: 1, size: 4 },
        font: '10px ui-monospace, "IBM Plex Mono", monospace',
        size: 56,
        values: yFormat
          ? (_u, splits) => splits.map((v) => yFormat(v))
          : undefined,
      },
    ],
    series: [
      {
        value: (_u, v) =>
          v == null ? '—' : new Date(v * 1000).toLocaleTimeString(),
      },
      ...series.map<uPlot.Series>((s) => ({
        label: s.label,
        stroke: s.color,
        width: s.width ?? 1.5,
        points: { show: false },
        value: (_u, v) =>
          v == null
            ? '—'
            : s.format
              ? s.format(v)
              : yFormat
                ? yFormat(v)
                : String(v),
      })),
    ],
  }
}

// resolveColor reads a CSS var (--color-*) from the document and
// falls back to the supplied default. uPlot wants concrete strings,
// not CSS var references, so callers wrap their token names through
// this helper before building series.
export function resolveColor(varName: string, fallback: string): string {
  if (typeof document === 'undefined') return fallback
  const v = getComputedStyle(document.documentElement).getPropertyValue(varName).trim()
  return v || fallback
}
