import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import { api, fmt } from '../api'
import type {
  FlagsBucket,
  FlowTimeseriesPoint,
  RecentFlow,
  TimeRangeArg,
} from '../api'
import { useTheme } from '../theme'
import { ServiceLabel } from './ServiceLabel'
import { Hostname } from './Hostname'
import { type Filter } from '../filters'

// FlowDrillDown describes the thing the operator clicked. The drawer
// renders a chart + a record table narrowed by these filters, on top
// of the page-level filters already in effect.
//
// Slice B/C of the colossus push: every Top-N row, every Investigate
// row, every chip with enough specificity opens this drawer with the
// matching scope. Two tabs — Chart for the timeseries, Records for
// the raw rows behind it.
export type FlowDrillDown = {
  title: string
  subtitle?: string
  filters: Filter[]
}

type Tab = 'chart' | 'state' | 'records'

export function FlowDrawer({
  drill,
  pageFilters,
  range,
  onClose,
}: {
  drill: FlowDrillDown | null
  pageFilters: URLSearchParams
  range: TimeRangeArg
  onClose: () => void
}) {
  const [tab, setTab] = useState<Tab>('chart')
  // Reset to the chart tab whenever the drill target changes — the
  // record list under "Records" is per-drill and shouldn't carry
  // tab state across distinct conversations.
  useEffect(() => {
    setTab('chart')
  }, [drill?.title])
  // Esc closes; lock body scroll while open. Effects only run while
  // the drawer is mounted (drill !== null).
  useEffect(() => {
    if (!drill) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [drill, onClose])
  if (!drill) return null
  // Compose the drill filters on top of the page filters. Drill
  // values win on conflicts because they're the operator's most
  // recent intent.
  const merged = mergeFilters(pageFilters, drill.filters)
  return (
    <div
      className="fixed inset-0 z-30"
      role="dialog"
      aria-modal="true"
      aria-label={`Flow drilldown: ${drill.title}`}
    >
      <button
        aria-label="Close drawer"
        onClick={onClose}
        className="absolute inset-0 bg-ink/60 backdrop-blur-[2px] cursor-default"
      />
      <aside className="absolute right-0 top-0 h-full w-full max-w-[820px] bg-ink border-l border-line flex flex-col shadow-2xl">
        <DrawerHeader drill={drill} onClose={onClose} />
        <DrawerTabs tab={tab} onChange={setTab} />
        <div className="flex-1 overflow-auto">
          {tab === 'chart' && <ChartTab filters={merged} range={range} />}
          {tab === 'state' && <ConnectionStateTab filters={merged} range={range} />}
          {tab === 'records' && <RecordsTab filters={merged} range={range} />}
        </div>
      </aside>
    </div>
  )
}

function mergeFilters(page: URLSearchParams, drill: Filter[]): URLSearchParams {
  const merged = new URLSearchParams(page)
  for (const f of drill) merged.set(f.key, f.value)
  return merged
}

function DrawerHeader({
  drill,
  onClose,
}: {
  drill: FlowDrillDown
  onClose: () => void
}) {
  return (
    <header className="flex items-start gap-3 px-5 py-4 border-b border-line bg-surface">
      <div className="min-w-0 flex-1">
        <div className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold mb-1">
          drilldown
        </div>
        <h2 className="font-mono text-[16px] text-text font-medium leading-tight truncate">
          {drill.title}
        </h2>
        {drill.subtitle && (
          <div className="font-mono text-[11.5px] text-dim mt-1">{drill.subtitle}</div>
        )}
        <div className="mt-2 flex gap-1.5 flex-wrap">
          {drill.filters.map((f) => (
            <span
              key={`${f.key}_${f.value}`}
              className="font-mono text-[10.5px] inline-flex items-center px-1.5 py-px border border-line text-dim"
            >
              <span className="text-faint">{f.key}</span>
              <span className="mx-1 text-faint">·</span>
              <span className="text-text">{f.label ?? f.value}</span>
            </span>
          ))}
        </div>
      </div>
      <button
        type="button"
        onClick={onClose}
        aria-label="close"
        className="font-mono text-[14px] text-dim hover:text-text leading-none px-2 py-1 border border-line"
      >
        ×
      </button>
    </header>
  )
}

function DrawerTabs({ tab, onChange }: { tab: Tab; onChange: (t: Tab) => void }) {
  return (
    <div className="flex border-b border-line bg-ink">
      <DrawerTab id="chart" active={tab} onChange={onChange}>
        Chart
      </DrawerTab>
      <DrawerTab id="state" active={tab} onChange={onChange}>
        Connection state
      </DrawerTab>
      <DrawerTab id="records" active={tab} onChange={onChange}>
        Records
      </DrawerTab>
    </div>
  )
}

function DrawerTab({
  id,
  active,
  onChange,
  children,
}: {
  id: Tab
  active: Tab
  onChange: (t: Tab) => void
  children: ReactNode
}) {
  const selected = id === active
  return (
    <button
      onClick={() => onChange(id)}
      className={`relative px-4 py-2.5 text-[13px] border-r border-line ${
        selected ? 'text-text' : 'text-dim hover:text-text hover:bg-surface'
      }`}
    >
      {children}
      {selected && <span className="absolute left-0 right-0 -bottom-px h-0.5 bg-accent" />}
    </button>
  )
}

/* ----------------------------- Chart tab ----------------------------- */

function ChartTab({
  filters,
  range,
}: {
  filters: URLSearchParams
  range: TimeRangeArg
}) {
  // Drawer is for studying a snapshot — explicit refresh via reopen
  // or filter change, never auto. Keeps the chart still while the
  // operator reads it.
  const q = useQuery({
    queryKey: ['flows-timeseries', filters.toString(), JSON.stringify(range)],
    queryFn: () => api.flowsTimeseries(filters, range),
    refetchOnWindowFocus: false,
  })
  const points = q.data?.points ?? []
  const bucket = q.data?.bucket_seconds ?? 0
  const totalBytes = points.reduce((a, p) => a + p.bytes, 0)
  const totalPackets = points.reduce((a, p) => a + p.packets, 0)
  const totalFlows = points.reduce((a, p) => a + p.flows, 0)
  return (
    <div className="px-5 py-4 space-y-4">
      <div className="grid grid-cols-3 border border-line">
        <Stat label="bytes total" value={fmt.bytes(totalBytes)} />
        <Stat label="packets total" value={fmt.num(totalPackets)} />
        <Stat label="flow records" value={fmt.num(totalFlows)} />
      </div>
      <div>
        <SectionHead
          title="Bytes per bucket"
          sub={bucket > 0 ? `${bucket}s buckets` : '—'}
        />
        <DrawerChart
          points={points}
          field="bytes"
          formatY={(v) => fmt.bytes(v)}
          loading={q.isLoading}
          error={q.error as Error | undefined}
        />
        <Legend
          color="accent"
          label="bytes"
          last={points.length ? fmt.bytes(points[points.length - 1].bytes) : '—'}
        />
      </div>
      <div>
        <SectionHead
          title="Packets per bucket"
          sub={bucket > 0 ? `${bucket}s buckets` : '—'}
        />
        <DrawerChart
          points={points}
          field="packets"
          formatY={(v) => fmt.num(v)}
          loading={q.isLoading}
          error={q.error as Error | undefined}
        />
        <Legend
          color="ok"
          label="packets"
          last={points.length ? fmt.num(points[points.length - 1].packets) : '—'}
        />
      </div>
    </div>
  )
}

function Stat({
  label,
  value,
  tone,
}: {
  label: string
  value: string
  tone?: string
}) {
  return (
    <div className="px-3 py-2.5 border-r border-line last:border-r-0 bg-surface">
      <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold">
        {label}
      </div>
      <div className={`font-mono text-[15px] tabular mt-0.5 ${tone ?? 'text-text'}`}>
        {value}
      </div>
    </div>
  )
}

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

function DrawerChart({
  points,
  field,
  formatY,
  loading,
  error,
}: {
  points: FlowTimeseriesPoint[]
  field: 'bytes' | 'packets'
  formatY: (v: number) => string
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
    const ys = points.map((p) => Number(p[field]))
    const data: uPlot.AlignedData = [xs, ys]
    const width = wrap.clientWidth || 800
    const height = 180

    if (plotRef.current && themeRef.current !== resolved) {
      plotRef.current.destroy()
      plotRef.current = null
    }
    themeRef.current = resolved

    const color = field === 'bytes' ? 'accent' : 'ok'
    if (!plotRef.current) {
      plotRef.current = new uPlot(
        buildOpts(width, height, readPlotColors(), color, formatY),
        data,
        wrap,
      )
    } else {
      plotRef.current.setSize({ width, height })
      plotRef.current.setData(data)
    }
  }, [points, field, formatY, resolved])

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return
    const ro = new ResizeObserver(() => {
      if (plotRef.current && wrap.clientWidth > 0) {
        plotRef.current.setSize({ width: wrap.clientWidth, height: 180 })
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
      <div ref={wrapRef} className="w-full h-[180px] bg-surface border border-line uplot-host" />
      {loading && points.length === 0 && <Overlay>loading…</Overlay>}
      {error && <Overlay tone="error">timeseries: {error.message}</Overlay>}
      {!loading && !error && points.length === 0 && (
        <Overlay>no points · widen the window or relax filters</Overlay>
      )}
    </div>
  )
}

function buildOpts(
  width: number,
  height: number,
  c: PlotColors,
  colorKey: 'accent' | 'ok',
  formatY: (v: number) => string,
): uPlot.Options {
  const stroke = colorKey === 'accent' ? c.accent : c.ok
  return {
    width,
    height,
    padding: [12, 16, 6, 8],
    cursor: { drag: { x: false, y: false }, points: { size: 6 } },
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
        size: 60,
        values: (_u, splits) => splits.map(formatY),
      },
    ],
    series: [
      {
        value: (_u, v) => (v == null ? '—' : new Date(v * 1000).toLocaleTimeString()),
      },
      {
        stroke,
        width: 1.5,
        points: { show: false },
        value: (_u, v) => (v == null ? '—' : formatY(v)),
      },
    ],
  }
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

function SectionHead({ title, sub }: { title: string; sub?: string }) {
  return (
    <div className="flex items-baseline gap-3 mb-2">
      <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
      {sub && <span className="font-mono text-[11px] text-faint">{sub}</span>}
    </div>
  )
}

function Legend({
  color,
  label,
  last,
}: {
  color: 'accent' | 'ok'
  label: string
  last: string
}) {
  const cls = color === 'accent' ? 'bg-accent' : 'bg-ok'
  return (
    <div className="mt-2 flex gap-5 font-mono text-[11px] text-dim tabular">
      <span className="flex items-center gap-2">
        <span className={`inline-block w-3 h-0.5 ${cls}`} />
        {label} · <strong className="text-text">{last}</strong>
      </span>
    </div>
  )
}

/* -------------------------- Connection state tab -------------------------- */

// ConnectionStateTab visualises the TCP-flag mix over the active
// window for the drilldown. Four sparklines stacked top-to-bottom
// (SYN, SYN+ACK, FIN, RST) plus an "ACK-only" line for context —
// these are the flags an operator cares about during an
// investigation: connection initiation, handshake completion,
// graceful close, hard reset, and bulk data flow respectively.
//
// The sparklines render only the flag in question, scaled to its
// own peak — comparing the absolute count of SYN to the absolute
// count of ACK isn't useful because ACK dominates by orders of
// magnitude. What matters is the *shape* of each over time.
//
// PSH and URG are deliberately omitted from the chart row but
// surface in the summary header when present, so the operator
// knows to look elsewhere if those flags carry meaning for them.
function ConnectionStateTab({
  filters,
  range,
}: {
  filters: URLSearchParams
  range: TimeRangeArg
}) {
  const q = useQuery({
    queryKey: ['flows-flags-ts', filters.toString(), JSON.stringify(range)],
    queryFn: () => api.flowsFlagsTimeseries(filters, range),
    refetchOnWindowFocus: false,
  })
  const buckets = q.data?.buckets ?? []
  const totals = buckets.reduce(
    (a, b) => ({
      syn: a.syn + b.syn,
      syn_ack: a.syn_ack + b.syn_ack,
      fin: a.fin + b.fin,
      rst: a.rst + b.rst,
      ack_only: a.ack_only + b.ack_only,
      psh: a.psh + b.psh,
      urg: a.urg + b.urg,
      total: a.total + b.total,
    }),
    { syn: 0, syn_ack: 0, fin: 0, rst: 0, ack_only: 0, psh: 0, urg: 0, total: 0 },
  )
  const handshakeRatio =
    totals.syn > 0 ? Math.min(100, (totals.syn_ack / totals.syn) * 100) : 0
  return (
    <div className="px-5 py-4 space-y-4">
      <StateSummary totals={totals} handshakeRatio={handshakeRatio} loading={q.isLoading} />
      {q.error && (
        <div className="font-mono text-[12px] text-crit">
          flag timeseries: {(q.error as Error).message}
        </div>
      )}
      {!q.isLoading && totals.total === 0 && !q.error && (
        <div className="px-4 py-8 text-center text-[12px] font-mono text-dim border border-line">
          no flow records in this window for this drilldown
        </div>
      )}
      {!q.isLoading && totals.total > 0 && totals.syn + totals.fin + totals.rst + totals.ack_only === 0 && (
        <div className="px-4 py-3 text-[11.5px] font-mono text-dim border border-line bg-surface">
          no TCP flags in this window — likely a non-TCP drilldown (UDP, ICMP)
        </div>
      )}
      <div className="grid grid-cols-1 gap-3">
        <FlagSparkline
          buckets={buckets}
          field="syn"
          label="SYN"
          tone="accent"
          help="connection initiation"
          loading={q.isLoading}
        />
        <FlagSparkline
          buckets={buckets}
          field="syn_ack"
          label="SYN+ACK"
          tone="ok"
          help="handshake completion"
          loading={q.isLoading}
        />
        <FlagSparkline
          buckets={buckets}
          field="fin"
          label="FIN"
          tone="warn"
          help="graceful close"
          loading={q.isLoading}
        />
        <FlagSparkline
          buckets={buckets}
          field="rst"
          label="RST"
          tone="crit"
          help="hard reset · interesting"
          loading={q.isLoading}
        />
        <FlagSparkline
          buckets={buckets}
          field="ack_only"
          label="ACK only"
          tone="dim"
          help="data flow / keepalive"
          loading={q.isLoading}
        />
      </div>
    </div>
  )
}

function StateSummary({
  totals,
  handshakeRatio,
  loading,
}: {
  totals: {
    syn: number
    syn_ack: number
    fin: number
    rst: number
    ack_only: number
    psh: number
    urg: number
    total: number
  }
  handshakeRatio: number
  loading: boolean
}) {
  const handshakeTone =
    handshakeRatio >= 90
      ? 'text-ok'
      : handshakeRatio >= 50
        ? 'text-warn'
        : totals.syn === 0
          ? 'text-dim'
          : 'text-crit'
  const rstTone = totals.rst > 0 ? 'text-crit' : 'text-faint'
  const flagsSeen: string[] = []
  if (totals.syn > 0) flagsSeen.push('SYN')
  if (totals.syn_ack > 0) flagsSeen.push('SYN+ACK')
  if (totals.fin > 0) flagsSeen.push('FIN')
  if (totals.rst > 0) flagsSeen.push('RST')
  if (totals.ack_only > 0) flagsSeen.push('ACK')
  if (totals.psh > 0) flagsSeen.push('PSH')
  if (totals.urg > 0) flagsSeen.push('URG')
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 border border-line">
      <Stat label="records" value={loading ? '…' : fmt.num(totals.total)} />
      <Stat
        label="handshake completion"
        value={
          loading
            ? '…'
            : totals.syn === 0
              ? '—'
              : `${handshakeRatio.toFixed(0)}%`
        }
        tone={handshakeTone}
      />
      <Stat label="RST count" value={loading ? '…' : fmt.num(totals.rst)} tone={rstTone} />
      <Stat
        label="flags seen"
        value={
          loading
            ? '…'
            : flagsSeen.length > 0
              ? flagsSeen.join(' · ')
              : '—'
        }
      />
    </div>
  )
}

function FlagSparkline({
  buckets,
  field,
  label,
  tone,
  help,
  loading,
}: {
  buckets: FlagsBucket[]
  field: 'syn' | 'syn_ack' | 'fin' | 'rst' | 'ack_only'
  label: string
  tone: 'accent' | 'ok' | 'warn' | 'crit' | 'dim'
  help: string
  loading: boolean
}) {
  const wrapRef = useRef<HTMLDivElement | null>(null)
  const plotRef = useRef<uPlot | null>(null)
  const themeRef = useRef<string>('')
  const { resolved } = useTheme()
  const total = buckets.reduce((a, b) => a + Number(b[field]), 0)

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return
    const xs = buckets.map((b) => Math.floor(new Date(b.ts).getTime() / 1000))
    const ys = buckets.map((b) => Number(b[field]))
    const data: uPlot.AlignedData = [xs, ys]
    const width = wrap.clientWidth || 600
    const height = 64

    if (plotRef.current && themeRef.current !== resolved) {
      plotRef.current.destroy()
      plotRef.current = null
    }
    themeRef.current = resolved

    if (!plotRef.current) {
      plotRef.current = new uPlot(buildSparkOpts(width, height, readPlotColors(), tone), data, wrap)
    } else {
      plotRef.current.setSize({ width, height })
      plotRef.current.setData(data)
    }
  }, [buckets, field, tone, resolved])

  useEffect(() => {
    const wrap = wrapRef.current
    if (!wrap) return
    const ro = new ResizeObserver(() => {
      if (plotRef.current && wrap.clientWidth > 0) {
        plotRef.current.setSize({ width: wrap.clientWidth, height: 64 })
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
    <div className="border border-line">
      <div className="flex items-baseline gap-3 px-3 py-1.5 border-b border-line bg-surface">
        <span
          className={`font-mono text-[11px] uppercase tracking-[0.06em] font-semibold ${toneToText(tone)}`}
        >
          {label}
        </span>
        <span className="font-mono text-[11px] text-faint">{help}</span>
        <span className="ml-auto font-mono text-[11px] tabular text-text">{fmt.num(total)}</span>
      </div>
      <div className="relative">
        <div ref={wrapRef} className="w-full h-[64px] bg-ink uplot-host" />
        {loading && buckets.length === 0 && (
          <div className="absolute inset-0 flex items-center justify-center text-faint font-mono text-[10.5px] pointer-events-none">
            loading…
          </div>
        )}
        {!loading && total === 0 && (
          <div className="absolute inset-0 flex items-center justify-center text-faint font-mono text-[10.5px] pointer-events-none">
            none in window
          </div>
        )}
      </div>
    </div>
  )
}

function toneToText(t: 'accent' | 'ok' | 'warn' | 'crit' | 'dim'): string {
  switch (t) {
    case 'accent':
      return 'text-accent'
    case 'ok':
      return 'text-ok'
    case 'warn':
      return 'text-warn'
    case 'crit':
      return 'text-crit'
    case 'dim':
      return 'text-dim'
  }
}

function buildSparkOpts(
  width: number,
  height: number,
  c: PlotColors,
  tone: 'accent' | 'ok' | 'warn' | 'crit' | 'dim',
): uPlot.Options {
  const stroke =
    tone === 'accent'
      ? c.accent
      : tone === 'ok'
        ? c.ok
        : tone === 'warn'
          ? '#d4a017'
          : tone === 'crit'
            ? '#e04646'
            : c.faint
  return {
    width,
    height,
    padding: [6, 8, 4, 8],
    cursor: { drag: { x: false, y: false }, points: { size: 4 } },
    legend: { show: false },
    scales: {
      x: { time: true },
      y: {
        range: (_u, _min, max) => [0, Math.max(1, max)],
      },
    },
    axes: [
      { show: false },
      { show: false },
    ],
    series: [
      {
        value: (_u, v) => (v == null ? '—' : new Date(v * 1000).toLocaleTimeString()),
      },
      {
        stroke,
        width: 1.25,
        points: { show: false },
        value: (_u, v) => (v == null ? '0' : String(v)),
      },
    ],
  }
}

/* ----------------------------- Records tab ----------------------------- */

function RecordsTab({
  filters,
  range,
}: {
  filters: URLSearchParams
  range: TimeRangeArg
}) {
  const q = useQuery({
    queryKey: ['drawer-records', filters.toString(), JSON.stringify(range)],
    queryFn: () => api.flowsList(filters, range, { limit: 50, sort: 'observed', dir: 'desc' }),
    refetchOnWindowFocus: false,
  })
  const flows = q.data?.flows ?? []
  return (
    <div className="px-5 py-4 space-y-2">
      <div className="flex items-baseline gap-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          Recent records matching the drilldown
        </span>
        <span className="font-mono text-[11px] text-faint">
          {q.isLoading ? 'loading…' : `${flows.length} most recent`}
        </span>
      </div>
      {q.error && (
        <div className="font-mono text-[12px] text-crit">error · {(q.error as Error).message}</div>
      )}
      {!q.isLoading && flows.length === 0 ? (
        <div className="px-4 py-8 text-center text-[12px] font-mono text-dim border border-line">
          no records match this drilldown in the window
        </div>
      ) : (
        <ul className="space-y-2">
          {flows.map((f, i) => (
            <RecordCard key={i} flow={f} />
          ))}
        </ul>
      )}
    </div>
  )
}

function RecordCard({ flow }: { flow: RecentFlow }) {
  const [open, setOpen] = useState(false)
  return (
    <li className="border border-line">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="w-full text-left px-3 py-2 flex items-center gap-3 hover:bg-surface"
      >
        <span
          aria-hidden
          className={`text-faint text-[10px] inline-block transition-transform ${open ? 'rotate-90' : ''}`}
        >
          ▶
        </span>
        <span className="font-mono text-[11.5px] text-faint tabular shrink-0">
          {fmt.time(flow.observed).slice(11, 23)}
        </span>
        <span className="font-mono text-[12px] text-text truncate flex-1">
          {flow.src_addr}:{flow.src_port} <span className="text-faint">→</span>{' '}
          {flow.dst_addr}:{flow.dst_port}
        </span>
        <span className="font-mono text-[11.5px] text-accent shrink-0">{fmt.proto(flow.proto)}</span>
        <span className="font-mono text-[11.5px] text-text tabular shrink-0">
          {fmt.bytes(flow.bytes)}
        </span>
      </button>
      {open && <RawRecord flow={flow} />}
    </li>
  )
}

function RawRecord({ flow }: { flow: RecentFlow }) {
  type Field = { k: string; v: ReactNode; mono?: boolean; title?: string }
  const fields: Field[] = [
    { k: 'observed', v: flow.observed, mono: true, title: flow.observed },
    {
      k: 'exporter',
      v: flow.exporter_name ? `${flow.exporter_name} (${flow.exporter})` : flow.exporter,
      mono: true,
      title: flow.exporter,
    },
    {
      k: 'src_addr',
      v: (
        <>
          {flow.src_addr}
          <Hostname ip={flow.src_addr} />
        </>
      ),
      mono: true,
      title: flow.src_addr,
    },
    { k: 'src_port', v: String(flow.src_port), mono: true },
    {
      k: 'dst_addr',
      v: (
        <>
          {flow.dst_addr}
          <Hostname ip={flow.dst_addr} />
        </>
      ),
      mono: true,
      title: flow.dst_addr,
    },
    { k: 'dst_port', v: String(flow.dst_port), mono: true },
    { k: 'proto', v: `${fmt.proto(flow.proto)} (${flow.proto})`, mono: true },
    { k: 'bytes', v: `${fmt.bytes(flow.bytes)} (${flow.bytes})`, mono: true },
    { k: 'packets', v: fmt.num(flow.packets), mono: true },
    { k: 'input_ifindex', v: String(flow.input_ifindex), mono: true },
    { k: 'output_ifindex', v: String(flow.output_ifindex), mono: true },
    { k: 'src_as', v: flow.src_as ? `AS${flow.src_as}` : '—', mono: true },
    { k: 'dst_as', v: flow.dst_as ? `AS${flow.dst_as}` : '—', mono: true },
    {
      k: 'tcp_flags',
      v:
        flow.proto === 6
          ? `${decodeFlags(flow.tcp_flags)} (0x${flow.tcp_flags.toString(16).padStart(2, '0')})`
          : '—',
      mono: true,
      title: `0x${flow.tcp_flags.toString(16).padStart(2, '0')} = ${flow.tcp_flags}`,
    },
    { k: 'source', v: flow.source, mono: true },
  ]
  return (
    <div className="px-3 pb-3 pt-1 border-t border-line-soft bg-surface">
      <div className="flex items-baseline gap-3 pb-2 pt-1">
        <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold">
          raw record
        </span>
        <span className="font-mono text-[11px] text-faint">
          service ·{' '}
          <ServiceLabel proto={flow.proto} port={flow.dst_port} fallback="—" />
        </span>
      </div>
      <dl className="grid grid-cols-2 md:grid-cols-3 border-l border-t border-line-soft">
        {fields.map((f) => (
          <div
            key={f.k}
            className="px-3 py-1.5 border-r border-b border-line-soft min-w-0 overflow-hidden"
          >
            <dt className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-px">
              {f.k}
            </dt>
            <dd
              title={f.title}
              className={`text-[12px] text-text truncate ${f.mono ? 'font-mono' : ''}`}
            >
              {f.v}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  )
}

// decodeFlags turns the 8-bit TCP flag byte (RFC 793 + RFC 3168) into
// the operator-friendly stack: "SYN+ACK", "FIN+ACK", "RST", "ACK",
// etc. ECN bits (CWR / ECE) are decoded but very rarely matter in
// this context — included for completeness.
function decodeFlags(flags: number): string {
  if (flags === 0) return '—'
  const parts: string[] = []
  if (flags & 0x80) parts.push('CWR')
  if (flags & 0x40) parts.push('ECE')
  if (flags & 0x20) parts.push('URG')
  if (flags & 0x10) parts.push('ACK')
  if (flags & 0x08) parts.push('PSH')
  if (flags & 0x04) parts.push('RST')
  if (flags & 0x02) parts.push('SYN')
  if (flags & 0x01) parts.push('FIN')
  // Reorder to the ordering operators expect: SYN before ACK, FIN
  // last, RST stands alone visually.
  const order = ['SYN', 'ACK', 'PSH', 'URG', 'CWR', 'ECE', 'FIN', 'RST']
  parts.sort((a, b) => order.indexOf(a) - order.indexOf(b))
  return parts.join('+')
}
