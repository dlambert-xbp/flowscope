import { useEffect, useRef, type ReactNode } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { useTheme } from '../theme'
import { useTimeRange } from '../timeRange'

// TimeseriesChart is the shared uPlot wrapper for every chart in the
// product. One series per `series` entry, drag-to-zoom that maps to
// the global TimeRange so every other panel re-narrows in sync, and
// theme-aware colours that rebuild on theme switch.
//
// Brushing semantics: the operator drags across the chart, the
// pixel range is converted to (from, to) Date objects via
// u.posToVal(), and the global TimeRange is updated to that absolute
// window. The URL picks it up, every consumer (charts, tables,
// summary tiles) re-renders against the same window. Reset is via
// the TimeRangeSelector preset buttons in the top bar.

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
  const { resolved } = useTheme()
  const { setAbsolute } = useTimeRange()
  // Latest brush handler captured in a ref so the uPlot hook (built
  // once at construction time) always calls the freshest closure.
  // Avoids rebuilding the plot every render just to refresh callbacks.
  const setAbsoluteRef = useRef(setAbsolute)
  setAbsoluteRef.current = setAbsolute
  // Structural signature: any change here forces a destroy + rebuild
  // of the uPlot instance so series labels, colours, axis formatters
  // and y-anchors all refresh. Pure data changes (new xs / new
  // series[i].values) take the cheap setData path.
  const structuralKey = `${height}|${yMin ?? ''}|${yMax ?? ''}|${series.length}|${series.map((s) => `${s.label}/${s.color}`).join(',')}`
  const structuralRef = useRef(structuralKey)

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return

    if (xs.length === 0) {
      if (plotRef.current) {
        plotRef.current.destroy()
        plotRef.current = null
      }
      return
    }

    const data: uPlot.AlignedData = [xs, ...series.map((s) => s.values)]
    const width = wrap.clientWidth || 600

    const structuralChanged = structuralRef.current !== structuralKey
    if (
      plotRef.current &&
      (themeRef.current !== resolved || structuralChanged)
    ) {
      plotRef.current.destroy()
      plotRef.current = null
    }
    themeRef.current = resolved
    structuralRef.current = structuralKey

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
          onBrush: (from, to) => setAbsoluteRef.current(from, to),
        }),
        data,
        wrap,
      )
    } else {
      plotRef.current.setSize({ width, height })
      // resetScales=true (the uPlot default) refits the chart to the
      // freshly fetched window. With brushing now wired to the
      // global TimeRange, every refetch already reflects the
      // operator's chosen window — let uPlot snap to that data
      // extent cleanly instead of preserving a stale pixel scale.
      plotRef.current.setData(data)
    }
  }, [xs, series, height, yMin, yMax, yFormat, resolved, structuralKey])

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

  return (
    <div className="relative">
      <div
        ref={wrapRef}
        className="w-full bg-ink border border-line uplot-host"
        style={{ height }}
      />
      {loading && xs.length === 0 && <Overlay>loading…</Overlay>}
      {error && <Overlay tone="error">{error.message}</Overlay>}
      {!loading && !error && xs.length === 0 && (
        <Overlay>{emptyLabel ?? 'no points yet'}</Overlay>
      )}
      {xs.length > 0 && (
        <div className="absolute bottom-1.5 left-2 font-mono text-[10px] text-faint pointer-events-none">
          drag to set time range
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
  onBrush,
}: {
  width: number
  height: number
  colors: PlotColors
  series: ChartSeries[]
  yMin?: number
  yMax?: number
  yFormat?: (v: number) => string
  onBrush: (from: Date, to: Date) => void
}): uPlot.Options {
  // pendingBrushRef: when the user drags, uPlot calls setSelect
  // repeatedly with a growing rect. On mouseup (with setScale=true)
  // uPlot auto-zooms and then fires setSelect again with width=0 to
  // clear the rectangle. We capture the last non-zero pixel range
  // and fire onBrush exactly when it clears — that's the drag-end
  // signal, and gives us both immediate visual feedback (uPlot's
  // auto-zoom) and the absolute (from, to) for the global range.
  let pendingBrush: { min: number; max: number } | null = null

  return {
    width,
    height,
    padding: [12, 16, 6, 8],
    cursor: {
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
      setSelect: [
        (u) => {
          const sel = u.select
          if (sel.width > 0) {
            // Active selection while the operator is dragging.
            const lo = u.posToVal(sel.left, 'x')
            const hi = u.posToVal(sel.left + sel.width, 'x')
            if (Number.isFinite(lo) && Number.isFinite(hi) && hi > lo) {
              pendingBrush = { min: lo, max: hi }
            }
          } else if (pendingBrush) {
            // Selection just cleared — drag completed. uPlot has
            // already auto-zoomed the canvas (setScale: true above);
            // now propagate the same range to the global TimeRange so
            // every other panel re-narrows in sync. The next
            // refetch will arrive with data bounded to this window
            // and the chart will naturally redraw at the new extent.
            const { min, max } = pendingBrush
            pendingBrush = null
            const from = new Date(min * 1000)
            const to = new Date(max * 1000)
            onBrush(from, to)
          }
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
