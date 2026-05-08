import { useQuery } from '@tanstack/react-query'
import { Fragment, useEffect, useState, type ReactNode } from 'react'
import { api, fmt, labelExporter, labelInterface } from '../api'
import type {
  Device,
  DeviceInventory,
  InterfaceRow,
  RecentFlow,
  TopService,
  TopTalker,
} from '../api'
import { InterfaceChart } from './InterfaceChart'
import { ServiceLabel } from './ServiceLabel'
import {
  rangeLabel,
  rangeSeconds,
  toApi,
  type TimeRange,
} from '../timeRange'
import { Th, useTableSort, type SortColumns } from './sortable'
import { formatModel } from '../lib/oidModels'
import type { Filter } from '../filters'

// NavigateToFlows is the cross-tab navigation primitive injected by
// the App shell. Devices "Investigate →" buttons use it to deep-link
// into Flows with the supplied filter chips pre-applied.
type NavigateToFlows = (filters: Filter[]) => void

const DEVICE_IFACE_COLS: SortColumns<InterfaceRow> = {
  interface: (r) => labelInterface(r).primary,
  in_latest: (r) => r.in_bps_latest,
  out_latest: (r) => r.out_bps_latest,
  in_peak: (r) => r.in_bps_peak,
  out_peak: (r) => r.out_bps_peak,
  last_seen: (r) => r.last_seen,
}

const DEVICE_FLOW_COLS: SortColumns<RecentFlow> = {
  observed: (r) => r.observed,
  source: (r) => r.source,
  src_dst: (r) => `${r.src_addr}:${r.src_port} ${r.dst_addr}:${r.dst_port}`,
  proto: (r) => r.proto,
  packets: (r) => r.packets,
  bytes: (r) => r.bytes,
}

const RAIL_WIDTH_KEY = 'flowscope.devices.railWidth'
const RAIL_WIDTH_DEFAULT = 280
const RAIL_WIDTH_MIN = 220
const RAIL_WIDTH_MAX = 480

function useResizableWidth(key: string, def: number, min: number, max: number) {
  const [width, setWidth] = useState<number>(() => {
    try {
      const saved = Number(localStorage.getItem(key))
      if (Number.isFinite(saved) && saved >= min && saved <= max) return saved
    } catch {
      // localStorage unavailable (private mode, SSR) — fall through to default
    }
    return def
  })
  useEffect(() => {
    try {
      localStorage.setItem(key, String(width))
    } catch {
      // ignore
    }
  }, [key, width])
  const onMouseDown = (e: React.MouseEvent) => {
    e.preventDefault()
    const startX = e.clientX
    const startW = width
    const onMove = (ev: MouseEvent) => {
      const next = Math.max(min, Math.min(max, startW + (ev.clientX - startX)))
      setWidth(next)
    }
    const onUp = () => {
      document.removeEventListener('mousemove', onMove)
      document.removeEventListener('mouseup', onUp)
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
    }
    document.addEventListener('mousemove', onMove)
    document.addEventListener('mouseup', onUp)
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
  }
  return { width, onMouseDown }
}

// TwoLine renders a primary value with a smaller, faint secondary
// underneath. Used everywhere the SNMP enrichment exposes a
// human-friendly name plus a stable identifier (sys_name + IP, or
// if_descr + if_alias). The wrapper applies `truncate` so wide
// values don't blow out narrow cells.
function TwoLine({
  primary,
  secondary,
  primaryClass = 'font-mono',
  secondaryClass = 'font-mono italic text-faint',
}: {
  primary: ReactNode
  secondary?: ReactNode
  primaryClass?: string
  secondaryClass?: string
}) {
  return (
    <div className="min-w-0 max-w-full">
      <div className={`${primaryClass} truncate`}>{primary}</div>
      {secondary && (
        <div className={`${secondaryClass} text-[10.5px] truncate`}>{secondary}</div>
      )}
    </div>
  )
}

// Devices tab — directory of exporters seen in flows on the left,
// feature view of the selected exporter on the right with three
// sub-tabs (Summary / Interfaces / Flows). SNMP-driven enrichment
// (model, OS, uptime, location) lands in a later slice; for now the
// view shows what we know from observed flows + counter samples.
export function Devices({
  range,
  rangeKey,
  onNavigateToFlows,
}: {
  range: TimeRange
  rangeKey: unknown
  onNavigateToFlows: NavigateToFlows
}) {
  const apiRange = toApi(range)
  const list = useQuery({
    queryKey: ['devices', rangeKey],
    queryFn: () => api.devices(apiRange),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const devices = list.data?.devices ?? []
  const [selected, setSelected] = useState<string | null>(null)

  // Auto-select the first device on first load if nothing is selected.
  if (selected === null && devices.length > 0) {
    setSelected(devices[0].exporter)
  }

  const rail = useResizableWidth(
    RAIL_WIDTH_KEY,
    RAIL_WIDTH_DEFAULT,
    RAIL_WIDTH_MIN,
    RAIL_WIDTH_MAX,
  )

  return (
    <div className="grid h-full" style={{ gridTemplateColumns: `${rail.width}px 1fr` }}>
      <Directory
        devices={devices}
        selected={selected}
        onSelect={setSelected}
        loading={list.isLoading}
        range={range}
        onResizeStart={rail.onMouseDown}
      />
      <Feature
        exporter={selected}
        range={range}
        rangeKey={rangeKey}
        onNavigateToFlows={onNavigateToFlows}
      />
    </div>
  )
}

/* ----------------------------- Directory ----------------------------- */

function Directory({
  devices,
  selected,
  onSelect,
  loading,
  range,
  onResizeStart,
}: {
  devices: Device[]
  selected: string | null
  onSelect: (e: string) => void
  loading: boolean
  range: TimeRange
  onResizeStart: (e: React.MouseEvent) => void
}) {
  const [filter, setFilter] = useState('')
  const filtered = devices.filter((d) =>
    filter === '' ? true : d.exporter.includes(filter),
  )
  return (
    <aside className="relative border-r border-line bg-surface flex flex-col overflow-hidden">
      <div className="p-3 border-b border-line">
        <input
          placeholder="filter exporters…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="w-full h-7 px-2 bg-ink border border-line text-[12.5px] outline-none focus:border-accent"
        />
      </div>
      <div className="overflow-auto">
        <div className="px-3 py-2 text-[10px] uppercase tracking-[0.16em] font-mono text-faint flex justify-between">
          <span>exporters</span>
          <span>{loading ? '…' : `${filtered.length}/${devices.length}`}</span>
        </div>
        {filtered.length === 0 && !loading && (
          <div className="px-3 py-6 text-[12px] font-mono text-dim">
            {devices.length === 0
              ? 'no exporters seen in window'
              : 'no matches'}
          </div>
        )}
        {filtered.map((d) => (
          <DirectoryRow
            key={d.exporter}
            d={d}
            active={d.exporter === selected}
            onSelect={() => onSelect(d.exporter)}
            seconds={rangeSeconds(range)}
          />
        ))}
      </div>
      <div
        role="separator"
        aria-label="Resize directory"
        aria-orientation="vertical"
        onMouseDown={onResizeStart}
        className="absolute top-0 right-0 h-full w-1.5 -mr-[3px] cursor-col-resize z-10 group"
      >
        <div className="h-full w-px ml-auto bg-line group-hover:bg-accent group-active:bg-accent transition-colors" />
      </div>
    </aside>
  )
}

function DirectoryRow({
  d,
  active,
  onSelect,
  seconds,
}: {
  d: Device
  active: boolean
  onSelect: () => void
  seconds: number
}) {
  const since = secondsSince(d.last_seen)
  const dot =
    since < 60 ? 'bg-ok' : since < 300 ? 'bg-warn' : 'bg-crit'
  const lbl = labelExporter(d)
  return (
    <button
      onClick={onSelect}
      className={`w-full text-left px-3 py-2 border-b border-line-soft flex items-center gap-3 hover:bg-hover ${
        active ? 'bg-accent-wash' : ''
      }`}
    >
      <span className={`w-1.5 h-1.5 rounded-full ${dot} shrink-0 mt-0.5 self-start`} />
      <div className="min-w-0 flex-1">
        <div className="font-mono text-[12.5px] truncate">{lbl.primary}</div>
        {lbl.secondary && (
          <div className="font-mono text-[10.5px] text-faint truncate">{lbl.secondary}</div>
        )}
      </div>
      <span className="ml-auto font-mono text-[10.5px] text-faint shrink-0 tabular">
        {fmt.bps((d.bytes * 8) / Math.max(1, seconds))}
      </span>
    </button>
  )
}

/* ----------------------------- Feature view ----------------------------- */

type SubTab = 'summary' | 'interfaces' | 'flows'

function Feature({
  exporter,
  range,
  rangeKey,
  onNavigateToFlows,
}: {
  exporter: string | null
  range: TimeRange
  rangeKey: unknown
  onNavigateToFlows: NavigateToFlows
}) {
  const [sub, setSub] = useState<SubTab>('summary')
  if (!exporter) {
    return (
      <div className="p-8 text-dim font-mono text-[13px]">
        Select an exporter from the directory.
      </div>
    )
  }
  return (
    <article className="overflow-auto">
      <FeatureHeader exporter={exporter} range={range} rangeKey={rangeKey} />
      <SubTabs active={sub} onChange={setSub} />
      <div>
        {sub === 'summary' && <SummaryTab exporter={exporter} range={range} rangeKey={rangeKey} />}
        {sub === 'interfaces' && <InterfacesTab exporter={exporter} range={range} rangeKey={rangeKey} />}
        {sub === 'flows' && (
          <FlowsTab exporter={exporter} onNavigateToFlows={onNavigateToFlows} />
        )}
      </div>
    </article>
  )
}

function FeatureHeader({
  exporter,
  range,
  rangeKey,
}: {
  exporter: string
  range: TimeRange
  rangeKey: unknown
}) {
  const apiRange = toApi(range)
  const q = useQuery({
    queryKey: ['device', exporter, rangeKey],
    queryFn: () => api.device(exporter, apiRange),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const inv = useQuery({
    queryKey: ['device-inventory', exporter],
    // 404 is expected before the snmp service has walked this exporter.
    // catch and resolve to undefined so the UI shows the "no SNMP yet"
    // banner instead of a hard error.
    queryFn: () =>
      api
        .deviceInventory(exporter)
        .catch(() => undefined as DeviceInventory | undefined),
    refetchInterval: 30_000,
  })
  const d = q.data
  const i = inv.data
  const since = d ? secondsSince(d.last_seen) : Infinity
  const status =
    since < 60 ? 'online' : since < 300 ? 'silent' : 'offline'
  const tone =
    status === 'online' ? 'text-ok' : status === 'silent' ? 'text-warn' : 'text-crit'
  const headline = i?.sys_name || exporter
  return (
    <header className="px-6 pt-6 pb-4 border-b border-line bg-surface">
      <div className="flex items-center gap-3 text-[10.5px] uppercase tracking-[0.1em] font-semibold text-dim mb-1">
        <span className={tone}>● {status}</span>
        <span className="font-mono text-[10.5px] text-faint normal-case tracking-[0.02em]">
          last seen {d ? fmt.time(d.last_seen).slice(11, 19) + 'Z' : '—'}
        </span>
        <span className="font-mono text-[10.5px] text-faint normal-case tracking-[0.02em]">
          first seen {d ? fmt.time(d.first_seen).slice(11, 19) + 'Z' : '—'}
        </span>
        {i && (
          <span className="font-mono text-[10.5px] text-faint normal-case tracking-[0.02em]">
            snmp {fmt.time(i.polled_at).slice(11, 19)}Z
          </span>
        )}
      </div>
      <h1 className="font-mono text-[26px] font-semibold tracking-tight text-text leading-[1.1]">
        {headline}
        {i?.sys_name && (
          <span className="font-mono text-[14px] font-normal text-faint ml-3">
            {exporter}
          </span>
        )}
      </h1>
      <p className="text-[13.5px] text-dim mt-1.5 max-w-[78ch] leading-[1.5]">
        {i ? (
          <>
            <span className="text-text">{shortDescr(i.sys_descr)}</span>
            {i.sys_location && (
              <>
                {' · '}
                <span>{i.sys_location}</span>
              </>
            )}
            {' · uptime '}
            <span className="text-text">{formatUptime(i.sys_uptime_ms)}</span>
            {d?.iface_count ? (
              <>
                {' · '}
                <span className="text-text font-medium">
                  {fmt.num(d.iface_count)} interface{d.iface_count === 1 ? '' : 's'}
                </span>{' '}
                emitting counter samples
              </>
            ) : null}
            .
          </>
        ) : (
          <>
            Exporter inferred from observed flow records. SNMP has not yet walked
            this device — the snmp service polls every 15 min once an exporter
            shows up in flows.
          </>
        )}
      </p>
      <SpecRow d={d} i={i} range={range} />
    </header>
  )
}

function SpecRow({ d, i, range }: { d?: Device; i?: DeviceInventory; range: TimeRange }) {
  const seconds = Math.max(1, rangeSeconds(range))
  const winLabel = rangeLabel(range)
  const cells: { k: string; v: string; mono?: boolean }[] = [
    { k: 'address', v: d?.exporter ?? '—', mono: true },
    { k: 'model', v: i ? formatModel(i.sys_object_id) : '—' },
    { k: 'snmp ifaces', v: i ? fmt.num(i.iface_count) : '—', mono: true },
    { k: 'flow ifaces', v: d ? fmt.num(d.iface_count) : '—', mono: true },
    { k: `volume · ${winLabel}`, v: d ? fmt.bytes(d.bytes) : '—' },
    { k: 'avg rate', v: d ? fmt.bps((d.bytes * 8) / seconds) : '—' },
  ]
  return (
    <div className="grid grid-cols-3 md:grid-cols-6 mt-4 border-t border-l border-line">
      {cells.map((c, i) => (
        <div
          key={i}
          className="px-3 py-2.5 border-r border-b border-line min-w-0 overflow-hidden"
        >
          <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-0.5">
            {c.k}
          </div>
          <div
            title={c.v}
            className={`text-[14px] tabular text-text truncate ${c.mono ? 'font-mono' : ''}`}
          >
            {c.v}
          </div>
        </div>
      ))}
    </div>
  )
}

function SubTabs({ active, onChange }: { active: SubTab; onChange: (s: SubTab) => void }) {
  return (
    <div className="flex border-b border-line bg-ink">
      <Tab id="summary" active={active} onChange={onChange}>Summary</Tab>
      <Tab id="interfaces" active={active} onChange={onChange}>Interfaces</Tab>
      <Tab id="flows" active={active} onChange={onChange}>Flows</Tab>
    </div>
  )
}

function Tab({
  id,
  active,
  onChange,
  children,
}: {
  id: SubTab
  active: SubTab
  onChange: (s: SubTab) => void
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

/* ----------------------------- Summary tab ----------------------------- */

function SummaryTab({
  exporter,
  range,
  rangeKey,
}: {
  exporter: string
  range: TimeRange
  rangeKey: unknown
}) {
  const winLabel = rangeLabel(range)
  return (
    <div className="px-6 py-5 space-y-5">
      <Section title="Inventory" sub="snmp · v2c" right="SOURCE · SNMP">
        <InventoryPanel exporter={exporter} />
      </Section>
      <Section
        title="Recent flows reported by this exporter"
        sub="last 60s · forwarded traffic, not addressed to this device"
      >
        <RecentFlowsMini exporter={exporter} limit={6} />
      </Section>
      <Section title="Top interfaces" sub={`counter samples · ${winLabel}`} right="SOURCE · COUNTERS">
        <InterfacesMini exporter={exporter} range={range} rangeKey={rangeKey} />
      </Section>
    </div>
  )
}

function InventoryPanel({ exporter }: { exporter: string }) {
  const q = useQuery({
    queryKey: ['device-inventory', exporter],
    queryFn: () =>
      api
        .deviceInventory(exporter)
        .catch(() => undefined as DeviceInventory | undefined),
    refetchInterval: 30_000,
  })
  if (q.isLoading) return <p className="text-dim font-mono text-[12px]">loading…</p>
  const i = q.data
  if (!i) {
    return (
      <p className="text-dim font-mono text-[12px]">
        no SNMP data yet · the snmp service polls every 15 min after the
        exporter first appears in flows · check{' '}
        <code className="bg-raise px-1 text-text">FLOWSCOPE_SNMP_COMMUNITY</code> on the snmp service if a real network is reachable
      </p>
    )
  }
  const cells: { k: string; v: string; mono?: boolean }[] = [
    { k: 'hostname', v: i.sys_name || '—' },
    { k: 'description', v: shortDescr(i.sys_descr) },
    { k: 'object id', v: i.sys_object_id || '—', mono: true },
    { k: 'uptime', v: formatUptime(i.sys_uptime_ms) },
    { k: 'location', v: i.sys_location || '—' },
    { k: 'contact', v: i.sys_contact || '—' },
    { k: 'last poll', v: fmt.time(i.polled_at).slice(11, 19) + 'Z', mono: true },
    { k: 'poll status', v: i.poll_status, mono: true },
  ]
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 border-l border-t border-line">
      {cells.map((c, idx) => (
        <div
          key={idx}
          className="px-3 py-2.5 border-r border-b border-line min-w-0 overflow-hidden"
        >
          <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-0.5">
            {c.k}
          </div>
          <div
            title={c.v}
            className={`text-[13px] text-text leading-[1.3] truncate ${c.mono ? 'font-mono' : ''}`}
          >
            {c.v}
          </div>
        </div>
      ))}
    </div>
  )
}

function Section({
  title,
  sub,
  right,
  children,
}: {
  title: string
  sub?: string
  right?: string
  children: ReactNode
}) {
  return (
    <section>
      <div className="flex items-baseline gap-3 pb-2 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
        {sub && <span className="font-mono text-[11px] text-faint">{sub}</span>}
        {right && (
          <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-accent">{right}</span>
        )}
      </div>
      <div className="pt-2">{children}</div>
    </section>
  )
}

function RecentFlowsMini({ exporter, limit }: { exporter: string; limit: number }) {
  const q = useQuery({
    queryKey: ['device-recent', exporter, limit],
    queryFn: () => api.recentFlows(limit, exporter),
    refetchInterval: 2000,
  })
  const flows = q.data?.flows ?? []
  if (q.isLoading) return <p className="text-dim font-mono text-[12px]">loading…</p>
  if (flows.length === 0) {
    return <p className="text-dim font-mono text-[12px]">no flows yet</p>
  }
  return (
    <ul className="font-mono text-[12px] space-y-1">
      {flows.map((f, i) => (
        <li key={i} className="flex gap-3 text-dim">
          <span className="text-faint">{fmt.time(f.observed).slice(11, 19)}</span>
          <span className="text-accent">{fmt.proto(f.proto)}</span>
          <span className="truncate">
            {f.src_addr}:{f.src_port} <span className="text-faint">→</span>{' '}
            {f.dst_addr}:{f.dst_port}
          </span>
          <span className="ml-auto text-text tabular shrink-0">{fmt.bytes(f.bytes)}</span>
        </li>
      ))}
    </ul>
  )
}

function InterfacesMini({
  exporter,
  range,
  rangeKey,
}: {
  exporter: string
  range: TimeRange
  rangeKey: unknown
}) {
  const apiRange = toApi(range)
  const q = useQuery({
    queryKey: ['device-ifaces', exporter, rangeKey],
    queryFn: () => api.interfaces(apiRange, exporter),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const ifaces = q.data?.interfaces ?? []
  // Mini view shows top 10 by current ingress rate so the user sees
  // the busy interfaces; the full sortable table lives on the
  // Interfaces sub-tab.
  const top = [...ifaces]
    .sort((a, b) => b.in_bps_latest - a.in_bps_latest)
    .slice(0, 10)
  if (q.isLoading) return <p className="text-dim font-mono text-[12px]">loading…</p>
  if (ifaces.length === 0) {
    return (
      <p className="text-dim font-mono text-[12px]">
        no counter samples · sFlow / gNMI required for authoritative rates
      </p>
    )
  }
  return (
    <table className="w-full table-fixed">
      <colgroup>
        <col style={{ width: '40%' }} />
        <col />
        <col />
        <col />
        <col />
        <col style={{ width: '90px' }} />
      </colgroup>
      <thead>
        <tr>
          <th>interface</th>
          <th className="r">in (latest)</th>
          <th className="r">out (latest)</th>
          <th className="r">in peak</th>
          <th className="r">out peak</th>
          <th className="r">last seen</th>
        </tr>
      </thead>
      <tbody>
        {top.map((i: InterfaceRow) => {
          const lbl = labelInterface(i)
          return (
            <tr key={i.ifindex} className="hover:bg-surface">
              <td>
                <TwoLine primary={lbl.primary} secondary={lbl.secondary || undefined} />
              </td>
              <td className="r n">{fmt.bps(i.in_bps_latest)}</td>
              <td className="r n">{fmt.bps(i.out_bps_latest)}</td>
              <td className="r n text-accent">{fmt.bps(i.in_bps_peak)}</td>
              <td className="r n text-ok">{fmt.bps(i.out_bps_peak)}</td>
              <td className="r n text-faint">{fmt.time(i.last_seen).slice(11, 19)}</td>
            </tr>
          )
        })}
      </tbody>
    </table>
  )
}

/* ----------------------------- Interfaces tab ----------------------------- */

function InterfacesTab({
  exporter,
  range,
  rangeKey,
}: {
  exporter: string
  range: TimeRange
  rangeKey: unknown
}) {
  // Multiple charts can be open at once. Stored as a Set keyed by
  // ifindex. Reset whenever the selected exporter changes — many
  // devices share interface names / ifindexes, so a "stale" open
  // chart from a previous device would silently show that other
  // device's data.
  const [activeIfindexes, setActiveIfindexes] = useState<Set<number>>(
    () => new Set(),
  )
  useEffect(() => {
    setActiveIfindexes(new Set())
  }, [exporter])
  const toggleChart = (ifindex: number) => {
    setActiveIfindexes((prev) => {
      const next = new Set(prev)
      if (next.has(ifindex)) {
        next.delete(ifindex)
      } else {
        next.add(ifindex)
      }
      return next
    })
  }
  const apiRange = toApi(range)
  const winLabel = rangeLabel(range)
  const q = useQuery({
    queryKey: ['device-ifaces-full', exporter, rangeKey],
    queryFn: () => api.interfaces(apiRange, exporter),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const ifaces = q.data?.interfaces ?? []
  // Default: sort by interface name asc — gives natural ABC order
  // (Ethernet1 < Ethernet2 < … < Ethernet10 < Port-Channel1 < …)
  // because useTableSort uses localeCompare(numeric: true).
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(ifaces, DEVICE_IFACE_COLS, {
    key: 'interface',
    dir: 'asc',
  })
  const thProps = (k: string) => ({
    sortKey: k,
    active: sortKey === k,
    dir: sortDir,
    onToggle: toggle,
  })
  return (
    <div>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          All interfaces
        </span>
        <span className="font-mono text-[11px] text-faint">
          {q.isLoading ? 'loading…' : `${ifaces.length} active · ${winLabel}`}
        </span>
        <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-accent">
          SOURCE · COUNTERS
        </span>
      </div>
      {ifaces.length === 0 ? (
        <div className="px-4 py-8 text-center text-[12px] font-mono text-dim">
          no counter samples for this exporter · NetFlow-only sources do not produce them
        </div>
      ) : (
        <table className="w-full table-fixed">
          <colgroup>
            <col style={{ width: '34%' }} />
            <col />
            <col />
            <col />
            <col />
            <col style={{ width: '90px' }} />
            <col style={{ width: '80px' }} />
          </colgroup>
          <thead>
            <tr>
              <Th {...thProps('interface')}>interface</Th>
              <Th {...thProps('in_latest')} align="r">in (latest)</Th>
              <Th {...thProps('out_latest')} align="r">out (latest)</Th>
              <Th {...thProps('in_peak')} align="r">in peak</Th>
              <Th {...thProps('out_peak')} align="r">out peak</Th>
              <Th {...thProps('last_seen')} align="r">last seen</Th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sortedRows.map((i: InterfaceRow) => {
              const lbl = labelInterface(i)
              const isActive = activeIfindexes.has(i.ifindex)
              return (
                <Fragment key={i.ifindex}>
                  <tr className="hover:bg-surface">
                    <td>
                      <TwoLine primary={lbl.primary} secondary={lbl.secondary || undefined} />
                    </td>
                    <td className="r n">{fmt.bps(i.in_bps_latest)}</td>
                    <td className="r n">{fmt.bps(i.out_bps_latest)}</td>
                    <td className="r n text-accent">{fmt.bps(i.in_bps_peak)}</td>
                    <td className="r n text-ok">{fmt.bps(i.out_bps_peak)}</td>
                    <td className="r n text-faint">{fmt.time(i.last_seen).slice(11, 19)}</td>
                    <td className="r">
                      <button
                        className={`text-[11px] font-mono ${isActive ? 'text-text' : 'text-accent hover:underline'}`}
                        onClick={() => toggleChart(i.ifindex)}
                      >
                        {isActive ? '× close' : 'chart →'}
                      </button>
                    </td>
                  </tr>
                  {isActive && (
                    <tr>
                      <td colSpan={7} style={{ padding: 0, borderBottom: 'none' }}>
                        <InterfaceChart exporter={exporter} ifindex={i.ifindex} range={range} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}

/* ----------------------------- Flows tab ----------------------------- */

function FlowsTab({
  exporter,
  onNavigateToFlows,
}: {
  exporter: string
  onNavigateToFlows: NavigateToFlows
}) {
  const q = useQuery({
    queryKey: ['device-flows', exporter],
    queryFn: () => api.recentFlows(50, exporter),
    refetchInterval: 2000,
  })
  const flows = q.data?.flows ?? []
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(flows, DEVICE_FLOW_COLS, {
    key: 'observed',
    dir: 'desc',
  })
  const thProps = (k: string) => ({
    sortKey: k,
    active: sortKey === k,
    dir: sortDir,
    onToggle: toggle,
  })
  const investigateExporter = () =>
    onNavigateToFlows([{ key: 'exporter', value: exporter }])
  return (
    <div>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          Recent flows reported by this exporter
        </span>
        <span className="font-mono text-[11px] text-faint">
          {q.isLoading ? 'loading…' : `${flows.length} most recent`}
        </span>
        <button
          onClick={investigateExporter}
          className="ml-auto font-mono text-[10.5px] tracking-[0.06em] text-accent hover:underline"
        >
          Investigate on Flows →
        </button>
      </div>
      <p className="px-4 py-2 text-[11.5px] text-dim border-b border-line bg-surface leading-[1.5]">
        Switches export flows for traffic they <span className="text-text">forward</span>, not just
        traffic addressed to themselves. Source and destination IPs here are endpoints elsewhere on
        the network — this device just observed the conversation.
      </p>
      {flows.length === 0 ? (
        <div className="px-4 py-8 text-center text-[12px] font-mono text-dim">
          no flows yet for this exporter
        </div>
      ) : (
        <table className="w-full table-fixed">
          <colgroup>
            <col style={{ width: '110px' }} />
            <col style={{ width: '90px' }} />
            <col />
            <col style={{ width: '70px' }} />
            <col style={{ width: '90px' }} />
            <col style={{ width: '90px' }} />
          </colgroup>
          <thead>
            <tr>
              <Th {...thProps('observed')}>time</Th>
              <Th {...thProps('source')}>source</Th>
              <Th {...thProps('src_dst')}>src → dst</Th>
              <Th {...thProps('proto')}>proto</Th>
              <Th {...thProps('packets')} align="r">packets</Th>
              <Th {...thProps('bytes')} align="r">bytes</Th>
            </tr>
          </thead>
          <tbody>
            {sortedRows.map((f: RecentFlow, i: number) => (
              <tr key={i} className="hover:bg-surface">
                <td className="n text-faint">{fmt.time(f.observed).slice(11, 23)}</td>
                <td className="n text-dim">{f.source}</td>
                <td className="n truncate">
                  {f.src_addr}:{f.src_port}{' '}
                  <span className="text-faint">→</span>{' '}
                  {f.dst_addr}:{f.dst_port}
                </td>
                <td className="font-mono text-accent">{fmt.proto(f.proto)}</td>
                <td className="r n">{fmt.num(f.packets)}</td>
                <td className="r n">{fmt.bytes(f.bytes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="grid grid-cols-1 lg:grid-cols-2 border-t border-line">
        <MiniTalkers exporter={exporter} onNavigateToFlows={onNavigateToFlows} />
        <MiniServices exporter={exporter} onNavigateToFlows={onNavigateToFlows} />
      </div>
    </div>
  )
}

/* ----------------------- Top-5 mini panels ----------------------- */

function MiniPanelHead({
  title,
  sub,
  onInvestigate,
  borderRight,
}: {
  title: string
  sub: string
  onInvestigate: () => void
  borderRight?: boolean
}) {
  return (
    <div
      className={`flex items-baseline gap-3 px-4 py-3 border-b border-line ${
        borderRight ? 'lg:border-r' : ''
      }`}
    >
      <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
        {title}
      </span>
      <span className="font-mono text-[11px] text-faint">{sub}</span>
      <button
        onClick={onInvestigate}
        className="ml-auto font-mono text-[10.5px] tracking-[0.06em] text-accent hover:underline"
      >
        Investigate →
      </button>
    </div>
  )
}

function MiniBars<T>({
  rows,
  loading,
  empty,
  keyOf,
  renderLeft,
  renderRight,
  valueOf,
}: {
  rows: T[]
  loading: boolean
  empty: string
  keyOf: (r: T) => string
  renderLeft: (r: T) => ReactNode
  renderRight: (r: T) => ReactNode
  valueOf: (r: T) => number
}) {
  if (loading) {
    return <div className="px-4 py-6 text-faint font-mono text-[12px]">loading…</div>
  }
  if (rows.length === 0) {
    return <div className="px-4 py-6 text-center text-[12px] font-mono text-dim">{empty}</div>
  }
  const total = rows.reduce((a, r) => a + valueOf(r), 0)
  return (
    <ul>
      {rows.map((r) => {
        const v = valueOf(r)
        const pct = total > 0 ? (v / total) * 100 : 0
        return (
          <li
            key={keyOf(r)}
            className="px-4 py-2 border-b border-line-soft last:border-b-0 hover:bg-surface"
          >
            <div className="flex items-baseline justify-between gap-3">
              <div className="min-w-0 truncate">{renderLeft(r)}</div>
              <div className="font-mono text-[12px] tabular text-text shrink-0">
                {renderRight(r)}
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
  )
}

function MiniTalkers({
  exporter,
  onNavigateToFlows,
}: {
  exporter: string
  onNavigateToFlows: NavigateToFlows
}) {
  const qs = new URLSearchParams({ exporter })
  const q = useQuery({
    queryKey: ['device-mini-talkers', exporter],
    queryFn: () => api.topTalkers(qs, 300, 5, 'bytes'),
    refetchInterval: 5000,
  })
  return (
    <section className="lg:border-r lg:border-line">
      <MiniPanelHead
        title="Top talkers"
        sub="this exporter · 5min · by bytes"
        onInvestigate={() =>
          onNavigateToFlows([{ key: 'exporter', value: exporter }])
        }
      />
      <MiniBars
        rows={q.data?.rows ?? []}
        loading={q.isLoading}
        empty="no talkers in last 5 min"
        keyOf={(r: TopTalker) => `${r.src_addr}>${r.dst_addr}`}
        renderLeft={(r) => (
          <span className="font-mono text-[12px]">
            <span className="text-text">{r.src_addr}</span>{' '}
            <span className="text-faint">→</span>{' '}
            <span className="text-text">{r.dst_addr}</span>
          </span>
        )}
        renderRight={(r) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </section>
  )
}

function MiniServices({
  exporter,
  onNavigateToFlows,
}: {
  exporter: string
  onNavigateToFlows: NavigateToFlows
}) {
  const qs = new URLSearchParams({ exporter })
  const q = useQuery({
    queryKey: ['device-mini-services', exporter],
    queryFn: () => api.topServices(qs, 300, 5, 'bytes'),
    refetchInterval: 5000,
  })
  return (
    <section>
      <MiniPanelHead
        title="Top services"
        sub="this exporter · 5min · by bytes"
        onInvestigate={() =>
          onNavigateToFlows([{ key: 'exporter', value: exporter }])
        }
      />
      <MiniBars
        rows={q.data?.rows ?? []}
        loading={q.isLoading}
        empty="no services in last 5 min"
        keyOf={(r: TopService) => `${r.dst_port}_${r.proto}`}
        renderLeft={(r) => (
          <span className="font-mono text-[12px] text-text">
            <ServiceLabel proto={r.proto} port={r.dst_port} />{' '}
            <span className="text-faint">
              · {fmt.proto(r.proto)} {r.dst_port}
            </span>
          </span>
        )}
        renderRight={(r) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </section>
  )
}

/* ----------------------------- Helpers ----------------------------- */

function secondsSince(iso: string): number {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return Infinity
  return Math.max(0, (Date.now() - t) / 1000)
}

// shortDescr trims sysDescr to its first line and a sane length so
// the dek and spec row don't blow out. Real network gear returns
// 200+ char strings full of carriage returns.
function shortDescr(s: string): string {
  if (!s) return '—'
  const firstLine = s.split(/[\r\n]/)[0].trim()
  if (firstLine.length > 80) return firstLine.slice(0, 77) + '…'
  return firstLine
}

// formatUptime turns ms-since-boot into "137d 4h 22m" form.
function formatUptime(ms: number): string {
  if (!ms) return '—'
  const sec = Math.floor(ms / 1000)
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d}d ${h}h ${m}m`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

