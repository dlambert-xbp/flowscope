import { useQuery } from '@tanstack/react-query'
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { api, fmt } from './api'
import { Overview } from './components/Overview'
import { Flows } from './components/Flows'
import { Devices } from './components/Devices'
import { Alerts } from './components/Alerts'
import { Settings } from './components/Settings'
import { ThemeToggle } from './theme'
import { useTimeRange, type TimeRange } from './timeRange'
import { TimeRangeSelector } from './components/TimeRangeSelector'
import { FILTER_KEYS, type Filter } from './filters'

type Tab = 'overview' | 'flows' | 'devices' | 'alerts' | 'settings'

const LABEL_PREFIX = '_l_'

const TAB_LABELS: Record<Tab, string> = {
  overview: 'Overview',
  flows: 'Flows',
  devices: 'Devices',
  alerts: 'Alerts',
  settings: 'Settings',
}

const TIME_TABS: ReadonlySet<Tab> = new Set<Tab>(['overview', 'flows', 'devices'])

export function App() {
  const [tab, setTab] = useState<Tab>('overview')
  const tr = useTimeRange()
  const showRange = TIME_TABS.has(tab)
  // Cross-tab navigation primitive: writes filter chips into the URL,
  // then switches tabs. Devices "Investigate →" links use this to
  // deep-link into Flows pre-filtered to the current exporter.
  const navigateToFlows = useCallback((filters: Filter[]) => {
    const sp = new URLSearchParams(window.location.search)
    for (const k of FILTER_KEYS) {
      sp.delete(k)
      sp.delete(LABEL_PREFIX + k)
    }
    for (const f of filters) {
      sp.set(f.key, f.value)
      if (f.label && f.label !== f.value) {
        sp.set(LABEL_PREFIX + f.key, f.label)
      }
    }
    const qs = sp.toString()
    const next = qs
      ? `${window.location.pathname}?${qs}`
      : window.location.pathname
    window.history.replaceState({}, '', next)
    setTab('flows')
  }, [])
  return (
    <div
      className="grid bg-ink text-text overflow-hidden h-screen"
      style={{ gridTemplateRows: '32px 44px 32px 1fr 24px' }}
    >
      <Strip />
      <Bar tab={tab} onTab={setTab} />
      <PageContext tab={tab} showRange={showRange} range={tr.range} onRangeChange={tr.set} />
      <main className="overflow-auto">
        {tab === 'overview' && (
          <Overview range={tr.range} rangeKey={tr.queryKey} />
        )}
        {tab === 'flows' && (
          <Flows range={tr.range} rangeKey={tr.queryKey} />
        )}
        {tab === 'devices' && (
          <Devices
            range={tr.range}
            rangeKey={tr.queryKey}
            onNavigateToFlows={navigateToFlows}
          />
        )}
        {tab === 'alerts' && <Alerts />}
        {tab === 'settings' && <Settings />}
      </main>
      <Cmd />
    </div>
  )
}

/* ----------------------------- Page context row ----------------------------- */

function PageContext({
  tab,
  showRange,
  range,
  onRangeChange,
}: {
  tab: Tab
  showRange: boolean
  range: TimeRange
  onRangeChange: (r: TimeRange) => void
}) {
  return (
    <div className="flex items-center gap-3 px-4 bg-surface border-b border-line">
      <span className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold">
        {TAB_LABELS[tab]}
      </span>
      {showRange && (
        <span className="ml-auto">
          <TimeRangeSelector range={range} onChange={onRangeChange} />
        </span>
      )}
    </div>
  )
}

/* ----------------------------- Top strip ----------------------------- */

function Strip() {
  const summary = useQuery({
    queryKey: ['summary', 'strip'],
    queryFn: () => api.summary(60),
    refetchInterval: 2000,
  })
  const ifaces = useQuery({
    queryKey: ['interfaces', 'strip'],
    queryFn: () => api.interfaces(60),
    refetchInterval: 5000,
  })
  const [now, setNow] = useState<string>(() => new Date().toUTCString())
  useEffect(() => {
    const id = setInterval(() => setNow(new Date().toUTCString()), 1000)
    return () => clearInterval(id)
  }, [])

  const flows = summary.data?.flows ?? 0
  const exporters = summary.data?.exporters ?? 0
  const ifaceCount = ifaces.data?.count ?? 0
  const status = summary.error ? 'error' : summary.isLoading ? 'connecting' : 'live'
  // Hardcode class pairs so Tailwind sees them in the source.
  const statusDot = status === 'error' ? 'bg-crit' : status === 'live' ? 'bg-ok' : 'bg-warn'
  const statusText = status === 'error' ? 'text-crit' : status === 'live' ? 'text-ok' : 'text-warn'

  return (
    <div className="flex items-center gap-6 px-4 bg-surface border-b border-line font-mono text-[11px] text-dim tabular whitespace-nowrap overflow-hidden">
      <span className="flex items-center gap-2">
        <span className={`w-1.5 h-1.5 rounded-full ${statusDot} ${status === 'live' ? 'animate-pulse' : ''}`} />
        <span className={`${statusText} text-[10.5px] uppercase tracking-[0.08em]`}>{status}</span>
      </span>
      <Pair label="flows · 60s" value={fmt.num(flows)} />
      <Pair label="exporters" value={fmt.num(exporters)} />
      <Pair label="interfaces" value={fmt.num(ifaceCount)} />
      <Pair label="ch lag" value={summary.data?.newest ? '<1s' : '—'} />
      <div className="ml-auto flex items-center gap-6">
        <span className="text-faint text-[10px] uppercase tracking-[0.08em]">env</span>
        <span className="text-text">prod-east-2</span>
        <span className="text-faint">·</span>
        <span className="text-text">{now.replace('GMT', 'UTC').slice(5, 25)}</span>
      </div>
    </div>
  )
}

function Pair({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex items-baseline gap-2">
      <span className="text-faint text-[10px] uppercase tracking-[0.08em]">{label}</span>
      <span className="text-text">{value}</span>
    </span>
  )
}

/* ----------------------------- Brand + tabs ----------------------------- */

function Bar({ tab, onTab }: { tab: Tab; onTab: (t: Tab) => void }) {
  const summary = useQuery({
    queryKey: ['summary', 'bar'],
    queryFn: () => api.summary(60),
    refetchInterval: 2000,
  })
  const devices = useQuery({
    queryKey: ['devices', 'bar'],
    queryFn: () => api.devices(300),
    refetchInterval: 10_000,
  })
  const alertSummary = useQuery({
    queryKey: ['alertSummary', 'bar'],
    queryFn: () => api.alertSummary(),
    refetchInterval: 5_000,
  })

  const flowCount = summary.isError ? undefined : summary.data?.flows
  const deviceCount = devices.isError ? undefined : devices.data?.count
  const openAlertCount = alertSummary.isError
    ? undefined
    : alertSummary.data
      ? alertSummary.data.open_critical +
        alertSummary.data.open_warning +
        alertSummary.data.open_info
      : undefined
  // Critical alerts get a louder treatment so the operator can spot
  // them without reading the number. Warning/info still light up the
  // tab but in the muted style.
  const alertTone = alertSummary.data && alertSummary.data.open_critical > 0 ? 'crit' : undefined

  return (
    <div
      className="grid items-center bg-ink border-b border-line"
      style={{ gridTemplateColumns: '200px 1fr 320px 320px' }}
    >
      <div className="flex items-center gap-3 px-4 h-full border-r border-line">
        <span className="relative w-4 h-4 border-[1.5px] border-accent">
          <span className="absolute inset-[3px] bg-accent" />
        </span>
        <span className="font-semibold tracking-tight text-[14px]">
          FlowScope<span className="text-accent">.</span>
        </span>
      </div>
      <div className="flex h-full">
        <TabBtn id="overview" active={tab} onTab={onTab} label="Overview" />
        <TabBtn id="flows" active={tab} onTab={onTab} label="Flows" count={fmt.compact(flowCount)} />
        <TabBtn id="devices" active={tab} onTab={onTab} label="Devices" count={fmt.compact(deviceCount)} />
        <TabBtn id="alerts" active={tab} onTab={onTab} label="Alerts" count={fmt.compact(openAlertCount)} tone={alertTone} />
        <TabBtn id="settings" active={tab} onTab={onTab} label="Settings" />
      </div>
      <div className="px-4">
        <div className="flex items-center gap-2 px-3 h-7 bg-surface border border-line">
          <span className="text-faint text-[12px]">⌕</span>
          <input
            disabled
            placeholder="search exporters, interfaces, IPs (coming soon)"
            className="w-full text-[12.5px] placeholder:text-faint disabled:cursor-not-allowed bg-transparent outline-none"
          />
          <span className="text-faint text-[10.5px] border border-line px-1.5 py-px font-mono">⌘K</span>
        </div>
      </div>
      <div className="flex items-center justify-end gap-3 px-4 h-full border-l border-line">
        <ThemeToggle />
        <a className="text-faint text-[11px] hover:text-text" href="/metrics">/metrics</a>
        <span className="flex items-center gap-2 px-2 py-1 border border-line text-[12px]">
          <span className="font-medium">DL</span>
          <span className="text-dim">·</span>
          <span className="text-dim font-normal">exelao</span>
        </span>
      </div>
    </div>
  )
}

function TabBtn({
  id,
  active,
  onTab,
  label,
  count,
  tone,
}: {
  id: Tab
  active: Tab
  onTab: (t: Tab) => void
  label: string
  count?: string
  tone?: 'crit'
}) {
  const selected = active === id
  const countClass = tone === 'crit' ? 'text-crit' : 'text-faint'
  return (
    <button
      onClick={() => onTab(id)}
      aria-selected={selected}
      className={`relative px-4 h-full flex items-center gap-2 text-[13px] border-r border-line ${
        selected ? 'text-text' : 'text-dim hover:text-text hover:bg-surface'
      }`}
    >
      <span>{label}</span>
      {count && <span className={`font-mono text-[11px] tabular ${countClass}`}>{count}</span>}
      {selected && <span className="absolute left-0 right-0 -bottom-px h-0.5 bg-accent" />}
    </button>
  )
}

/* ----------------------------- Bottom command line ----------------------------- */

function Cmd() {
  return (
    <div className="flex items-center gap-4 px-4 border-t border-line bg-surface font-mono text-[10.5px] text-faint tracking-[0.02em]">
      <Kbd>↑↓</Kbd>
      <span>navigate</span>
      <Kbd>Enter</Kbd>
      <span>open</span>
      <Kbd>/</Kbd>
      <span>filter</span>
      <Kbd>⌘K</Kbd>
      <span>command</span>
      <Kbd>?</Kbd>
      <span>help</span>
      <div className="ml-auto flex gap-4">
        <a className="text-accent" href="/api/summary">api</a>
        <a className="text-accent" href="/metrics">metrics</a>
        <span>v1 · build dev</span>
      </div>
    </div>
  )
}

function Kbd({ children }: { children: ReactNode }) {
  return (
    <span className="text-dim border border-line px-1.5 py-px text-[10px]">{children}</span>
  )
}

