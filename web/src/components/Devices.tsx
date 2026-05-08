import { useQuery } from '@tanstack/react-query'
import { Fragment, useState, type ReactNode } from 'react'
import { api, fmt, labelExporter, labelInterface } from '../api'
import type { Device, DeviceInventory, InterfaceRow, RecentFlow } from '../api'
import { InterfaceChart } from './InterfaceChart'
import {
  rangeLabel,
  rangeSeconds,
  toApi,
  useTimeRange,
  type TimeRange,
} from '../timeRange'
import { TimeRangeSelector } from './TimeRangeSelector'

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
export function Devices() {
  const tr = useTimeRange('dv')
  const apiRange = toApi(tr.range)
  const list = useQuery({
    queryKey: ['devices', tr.queryKey],
    queryFn: () => api.devices(apiRange),
    refetchInterval: tr.range.kind === 'preset' ? 5000 : false,
  })
  const devices = list.data?.devices ?? []
  const [selected, setSelected] = useState<string | null>(null)

  // Auto-select the first device on first load if nothing is selected.
  if (selected === null && devices.length > 0) {
    setSelected(devices[0].exporter)
  }

  return (
    <div className="grid h-full" style={{ gridTemplateColumns: '280px 1fr' }}>
      <Directory
        devices={devices}
        selected={selected}
        onSelect={setSelected}
        loading={list.isLoading}
        range={tr.range}
      />
      <Feature
        exporter={selected}
        range={tr.range}
        onRangeChange={tr.set}
        rangeKey={tr.queryKey}
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
}: {
  devices: Device[]
  selected: string | null
  onSelect: (e: string) => void
  loading: boolean
  range: TimeRange
}) {
  const [filter, setFilter] = useState('')
  const filtered = devices.filter((d) =>
    filter === '' ? true : d.exporter.includes(filter),
  )
  return (
    <aside className="border-r border-line bg-surface flex flex-col overflow-hidden">
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
  onRangeChange,
  rangeKey,
}: {
  exporter: string | null
  range: TimeRange
  onRangeChange: (r: TimeRange) => void
  rangeKey: unknown
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
      <FeatureHeader
        exporter={exporter}
        range={range}
        onRangeChange={onRangeChange}
        rangeKey={rangeKey}
      />
      <SubTabs active={sub} onChange={setSub} />
      <div>
        {sub === 'summary' && <SummaryTab exporter={exporter} range={range} rangeKey={rangeKey} />}
        {sub === 'interfaces' && <InterfacesTab exporter={exporter} range={range} rangeKey={rangeKey} />}
        {sub === 'flows' && <FlowsTab exporter={exporter} />}
      </div>
    </article>
  )
}

function FeatureHeader({
  exporter,
  range,
  onRangeChange,
  rangeKey,
}: {
  exporter: string
  range: TimeRange
  onRangeChange: (r: TimeRange) => void
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
        <span className="ml-auto normal-case">
          <TimeRangeSelector range={range} onChange={onRangeChange} />
        </span>
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
    { k: 'model', v: i ? vendorOID(i.sys_object_id) : '—' },
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
          className="px-3 py-2.5 border-r border-b border-line"
        >
          <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-0.5">
            {c.k}
          </div>
          <div className={`text-[14px] tabular text-text ${c.mono ? 'font-mono' : ''}`}>
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
      <Section title="Recent activity" sub="last 60s of flows">
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
        <div key={idx} className="px-3 py-2.5 border-r border-b border-line">
          <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-0.5">
            {c.k}
          </div>
          <div className={`text-[13px] text-text leading-[1.3] ${c.mono ? 'font-mono' : ''}`}>
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
        {ifaces.slice(0, 10).map((i: InterfaceRow) => {
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
  const [activeIfindex, setActiveIfindex] = useState<number | null>(null)
  const apiRange = toApi(range)
  const winLabel = rangeLabel(range)
  const q = useQuery({
    queryKey: ['device-ifaces-full', exporter, rangeKey],
    queryFn: () => api.interfaces(apiRange, exporter),
    refetchInterval: range.kind === 'preset' ? 5000 : false,
  })
  const ifaces = q.data?.interfaces ?? []
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
              <th>interface</th>
              <th className="r">in (latest)</th>
              <th className="r">out (latest)</th>
              <th className="r">in peak</th>
              <th className="r">out peak</th>
              <th className="r">last seen</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {ifaces.map((i: InterfaceRow) => {
              const lbl = labelInterface(i)
              const isActive = activeIfindex === i.ifindex
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
                        onClick={() => setActiveIfindex(isActive ? null : i.ifindex)}
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

function FlowsTab({ exporter }: { exporter: string }) {
  const q = useQuery({
    queryKey: ['device-flows', exporter],
    queryFn: () => api.recentFlows(50, exporter),
    refetchInterval: 2000,
  })
  const flows = q.data?.flows ?? []
  return (
    <div>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          Recent flows
        </span>
        <span className="font-mono text-[11px] text-faint">
          {q.isLoading ? 'loading…' : `${flows.length} most recent`}
        </span>
        <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-faint">
          REFRESH · 2s
        </span>
      </div>
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
              <th>time</th>
              <th>source</th>
              <th>src → dst</th>
              <th>proto</th>
              <th className="r">packets</th>
              <th className="r">bytes</th>
            </tr>
          </thead>
          <tbody>
            {flows.map((f: RecentFlow, i: number) => (
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
    </div>
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

// vendorOID maps the vendor-prefix portion of sysObjectID to a human
// label. Falls back to the raw OID. The full IETF / IANA enterprise
// registry is huge; a tiny lookup covers the common cases.
function vendorOID(oid: string): string {
  if (!oid) return '—'
  if (oid.startsWith('1.3.6.1.4.1.9.')) return 'Cisco · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.2636.')) return 'Juniper · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.30065.')) return 'Arista · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.4526.')) return 'Netgear · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.890.')) return 'Zyxel · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.6027.')) return 'Force10/Dell · ' + oid
  if (oid.startsWith('1.3.6.1.4.1.674.')) return 'Dell · ' + oid
  return oid
}
