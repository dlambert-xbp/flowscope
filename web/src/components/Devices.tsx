import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Fragment, useCallback, useEffect, useState, type ReactNode } from 'react'
import { api, fmt, isEpoch, labelExporter, labelInterface } from '../api'
import type {
  Device,
  DeviceInventory,
  DeviceResource,
  DeviceResourceKind,
  ExporterHealthRow,
  InterfaceRow,
} from '../api'
import { InterfaceChart } from './InterfaceChart'
import { TimeseriesChart, resolveColor } from './TimeseriesChart'
import { FlowsTopN, FlowsInvestigate } from './Flows'
import { NeighborsTable, TopologyGraph } from './TopologyGraph'
import type { Filter } from '../filters'
import {
  rangeLabel,
  rangeSeconds,
  toApi,
  useLiveInterval,
  useTimeRange,
  type TimeRange,
} from '../timeRange'
import { Th, useTableSort, type SortColumns } from './sortable'
import { formatModel } from '../lib/oidModels'
import {
  UNCATEGORIZED_LABEL,
  groupDevices,
  loadCollapsedGroups,
  normalizeLocation,
  saveCollapsedGroups,
  type DeviceGroup,
} from '../lib/deviceGroups'
const DEVICE_IFACE_COLS: SortColumns<InterfaceRow> = {
  interface: (r) => labelInterface(r).primary,
  in_latest: (r) => r.in_bps_latest,
  out_latest: (r) => r.out_bps_latest,
  in_peak: (r) => r.in_bps_peak,
  out_peak: (r) => r.out_bps_peak,
  last_seen: (r) => r.last_seen,
}

const RAIL_WIDTH_KEY = 'flowscope.devices.railWidth'
const RAIL_WIDTH_DEFAULT = 280
const RAIL_WIDTH_MIN = 220
const RAIL_WIDTH_MAX = 480

// URL param holding the currently selected exporter on the Devices
// tab. Named "device" rather than "exporter" so it doesn't collide
// with the Flows-tab filter chip (which is also "exporter") — they
// can both live in the URL without trampling each other when the
// operator deep-links across tabs.
const DEVICE_PARAM = 'device'

function readDeviceFromURL(): string | null {
  if (typeof window === 'undefined') return null
  return new URLSearchParams(window.location.search).get(DEVICE_PARAM)
}

function writeDeviceToURL(exporter: string | null) {
  if (typeof window === 'undefined') return
  const sp = new URLSearchParams(window.location.search)
  if (exporter) {
    sp.set(DEVICE_PARAM, exporter)
  } else {
    sp.delete(DEVICE_PARAM)
  }
  const qs = sp.toString()
  const next = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  if (window.location.pathname + window.location.search !== next) {
    window.history.replaceState({}, '', next)
  }
}

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
  onInvestigate,
}: {
  range: TimeRange
  rangeKey: unknown
  // onInvestigate hands chips off to the host shell which navigates
  // to Flows → Investigate. Threaded through Feature so the per-device
  // surfaces (Interfaces tab, etc.) can launch an investigation
  // without owning tab state.
  onInvestigate?: (chips: Filter[]) => void
}) {
  const apiRange = toApi(range)
  const list = useQuery({
    queryKey: ['devices', rangeKey],
    queryFn: () => api.devices(apiRange),
    refetchInterval: useLiveInterval(5000),
  })
  // Per-(exporter, source) freshness powers the source badges in the
  // left rail. We only need the rows themselves — last_seen drives the
  // active/stale decision, which is independent of the window length.
  const health = useQuery({
    queryKey: ['health-exporters', rangeKey],
    queryFn: () => api.healthExporters(apiRange),
    refetchInterval: useLiveInterval(10_000),
  })
  const devices = list.data?.devices ?? []
  const healthRows = health.data?.rows ?? []
  // Selected exporter lives in the URL (?device=<ip>) so a refresh or
  // a shared link restores the same detail view. The local mirror is
  // kept in sync via popstate (back/forward) and via the click handler
  // below, which writes the URL synchronously before any fetch fires
  // — render-on-state-change rule.
  const [selected, setSelectedState] = useState<string | null>(() => readDeviceFromURL())
  useEffect(() => {
    const onPop = () => setSelectedState(readDeviceFromURL())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  const setSelected = useCallback((exporter: string) => {
    writeDeviceToURL(exporter)
    setSelectedState(exporter)
  }, [])

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
        health={healthRows}
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
        onSelectExporter={setSelected}
        onInvestigate={onInvestigate}
      />
    </div>
  )
}

/* --------------------------- Source badges --------------------------- */

// SourceKind enumerates the data-collection sources we badge in the
// directory. Multiple flow protocols collapse into NETFLOW because the
// operator cares about the family, not the wire version.
type SourceKind = 'snmp' | 'sflow' | 'gnmi' | 'netflow'

// freshSeconds is the window in which a source is considered "active"
// for badge + green-dot purposes. Flow sources stream every few
// seconds when traffic is present; SNMP polls every poll-interval
// (default 60s, but configurable longer). Keeping 5 minutes covers
// both without flicker — slow walks still light up the SNMP badge.
const FRESH_SECONDS = 300

function sourceKindFor(s: string): SourceKind | null {
  switch (s) {
    case 'sflow':
      return 'sflow'
    case 'gnmi':
      return 'gnmi'
    case 'netflow_v5':
    case 'netflow_v9':
    case 'ipfix':
      return 'netflow'
    default:
      return null
  }
}

// activeSourcesFor returns the set of active sources for one exporter.
// SNMP is active when last_walked is non-epoch AND inside the
// freshness window. Each flow source is active when at least one
// ExporterHealthRow for that exporter+source has last_seen inside the
// freshness window.
function activeSourcesFor(d: Device, health: ExporterHealthRow[]): Set<SourceKind> {
  const out = new Set<SourceKind>()
  if (!isEpoch(d.last_walked) && secondsSince(d.last_walked) < FRESH_SECONDS) {
    out.add('snmp')
  }
  for (const r of health) {
    if (r.exporter !== d.exporter) continue
    if (secondsSince(r.last_seen) >= FRESH_SECONDS) continue
    const k = sourceKindFor(r.source)
    if (k) out.add(k)
  }
  return out
}

const BADGE_ORDER: SourceKind[] = ['snmp', 'sflow', 'gnmi', 'netflow']

const BADGE_TONE: Record<SourceKind, string> = {
  // SNMP is enrichment, not data — accent tone to differentiate it from
  // the flow-source pills.
  snmp: 'bg-accent-wash text-accent border-accent/40',
  // Counter-truth sources (sflow, gnmi) share the ok tone — both yield
  // authoritative bytes/sec rather than flow-derived approximations.
  sflow: 'bg-ok-wash text-ok border-ok/40',
  gnmi: 'bg-ok-wash text-ok border-ok/40',
  // NetFlow is flow-derived — dim tone so it sits behind counter sources.
  netflow: 'bg-raise text-dim border-line',
}

const BADGE_LABEL: Record<SourceKind, string> = {
  snmp: 'SNMP',
  sflow: 'SFLOW',
  gnmi: 'GNMI',
  netflow: 'NETFLOW',
}

function SourceBadges({ sources }: { sources: Set<SourceKind> }) {
  const visible = BADGE_ORDER.filter((k) => sources.has(k))
  if (visible.length === 0) return null
  return (
    <div className="flex gap-1 mt-0.5">
      {visible.map((k) => (
        <span
          key={k}
          className={`font-mono text-[9.5px] leading-none uppercase tracking-[0.06em] px-1 py-px border ${BADGE_TONE[k]}`}
        >
          {BADGE_LABEL[k]}
        </span>
      ))}
    </div>
  )
}

/* ----------------------------- Directory ----------------------------- */

function Directory({
  devices,
  health,
  selected,
  onSelect,
  loading,
  range,
  onResizeStart,
}: {
  devices: Device[]
  health: ExporterHealthRow[]
  selected: string | null
  onSelect: (e: string) => void
  loading: boolean
  range: TimeRange
  onResizeStart: (e: React.MouseEvent) => void
}) {
  const [filter, setFilter] = useState('')
  // Free-text filter matches the exporter IP, the SNMP sys_name, and
  // the sys_location (case-insensitive) so an operator can find a
  // device whether they remember the IP, the hostname, or the site
  // they put it at. Empty filter passes everything through.
  const needle = filter.trim().toLowerCase()
  const filtered = devices.filter((d) => {
    if (needle === '') return true
    if (d.exporter.toLowerCase().includes(needle)) return true
    if (d.sys_name && d.sys_name.toLowerCase().includes(needle)) return true
    if (d.sys_location && d.sys_location.toLowerCase().includes(needle)) return true
    return false
  })
  const groups = groupDevices(filtered)

  // Collapsed-group state is persisted in localStorage so the
  // operator's "I always close the lab folder" preference survives
  // refreshes. Default is "everything expanded" — we only ever
  // persist the collapsed set, so a brand-new folder for a brand-new
  // site shows up open.
  //
  // On first mount, if the selected device's group is collapsed in
  // persistence we drop it from the working set so the selection is
  // visible without the operator having to click. After that initial
  // hydration we leave the set alone — the operator can still
  // collapse the selected device's folder explicitly if they want.
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    const persisted = loadCollapsedGroups()
    if (!selected) return persisted
    // Walk the device list once to find the selected device and the
    // name of the group it belongs to, then unset that name in the
    // persisted set if present. We don't depend on the `groups` const
    // because lazy initializers can't see render-scope locals.
    for (const d of devices) {
      if (d.exporter !== selected) continue
      const groupName = normalizeLocation(d.sys_location) ?? UNCATEGORIZED_LABEL
      if (persisted.has(groupName)) {
        const next = new Set(persisted)
        next.delete(groupName)
        return next
      }
      break
    }
    return persisted
  })
  useEffect(() => {
    saveCollapsedGroups(collapsed)
  }, [collapsed])

  // Cross-tab navigation (Neighbors → click a neighbor) can change
  // the selection to a device in a collapsed group after mount; the
  // lazy initializer above only handles first-paint. Watch for the
  // selected device's group name to change and pull it out of the
  // collapsed set when it does. One frame of flicker is acceptable —
  // common-path selection clicks already happen on visible rows.
  const selectedGroupName = selected
    ? groups.find((g) => g.devices.some((d) => d.exporter === selected))?.name
    : undefined
  useEffect(() => {
    if (!selectedGroupName) return
    setCollapsed((prev) => {
      if (!prev.has(selectedGroupName)) return prev
      const next = new Set(prev)
      next.delete(selectedGroupName)
      return next
    })
  }, [selectedGroupName])

  // When a filter is active, any group with matches force-expands so
  // the operator can see what they searched for without one extra
  // click per folder.
  const filterActive = needle !== ''

  const toggleGroup = useCallback((name: string) => {
    // Render-on-state-change rule: state flips immediately on click,
    // before any side effects (the localStorage write below is in an
    // effect, not in the handler).
    setCollapsed((prev) => {
      const next = new Set(prev)
      if (next.has(name)) {
        next.delete(name)
      } else {
        next.add(name)
      }
      return next
    })
  }, [])

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
          <div data-testid="devices-empty" className="px-3 py-6 text-[12px] font-mono text-dim">
            {devices.length === 0
              ? 'no exporters seen in window'
              : 'no matches'}
          </div>
        )}
        {groups.map((g) => {
          // Filter forces a group open when it has matches so the
          // operator never has to expand a folder to see what they
          // typed. Otherwise respect the persisted collapsed set —
          // the initial state of which already excludes the selected
          // device's group, so the highlight is visible on mount.
          const isCollapsed = !filterActive && collapsed.has(g.name)
          return (
            <DirectoryGroup
              key={g.slug}
              group={g}
              collapsed={isCollapsed}
              onToggle={() => toggleGroup(g.name)}
              selected={selected}
              onSelect={onSelect}
              seconds={rangeSeconds(range)}
              health={health}
            />
          )
        })}
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

// DirectoryGroup is one collapsible folder in the left-rail directory.
// Header shows caret + location name + count badge; click anywhere on
// the header to toggle. Selection / hover styling on rows is
// unchanged from the flat-list era so existing Playwright selectors
// (data-testid="device-row") and operator muscle memory keep working.
function DirectoryGroup({
  group,
  collapsed,
  onToggle,
  selected,
  onSelect,
  seconds,
  health,
}: {
  group: DeviceGroup
  collapsed: boolean
  onToggle: () => void
  selected: string | null
  onSelect: (exporter: string) => void
  seconds: number
  health: ExporterHealthRow[]
}) {
  return (
    <div data-testid={`devices-group-${group.slug}`}>
      <button
        type="button"
        onClick={onToggle}
        aria-expanded={!collapsed}
        title={group.name}
        data-testid={`devices-group-header-${group.slug}`}
        className="w-full text-left px-3 py-1.5 border-b border-line-soft bg-ink hover:bg-hover flex items-center gap-2"
      >
        <span
          aria-hidden
          className="font-mono text-[10px] text-faint w-3 shrink-0 text-center"
        >
          {collapsed ? '▸' : '▾'}
        </span>
        <span
          className={`font-mono text-[11px] uppercase tracking-[0.08em] truncate min-w-0 flex-1 ${
            group.isUncategorized ? 'text-faint' : 'text-dim'
          }`}
        >
          {group.name}
        </span>
        <span className="ml-auto font-mono text-[10.5px] text-faint tabular shrink-0">
          {group.devices.length}
        </span>
      </button>
      {!collapsed &&
        group.devices.map((d) => (
          <DirectoryRow
            key={d.exporter}
            d={d}
            active={d.exporter === selected}
            onSelect={() => onSelect(d.exporter)}
            seconds={seconds}
            health={health}
          />
        ))}
    </div>
  )
}

function DirectoryRow({
  d,
  active,
  onSelect,
  seconds,
  health,
}: {
  d: Device
  active: boolean
  onSelect: () => void
  seconds: number
  health: ExporterHealthRow[]
}) {
  // sources is the set of fresh data-collection sources for this
  // device (SNMP and per-flow-source). The green dot lights up when
  // any source is fresh — flows OR SNMP, per operator request — so an
  // SNMP-only device that's actively polled looks just as "alive" as a
  // device pumping NetFlow. Badges below the hostname mirror the set
  // so the operator can tell at a glance what we're collecting.
  const sources = activeSourcesFor(d, health)
  const anyActive = sources.size > 0
  const discovered = isEpoch(d.last_seen) && !sources.has('snmp')
  const sinceFlow = isEpoch(d.last_seen) ? Infinity : secondsSince(d.last_seen)
  const dot = anyActive
    ? 'bg-ok'
    : discovered
      ? 'bg-faint'
      : sinceFlow < 300
        ? 'bg-warn'
        : 'bg-crit'
  const lbl = labelExporter(d)
  return (
    <button
      onClick={onSelect}
      data-testid="device-row"
      className={`w-full text-left px-3 py-2 border-b border-line-soft flex items-start gap-3 hover:bg-hover ${
        active ? 'bg-accent-wash' : ''
      }`}
    >
      <span
        className={`w-1.5 h-1.5 rounded-full ${dot} shrink-0 mt-1.5`}
        title={
          anyActive
            ? `active · ${[...sources].map((s) => s.toUpperCase()).join(' · ')}`
            : discovered
              ? 'SNMP walked · no flows in window'
              : undefined
        }
      />
      <div className="min-w-0 flex-1">
        <div className="font-mono text-[12.5px] truncate">{lbl.primary}</div>
        <SourceBadges sources={sources} />
        {lbl.secondary && (
          <div className="font-mono text-[10.5px] text-faint truncate mt-0.5">{lbl.secondary}</div>
        )}
      </div>
      <span className="ml-auto font-mono text-[10.5px] text-faint shrink-0 tabular mt-0.5">
        {isEpoch(d.last_seen) ? '—' : fmt.bps((d.bytes * 8) / Math.max(1, seconds))}
      </span>
    </button>
  )
}

/* ----------------------------- Feature view ----------------------------- */

type SubTab = 'summary' | 'interfaces' | 'flows' | 'neighbors'

function Feature({
  exporter,
  range,
  rangeKey,
  onSelectExporter,
  onInvestigate,
}: {
  exporter: string | null
  range: TimeRange
  rangeKey: unknown
  onSelectExporter: (exporter: string) => void
  onInvestigate?: (chips: Filter[]) => void
}) {
  const [sub, setSub] = useState<SubTab>('summary')
  // Reuse the same query key FeatureHeader uses so TanStack dedupes —
  // we just want the SNMP sys_name, if it's been walked, to label the
  // exporter chip when the operator deep-links into Flows.
  const inv = useQuery({
    queryKey: ['device-inventory', exporter ?? ''],
    queryFn: () =>
      api
        .deviceInventory(exporter as string)
        .catch(() => undefined as DeviceInventory | undefined),
    enabled: !!exporter,
    refetchInterval: useLiveInterval(30_000),
  })
  if (!exporter) {
    return (
      <div className="p-8 text-dim font-mono text-[13px]">
        Select an exporter from the directory.
      </div>
    )
  }
  // Always non-empty: falls back to the IP, so the chip never flashes
  // an empty label on click while the inventory query is in flight.
  const exporterLabel = inv.data?.sys_name || exporter
  return (
    <article className="overflow-auto">
      <FeatureHeader exporter={exporter} range={range} rangeKey={rangeKey} />
      <SubTabs active={sub} onChange={setSub} />
      <div>
        {sub === 'summary' && <SummaryTab exporter={exporter} range={range} rangeKey={rangeKey} />}
        {sub === 'interfaces' && (
          <InterfacesTab
            exporter={exporter}
            exporterLabel={exporterLabel}
            range={range}
            rangeKey={rangeKey}
            onInvestigate={onInvestigate}
          />
        )}
        {sub === 'flows' && (
          <FlowsTab
            exporter={exporter}
            exporterLabel={exporterLabel}
            range={range}
            rangeKey={rangeKey}
          />
        )}
        {sub === 'neighbors' && (
          <NeighborsTab
            exporter={exporter}
            onSelectExporter={(ex) => {
              // Render-on-state-change: switch the URL synchronously
              // (the host's setSelected already does that) BEFORE any
              // refetch. Also flip the sub-tab back to summary so the
              // new device starts at its overview.
              onSelectExporter(ex)
              setSub('summary')
            }}
          />
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
    refetchInterval: useLiveInterval(5000),
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
    refetchInterval: useLiveInterval(30_000),
  })
  const d = q.data
  const i = inv.data
  // discovered = SNMP knows the device but there are no flow records
  // inside the active time window. Show it as "discovered" (neutral)
  // rather than misleading "offline" — see [[snmp-only-devices]].
  const discovered = d ? isEpoch(d.last_seen) : false
  const since = d && !discovered ? secondsSince(d.last_seen) : Infinity
  const status: 'online' | 'silent' | 'offline' | 'discovered' = discovered
    ? 'discovered'
    : since < 60
      ? 'online'
      : since < 300
        ? 'silent'
        : 'offline'
  const tone =
    status === 'online'
      ? 'text-ok'
      : status === 'silent'
        ? 'text-warn'
        : status === 'discovered'
          ? 'text-faint'
          : 'text-crit'
  const headline = i?.sys_name || exporter
  return (
    <header className="px-6 pt-6 pb-4 border-b border-line bg-surface">
      <div className="flex items-center gap-3 text-[10.5px] uppercase tracking-[0.1em] font-semibold text-dim mb-1">
        <span className={tone}>● {status}</span>
        <span className="font-mono text-[10.5px] text-faint normal-case tracking-[0.02em]">
          last seen {d && !discovered ? fmt.time(d.last_seen).slice(11, 19) + 'Z' : '—'}
        </span>
        <WalkNowButton exporter={exporter} />
        <span className="font-mono text-[10.5px] text-faint normal-case tracking-[0.02em]">
          first seen {d && !discovered ? fmt.time(d.first_seen).slice(11, 19) + 'Z' : '—'}
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
            this device — the snmp service polls every 60s once an exporter
            shows up in flows.
          </>
        )}
      </p>
      <SpecRow d={d} i={i} range={range} />
    </header>
  )
}

// WalkNowButton enqueues an operator-triggered SNMP walk for this
// exporter and animates the queued → walking → fresh transition next
// to "last seen" in the FeatureHeader. The backend already supports
// POST /api/devices/{exporter}/snmp/walk; this just exposes it where
// an operator staring at a device would expect it (instead of buried
// in Settings → SNMP).
//
// State machine:
//   idle    → button reads "walk now"
//   queued  → button reads "queued · scanning"; ~30s window during
//             which we proactively re-fetch inventory + resources so
//             the new polled_at appears as soon as the walk lands.
//   error   → button reads "walk failed", auto-resets in 4s
function WalkNowButton({ exporter }: { exporter: string }) {
  const qc = useQueryClient()
  const [state, setState] = useState<'idle' | 'queued' | 'error'>('idle')
  const [errMsg, setErrMsg] = useState<string>('')
  const runWalk = async () => {
    setState('queued')
    setErrMsg('')
    try {
      await api.requestSnmpWalk(exporter)
    } catch (err) {
      setErrMsg(err instanceof Error ? err.message : String(err))
      setState('error')
      setTimeout(() => setState('idle'), 8000)
      return
    }
    // Trigger a few invalidations spaced over ~30s so the fresh
    // polled_at, inventory, and resource samples flow into the UI as
    // soon as the snmp service finishes the walk.
    const refresh = () => {
      qc.invalidateQueries({ queryKey: ['device-inventory', exporter] })
      qc.invalidateQueries({ queryKey: ['device-resources', exporter] })
    }
    const t1 = setTimeout(refresh, 5_000)
    const t2 = setTimeout(refresh, 15_000)
    const t3 = setTimeout(() => {
      refresh()
      setState('idle')
    }, 30_000)
    // Best-effort cleanup; if the user navigates away the stale
    // timers just no-op against an unmounted React tree.
    return () => {
      clearTimeout(t1)
      clearTimeout(t2)
      clearTimeout(t3)
    }
  }
  const label =
    state === 'queued'
      ? 'queued · scanning'
      : state === 'error'
        ? 'walk failed'
        : 'walk now'
  const tone =
    state === 'queued'
      ? 'border-accent text-accent'
      : state === 'error'
        ? 'border-crit text-crit'
        : 'border-line text-dim hover:border-accent hover:text-text'
  return (
    <button
      type="button"
      onClick={runWalk}
      disabled={state === 'queued'}
      aria-label="Trigger an SNMP walk on this exporter"
      title={state === 'error' && errMsg ? errMsg : undefined}
      data-testid="walk-now-button"
      className={`font-mono text-[10px] uppercase tracking-[0.1em] px-2 py-0.5 border ${tone} disabled:cursor-wait`}
    >
      {label}
    </button>
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
      <Tab id="neighbors" active={active} onChange={onChange}>Neighbors</Tab>
      <Tab id="flows" active={active} onChange={onChange}>Flows</Tab>
    </div>
  )
}

// NeighborsTab is the Devices → Neighbors sub-tab. Top half is the
// fleet-wide topology graph (so an operator can see how the selected
// device sits in the fabric); bottom half is the per-device
// adjacency list with one row per LLDP/CDP neighbor.
//
// Click a node on the graph → host shell navigates to that device's
// Summary tab. The local "Section" wrapper keeps the panel chrome
// consistent with the Summary tab's sections.
function NeighborsTab({
  exporter,
  onSelectExporter,
}: {
  exporter: string
  onSelectExporter: (exporter: string) => void
}) {
  return (
    <div className="px-6 py-5 space-y-5">
      <Section title="Topology" sub="lldp / cdp · 5-min walk cadence" right="SOURCE · SNMP">
        <div className="relative">
          <TopologyGraph
            selectedExporter={exporter}
            onSelectExporter={onSelectExporter}
          />
        </div>
      </Section>
      <Section title="Neighbors" sub="per-port adjacency" right="SOURCE · SNMP">
        <NeighborsTable exporter={exporter} />
      </Section>
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
      data-testid={`devices-subtab-${id}`}
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
}: {
  exporter: string
  range: TimeRange
  rangeKey: unknown
}) {
  // Subtitle reflects the actual version that walked this device on
  // the last poll. Falls back to a neutral 'snmp' when the row was
  // written before migration 000016 added the column.
  const inv = useQuery({
    queryKey: ['device-inventory', exporter],
    queryFn: () =>
      api
        .deviceInventory(exporter)
        .catch(() => undefined as DeviceInventory | undefined),
    refetchInterval: useLiveInterval(30_000),
  })
  const v = inv.data?.snmp_version
  const sub = v ? `snmp · ${v}` : 'snmp'
  return (
    <div className="px-6 py-5 space-y-5">
      <Section title="Inventory" sub={sub} right="SOURCE · SNMP">
        <InventoryPanel exporter={exporter} />
      </Section>
      <Section title="Health" sub="cpu · memory · storage · last 24h" right="SOURCE · SNMP">
        <ResourcesPanel exporter={exporter} />
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
    refetchInterval: useLiveInterval(30_000),
  })
  if (q.isLoading) return <p className="text-dim font-mono text-[12px]">loading…</p>
  const i = q.data
  if (!i) {
    return (
      <p className="text-dim font-mono text-[12px]">
        no SNMP data yet · the snmp service polls every 60s after the
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

// ResourcesPanel renders one tile per (kind, component) returned from
// /api/devices/{exporter}/resources, grouped by kind so CPU rows sit
// next to other CPU rows. Tile = headline number, secondary context
// (bytes for memory/storage), tiny sparkline. Range follows the
// global TimeRange so a brush on one chart re-narrows every panel
// (including this one); refetch cadence pauses entirely in fixed
// mode so the dashboard reads as a true snapshot until the operator
// switches preset or hits refresh.
function ResourcesPanel({ exporter }: { exporter: string }) {
  const { range, queryKey: rangeKey } = useTimeRange()
  const apiRange = toApi(range)
  const q = useQuery({
    queryKey: ['device-resources', exporter, rangeKey],
    queryFn: () => api.deviceResources(exporter, apiRange),
    refetchInterval: useLiveInterval(15_000),
  })
  // Selected kind is what the operator clicked to expand into a
  // multi-series chart. One rollup tile per kind; click to expand,
  // click again or × close to collapse. Stored as the kind enum
  // because each kind has at most one rollup tile in the grid.
  const [selected, setSelected] = useState<DeviceResourceKind | null>(null)
  if (q.isLoading) {
    return <p className="text-dim font-mono text-[12px]">loading…</p>
  }
  if (q.error) {
    return (
      <p className="text-crit font-mono text-[12px]">
        error · {(q.error as Error).message}
      </p>
    )
  }
  const rows = q.data?.rows ?? []
  if (rows.length === 0) {
    return (
      <p className="text-dim font-mono text-[12px]">
        no SNMP resource data yet · the snmp service walks HOST-RESOURCES-MIB
        and CISCO-PROCESS / MEMORY-POOL MIBs on the same cadence as inventory
      </p>
    )
  }
  // Group rows by kind so the rollup tile can aggregate across
  // components (CPU0 + CPU1 → "Overall CPU", Inlet + Hotspot →
  // "Overall Temperature", etc.).
  const groups: Partial<Record<DeviceResourceKind, DeviceResource[]>> = {}
  for (const r of rows) {
    const g = groups[r.kind] ?? []
    g.push(r)
    groups[r.kind] = g
  }
  const order: DeviceResourceKind[] = [
    'cpu',
    'memory',
    'storage',
    'temperature',
    'fan',
    'power',
    'voltage',
    'current',
  ]
  const presentKinds = order.filter((k) => groups[k] && groups[k]!.length > 0)
  const selectedComps = selected ? groups[selected] : undefined
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-2">
        {presentKinds.map((kind) => (
          <RollupTile
            key={kind}
            kind={kind}
            comps={groups[kind]!}
            active={selected === kind}
            onClick={() =>
              setSelected((s) => (s === kind ? null : kind))
            }
          />
        ))}
      </div>
      {selected && selectedComps && selectedComps.length > 0 && (
        <ExpandedKindChart
          kind={selected}
          comps={selectedComps}
          onClose={() => setSelected(null)}
        />
      )}
    </div>
  )
}

// RollupTile renders one tile per kind, summarising every component
// of that kind into a single headline number. Sparkline shows the
// aggregated metric over time (max temp, min fan rpm, etc.). Click
// to expand into a multi-series chart showing each component as a
// separate line.
function RollupTile({
  kind,
  comps,
  active,
  onClick,
}: {
  kind: DeviceResourceKind
  comps: DeviceResource[]
  active: boolean
  onClick: () => void
}) {
  const rollup = rollupOf(kind, comps)
  const sparkline = aggregateSparkline(kind, comps)
  const fixedRange = isUtilizationKind(kind)
  const ageS = comps.reduce(
    (lo, c) => Math.min(lo, secondsSince(c.latest_ts)),
    Infinity,
  )
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      title={active ? 'Collapse' : `Expand ${kind} chart`}
      className={`text-left border bg-ink px-3 py-2.5 min-w-0 hover:border-accent ${
        active ? 'border-accent' : 'border-line'
      }`}
    >
      <div className="font-mono text-[11px] text-dim flex items-center gap-1.5">
        <span className="uppercase tracking-[0.08em]">{kind}</span>
        <span className="text-faint">·</span>
        <span className="text-faint truncate">{rollup.context}</span>
        <span className="ml-auto text-faint" aria-hidden>
          {active ? '▾' : '▸'}
        </span>
      </div>
      <div className="flex items-baseline gap-2 mt-1">
        <span className={`font-mono text-[20px] tabular leading-none ${rollup.tone.text}`}>
          {rollup.headline}
          {rollup.unit && (
            <span className="text-[12px] text-faint">{rollup.unit}</span>
          )}
        </span>
      </div>
      <div className="mt-1.5">
        <Sparkline
          points={sparkline.values}
          stroke={rollup.tone.stroke}
          fixedRange={fixedRange}
        />
      </div>
      <div className="font-mono text-[10px] text-faint mt-1">
        {Number.isFinite(ageS)
          ? ageS < 60
            ? `polled ${ageS.toFixed(0)}s ago`
            : ageS < 3600
              ? `polled ${(ageS / 60).toFixed(0)}m ago`
              : `polled ${(ageS / 3600).toFixed(0)}h ago`
          : 'never polled'}
      </div>
    </button>
  )
}

// ExpandedKindChart renders every component of a kind as its own
// series on a single time-series chart. Operator picked option (B):
// one chart, legend lists each component (e.g. Inlet + Hotspot for
// temperature, PSU 1 + PSU 2 for power). Drag-to-zoom works the
// same as everywhere else.
function ExpandedKindChart({
  kind,
  comps,
  onClose,
}: {
  kind: DeviceResourceKind
  comps: DeviceResource[]
  onClose: () => void
}) {
  const usesNumeric = !isUtilizationKind(kind)
  // Pick the longest component as the timestamp source. Components
  // from the same walk should agree on timestamps, but a sensor that
  // came online mid-window will have fewer points; the chart aligns
  // by index so taking the longest gives the most informative axis.
  const xRef = comps.reduce(
    (acc, c) => (c.points.length > acc.points.length ? c : acc),
    comps[0],
  )
  const xs = xRef.points.map((p) =>
    Math.floor(new Date(p.ts).getTime() / 1000),
  )
  const decimals = kind === 'fan' ? 0 : 1
  const unitSuffix = comps[0]?.unit ? ` ${comps[0].unit}` : ''
  const yFormat = (v: number) => {
    if (!usesNumeric) return `${v.toFixed(0)}%`
    return `${v.toFixed(decimals)}${unitSuffix}`
  }
  const palette = MULTI_SERIES_PALETTE.map((name, i) =>
    resolveColor(name, MULTI_SERIES_FALLBACKS[i]),
  )
  const series = comps.map((c, i) => ({
    label: c.component,
    color: palette[i % palette.length],
    values: c.points.map((p) =>
      usesNumeric ? p.value_numeric : p.value_percent,
    ),
    format: yFormat,
  }))
  return (
    <div className="mt-1 border border-line bg-surface">
      <div className="flex items-baseline gap-3 px-4 py-2 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          {kind}
        </span>
        <span className="font-mono text-[10.5px] text-faint">
          {comps.length} {comps.length === 1 ? 'component' : 'components'}
          {' · '}
          via {Array.from(new Set(comps.map((c) => c.source))).join(', ')}
        </span>
        <button
          type="button"
          onClick={onClose}
          className="ml-auto font-mono text-[10.5px] text-dim hover:text-text"
          aria-label="Close expanded chart"
        >
          × close
        </button>
      </div>
      <div className="px-4 py-3">
        <TimeseriesChart
          xs={xs}
          series={series}
          height={220}
          yMin={usesNumeric ? undefined : 0}
          yMax={usesNumeric ? undefined : 100}
          yFormat={yFormat}
          emptyLabel="no readings in this window"
        />
        <MultiSeriesLegend
          series={series}
          comps={comps}
          unitSuffix={unitSuffix}
          isPercent={!usesNumeric ? false : true}
          decimals={decimals}
        />
      </div>
    </div>
  )
}

// MultiSeriesLegend lists each plotted component with its colour
// swatch + last reading. Keeps the chart legend honest when a device
// has 4–8 sensors of the same kind and the lines blur together.
function MultiSeriesLegend({
  series,
  comps,
  unitSuffix,
  isPercent,
  decimals,
}: {
  series: { label: string; color: string }[]
  comps: DeviceResource[]
  unitSuffix: string
  isPercent: boolean
  decimals: number
}) {
  return (
    <div className="mt-2 grid grid-cols-2 md:grid-cols-3 lg:grid-cols-4 gap-x-4 gap-y-1 font-mono text-[11px] text-dim">
      {series.map((s, i) => {
        const c = comps[i]
        const latest = isPercent
          ? `${c.latest_percent.toFixed(0)}%`
          : `${c.latest_numeric.toFixed(decimals)}${unitSuffix}`
        return (
          <span key={s.label} className="flex items-center gap-2 min-w-0">
            <span
              className="inline-block w-3 h-0.5 shrink-0"
              style={{ backgroundColor: s.color }}
            />
            <span className="truncate">{s.label}</span>
            <span className="ml-auto text-text tabular shrink-0">{latest}</span>
          </span>
        )
      })}
    </div>
  )
}

// Palette names (CSS vars) and their hard-coded fallbacks, resolved
// at call time so theme switches re-pick the right hex. The five
// entries cover most real devices (≤4–5 sensors per kind); a sixth+
// sensor of the same kind cycles back to accent.
const MULTI_SERIES_PALETTE = [
  '--color-accent',
  '--color-ok',
  '--color-warn',
  '--color-crit',
  '--color-faint',
] as const
const MULTI_SERIES_FALLBACKS = ['#8aa8c8', '#7fa67f', '#d4a72c', '#d97757', '#888'] as const

// rollupOf computes the per-kind headline number plus context label
// + tone. Each kind has its own "what does overall mean" rule:
//   cpu / memory / storage / temperature → max
//   fan                                  → min (slowest = alarm)
//   voltage / current                    → average (min–max in context)
//   power (state-overloaded)             → count of FAULT vs OK
function rollupOf(
  kind: DeviceResourceKind,
  comps: DeviceResource[],
): {
  headline: string
  unit: string
  context: string
  tone: { text: string; stroke: string }
} {
  // Power is a special case: value_percent is 0 (OK) or 100 (FAULT).
  if (kind === 'power') {
    const faults = comps.filter((c) => c.latest_percent >= 50).length
    const total = comps.length
    if (faults === 0) {
      return {
        headline: 'ALL OK',
        unit: '',
        context: `${total} ${total === 1 ? 'supply' : 'supplies'}`,
        tone: { text: 'text-ok', stroke: 'var(--color-ok, #7fa67f)' },
      }
    }
    return {
      headline: `${faults} FAULT`,
      unit: '',
      context: `${faults} of ${total} faulted`,
      tone: { text: 'text-crit', stroke: 'var(--color-crit, #d97757)' },
    }
  }

  if (isUtilizationKind(kind)) {
    const values = comps.map((c) => c.latest_percent)
    const agg = Math.max(...values)
    return {
      headline: agg.toFixed(0),
      unit: '%',
      context:
        comps.length === 1
          ? truncate(comps[0].component, 28)
          : `max of ${comps.length}`,
      tone: utilizationToneAt(agg),
    }
  }

  // Sensor kinds.
  const values = comps.map((c) => c.latest_numeric)
  const decimals = kind === 'fan' ? 0 : 1
  const unit = comps[0]?.unit || ''
  let agg: number
  let contextLabel: string
  switch (kind) {
    case 'temperature':
      agg = Math.max(...values)
      contextLabel =
        comps.length === 1
          ? truncate(comps[0].component, 28)
          : `max of ${comps.length}`
      break
    case 'fan':
      agg = Math.min(...values)
      contextLabel =
        comps.length === 1
          ? truncate(comps[0].component, 28)
          : `slowest of ${comps.length}`
      break
    case 'voltage':
    case 'current': {
      agg = values.reduce((a, b) => a + b, 0) / values.length
      if (comps.length === 1) {
        contextLabel = truncate(comps[0].component, 28)
      } else {
        const lo = Math.min(...values)
        const hi = Math.max(...values)
        contextLabel = `${lo.toFixed(decimals)}–${hi.toFixed(decimals)}${unit ? ` ${unit}` : ''}`
      }
      break
    }
    default:
      agg = values[0] ?? 0
      contextLabel = truncate(comps[0]?.component ?? '', 28)
  }
  return {
    headline: agg.toFixed(decimals),
    unit: unit ? ` ${unit}` : '',
    context: contextLabel,
    tone: sensorToneFor(kind, agg),
  }
}

// aggregateSparkline produces the tile sparkline series: one
// number per timestamp, aggregated across components with the same
// reducer as the headline. Aligned by point index (all components of
// a kind come from the same walk, so polled_at agrees by index).
function aggregateSparkline(
  kind: DeviceResourceKind,
  comps: DeviceResource[],
): { xs: number[]; values: number[] } {
  const minLen = comps.reduce(
    (n, c) => Math.min(n, c.points.length),
    Number.POSITIVE_INFINITY,
  )
  if (!Number.isFinite(minLen) || minLen === 0) {
    return { xs: [], values: [] }
  }
  const reducer = reducerFor(kind)
  const usesNumeric = !isUtilizationKind(kind) && kind !== 'power'
  const xs: number[] = []
  const values: number[] = []
  for (let i = 0; i < minLen; i++) {
    const valsAtI = comps.map((c) =>
      usesNumeric ? c.points[i].value_numeric : c.points[i].value_percent,
    )
    values.push(reducer(valsAtI))
    xs.push(Math.floor(new Date(comps[0].points[i].ts).getTime() / 1000))
  }
  return { xs, values }
}

function reducerFor(kind: DeviceResourceKind): (xs: number[]) => number {
  switch (kind) {
    case 'fan':
      return (xs) => (xs.length === 0 ? 0 : Math.min(...xs))
    case 'voltage':
    case 'current':
      return (xs) => (xs.length === 0 ? 0 : xs.reduce((a, b) => a + b, 0) / xs.length)
    case 'cpu':
    case 'memory':
    case 'storage':
    case 'temperature':
    case 'power':
    default:
      return (xs) => (xs.length === 0 ? 0 : Math.max(...xs))
  }
}

function utilizationToneAt(pct: number): { text: string; stroke: string } {
  if (pct >= 80) return { text: 'text-crit', stroke: 'var(--color-crit, #d97757)' }
  if (pct >= 60) return { text: 'text-warn', stroke: 'var(--color-warn, #d4a72c)' }
  return { text: 'text-text', stroke: 'var(--color-accent, #8aa8c8)' }
}

function sensorToneFor(
  kind: DeviceResourceKind,
  value: number,
): { text: string; stroke: string } {
  if (kind === 'temperature') {
    if (value >= 75) return { text: 'text-crit', stroke: 'var(--color-crit, #d97757)' }
    if (value >= 60) return { text: 'text-warn', stroke: 'var(--color-warn, #d4a72c)' }
  }
  return { text: 'text-text', stroke: 'var(--color-accent, #8aa8c8)' }
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s
  return s.slice(0, n - 1) + '…'
}

function isUtilizationKind(k: DeviceResourceKind): boolean {
  return k === 'cpu' || k === 'memory' || k === 'storage'
}

// Sparkline draws a polyline of values in inline SVG. When
// `fixedRange` is true the Y axis is clamped to [0, 100] (utilization
// metrics) so tiles read on a common scale; when false the axis
// auto-ranges with a small headroom (sensor metrics like 28–47 °C)
// so subtle variation is still visible. Last point gets a small dot
// so the eye lands on "now".
function Sparkline({
  points,
  stroke,
  fixedRange,
}: {
  points: number[]
  stroke: string
  fixedRange?: boolean
}) {
  const w = 100
  const h = 22
  if (points.length === 0) {
    return (
      <svg viewBox={`0 0 ${w} ${h}`} className="w-full h-[22px]" preserveAspectRatio="none">
        <line x1={0} y1={h - 1} x2={w} y2={h - 1} stroke="var(--color-line, #2a2a2a)" />
      </svg>
    )
  }
  let lo = 0
  let hi = 100
  if (!fixedRange) {
    lo = Math.min(...points)
    hi = Math.max(...points)
    if (hi === lo) {
      // Constant series — synthesize a band so the line doesn't sit
      // exactly on the bottom edge and disappear.
      hi = lo + 1
    } else {
      const pad = (hi - lo) * 0.1
      lo -= pad
      hi += pad
    }
  }
  const project = (v: number) => {
    const t = (v - lo) / (hi - lo)
    return h - Math.max(0, Math.min(1, t)) * (h - 2) - 1
  }
  if (points.length === 1) {
    const y = project(points[0])
    return (
      <svg viewBox={`0 0 ${w} ${h}`} className="w-full h-[22px]" preserveAspectRatio="none">
        <line x1={0} y1={y} x2={w} y2={y} stroke={stroke} strokeWidth={1} />
        <circle cx={w - 1} cy={y} r={1.5} fill={stroke} />
      </svg>
    )
  }
  const stepX = w / (points.length - 1)
  const path = points
    .map((p, i) => {
      const x = i * stepX
      const y = project(p)
      return `${i === 0 ? 'M' : 'L'} ${x.toFixed(2)} ${y.toFixed(2)}`
    })
    .join(' ')
  const lastY = project(points[points.length - 1])
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="w-full h-[22px]" preserveAspectRatio="none">
      <path d={path} fill="none" stroke={stroke} strokeWidth={1} />
      <circle cx={w - 0.5} cy={lastY} r={1.5} fill={stroke} />
    </svg>
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

/* ----------------------------- Interfaces tab ----------------------------- */

function InterfacesTab({
  exporter,
  exporterLabel,
  range,
  rangeKey,
  onInvestigate,
}: {
  exporter: string
  exporterLabel: string
  range: TimeRange
  rangeKey: unknown
  onInvestigate?: (chips: Filter[]) => void
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
    refetchInterval: useLiveInterval(5000),
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
              const ifaceChipLabel = i.if_descr || i.if_alias || `ifindex ${i.ifindex}`
              const investigate = onInvestigate
                ? () =>
                    onInvestigate([
                      {
                        key: 'exporter',
                        value: exporter,
                        label: exporterLabel || undefined,
                      },
                      {
                        key: 'output_ifindex',
                        value: String(i.ifindex),
                        label: ifaceChipLabel,
                        keyLabel: 'out iface',
                      },
                    ])
                : undefined
              return (
                <Fragment key={i.ifindex}>
                  <tr className="hover:bg-surface group">
                    <td>
                      {investigate ? (
                        <button
                          type="button"
                          onClick={investigate}
                          title="filter & investigate this interface"
                          className="block w-full text-left hover:text-accent hover:underline decoration-dotted underline-offset-2"
                        >
                          <TwoLine primary={lbl.primary} secondary={lbl.secondary || undefined} />
                        </button>
                      ) : (
                        <TwoLine primary={lbl.primary} secondary={lbl.secondary || undefined} />
                      )}
                    </td>
                    <td className="r n">{fmt.bps(i.in_bps_latest)}</td>
                    <td className="r n">{fmt.bps(i.out_bps_latest)}</td>
                    <td className="r n text-accent">{fmt.bps(i.in_bps_peak)}</td>
                    <td className="r n text-ok">{fmt.bps(i.out_bps_peak)}</td>
                    <td className="r n text-faint">{fmt.time(i.last_seen).slice(11, 19)}</td>
                    <td className="r">
                      <div className="inline-flex items-center gap-3">
                        {investigate && (
                          <button
                            type="button"
                            onClick={investigate}
                            className="text-[10.5px] font-mono tracking-[0.06em] text-accent opacity-0 group-hover:opacity-100 hover:underline"
                          >
                            investigate →
                          </button>
                        )}
                        <button
                          className={`text-[11px] font-mono ${isActive ? 'text-text' : 'text-accent hover:underline'}`}
                          onClick={() => toggleChart(i.ifindex)}
                        >
                          {isActive ? '× close' : 'chart →'}
                        </button>
                      </div>
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

// FlowsTab on the Devices page reuses the Flows-page surfaces (Top-N
// + Investigate) with the exporter locked to the selected device.
// The "Live tail" sub-tab from the global Flows page is intentionally
// omitted here — the operator is in a device context, not a network-
// wide one, and live tail is filter-agnostic by design.
type DeviceFlowsSubTab = 'top' | 'investigate'

function FlowsTab({
  exporter,
  exporterLabel,
  range,
  rangeKey,
}: {
  exporter: string
  exporterLabel: string
  range: TimeRange
  rangeKey: unknown
}) {
  const [sub, setSub] = useState<DeviceFlowsSubTab>('top')
  return (
    <div>
      <p className="px-4 py-2 text-[11.5px] text-dim border-b border-line bg-surface leading-[1.5]">
        Switches export flows for traffic they <span className="text-text">forward</span>, not just
        traffic addressed to themselves. Source and destination IPs here are endpoints elsewhere on
        the network — this device just observed the conversation.
      </p>
      <FlowsSubTabBar active={sub} onChange={setSub} />
      {sub === 'top' && (
        <FlowsTopN
          range={range}
          rangeKey={rangeKey}
          lockedExporter={exporter}
          lockedExporterLabel={exporterLabel}
        />
      )}
      {sub === 'investigate' && (
        <FlowsInvestigate
          range={range}
          rangeKey={rangeKey}
          lockedExporter={exporter}
          lockedExporterLabel={exporterLabel}
        />
      )}
    </div>
  )
}

function FlowsSubTabBar({
  active,
  onChange,
}: {
  active: DeviceFlowsSubTab
  onChange: (s: DeviceFlowsSubTab) => void
}) {
  return (
    <div className="flex border-b border-line bg-ink">
      <FlowsSubTabBtn id="top" active={active} onChange={onChange}>Top-N</FlowsSubTabBtn>
      <FlowsSubTabBtn id="investigate" active={active} onChange={onChange}>Investigate</FlowsSubTabBtn>
    </div>
  )
}

function FlowsSubTabBtn({
  id,
  active,
  onChange,
  children,
}: {
  id: DeviceFlowsSubTab
  active: DeviceFlowsSubTab
  onChange: (s: DeviceFlowsSubTab) => void
  children: ReactNode
}) {
  const selected = id === active
  return (
    <button
      type="button"
      onClick={() => onChange(id)}
      data-testid={`device-flows-subtab-${id}`}
      className={`relative px-4 py-2.5 text-[13px] border-r border-line ${
        selected ? 'text-text' : 'text-dim hover:text-text hover:bg-surface'
      }`}
    >
      {children}
      {selected && <span className="absolute left-0 right-0 -bottom-px h-0.5 bg-accent" />}
    </button>
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

