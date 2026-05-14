import { useQuery } from '@tanstack/react-query'
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { api, fmt } from './api'
import { getConfig } from './config'
import { Overview } from './components/Overview'
import { Flows } from './components/Flows'
import { Devices } from './components/Devices'
import { Alerts } from './components/Alerts'
import { Settings } from './components/Settings'
import { useQueryClient } from '@tanstack/react-query'
import { ThemeToggle } from './theme'
import { useTimeRange, useLiveInterval, type TimeRange } from './timeRange'
import { TimeRangeSelector } from './components/TimeRangeSelector'
import { setURLFilters, type Filter } from './filters'
type Tab = 'overview' | 'flows' | 'devices' | 'alerts' | 'settings'

const TAB_PARAM = 'tab'
const DEFAULT_TAB: Tab = 'overview'

const TAB_LABELS: Record<Tab, string> = {
  overview: 'Overview',
  flows: 'Flows',
  devices: 'Devices',
  alerts: 'Alerts',
  settings: 'Settings',
}

const VALID_TABS: ReadonlySet<Tab> = new Set<Tab>([
  'overview',
  'flows',
  'devices',
  'alerts',
  'settings',
])

const TIME_TABS: ReadonlySet<Tab> = new Set<Tab>(['overview', 'flows', 'devices'])

function readTabFromURL(): Tab {
  if (typeof window === 'undefined') return DEFAULT_TAB
  const v = new URLSearchParams(window.location.search).get(TAB_PARAM)
  return v && VALID_TABS.has(v as Tab) ? (v as Tab) : DEFAULT_TAB
}

// Params that are scoped to a single tab and should not linger in the
// URL once the operator navigates elsewhere. ?device= is meaningful
// only on Devices; ?s= (Settings sub-section) only on Settings.
const TAB_SCOPED_PARAMS: Partial<Record<Tab, readonly string[]>> = {
  devices: ['device'],
  settings: ['s', 'item'],
}

function writeTabToURL(next: Tab) {
  if (typeof window === 'undefined') return
  const sp = new URLSearchParams(window.location.search)
  if (next === DEFAULT_TAB) sp.delete(TAB_PARAM)
  else sp.set(TAB_PARAM, next)
  // Strip any tab-scoped params that don't belong on the destination tab.
  for (const [tab, params] of Object.entries(TAB_SCOPED_PARAMS) as [Tab, readonly string[]][]) {
    if (tab === next) continue
    for (const p of params) sp.delete(p)
  }
  const qs = sp.toString()
  const nextHref = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  if (window.location.pathname + window.location.search !== nextHref) {
    window.history.replaceState({}, '', nextHref)
  }
}

export function App() {
  // Tab lives in the URL via ?tab=…  so a reload or a shared link lands
  // on the same surface. Default tab (overview) is omitted from the URL
  // to keep clean links short. The popstate listener restores state on
  // back/forward navigation.
  const [tab, setTabState] = useState<Tab>(() => readTabFromURL())
  useEffect(() => {
    const onPop = () => setTabState(readTabFromURL())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  const setTab = useCallback((next: Tab) => {
    // Render-on-state-change: flip state synchronously, then write the
    // URL — so the tab re-renders in the same paint as the click.
    setTabState(next)
    writeTabToURL(next)
  }, [])
  // navigateToFlowsInvestigate lets any tab hand off to Flows →
  // Investigate with a pre-seeded chip set. Chips are written to the
  // URL before the tab swap so when Flows() mounts and reads from URL
  // they're already in place — render-on-state-change.
  const navigateToFlowsInvestigate = useCallback(
    (chips: Filter[]) => {
      setURLFilters(chips, { fs: 'investigate' })
      setTab('flows')
    },
    [setTab],
  )
  const tr = useTimeRange()
  const showRange = TIME_TABS.has(tab)
  return (
    <div
      className="grid bg-ink text-text overflow-hidden h-screen"
      style={{ gridTemplateRows: '32px 44px 32px 1fr 24px' }}
    >
      <Strip />
      <Bar tab={tab} onTab={setTab} />
      <PageContext tab={tab} showRange={showRange} range={tr.range} onRangeChange={tr.set} />
      <main className="overflow-auto" data-testid={`tab-panel-${tab}`}>
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
            onInvestigate={navigateToFlowsInvestigate}
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
        <span className="ml-auto flex items-center gap-2">
          <RefreshButton range={range} />
          <TimeRangeSelector range={range} onChange={onRangeChange} />
        </span>
      )}
    </div>
  )
}

// RefreshButton forces a one-shot refetch across every active query.
// Useful as a "force now" shortcut in live mode, and the only way to
// pull fresh data when the operator has pinned an absolute window
// (the QueryClient defaults pause auto-refresh in that mode).
function RefreshButton({ range }: { range: TimeRange }) {
  const qc = useQueryClient()
  const [spinning, setSpinning] = useState(false)
  const click = async () => {
    setSpinning(true)
    try {
      await qc.invalidateQueries()
    } finally {
      setTimeout(() => setSpinning(false), 400)
    }
  }
  const fixed = range.kind === 'absolute'
  return (
    <button
      type="button"
      onClick={click}
      aria-label="Refresh now"
      title={
        fixed
          ? 'Refresh — fixed time range pauses auto-refresh'
          : 'Refresh now'
      }
      className={`font-mono text-[11px] px-2 py-1 border ${
        fixed
          ? 'border-accent text-accent hover:bg-accent-wash'
          : 'border-line text-dim hover:border-accent hover:text-text'
      }`}
    >
      <span
        className={`inline-block ${spinning ? 'animate-spin' : ''}`}
        aria-hidden
      >
        ↻
      </span>{' '}
      refresh
    </button>
  )
}

/* ----------------------------- Top strip ----------------------------- */

function Strip() {
  const { range } = useTimeRange()
  const summary = useQuery({
    queryKey: ['summary', 'strip'],
    queryFn: () => api.summary(60),
    refetchInterval: useLiveInterval(2000),
  })
  const ifaces = useQuery({
    queryKey: ['interfaces', 'strip'],
    queryFn: () => api.interfaces(60),
    refetchInterval: useLiveInterval(5000),
  })
  const [now, setNow] = useState<string>(() => new Date().toUTCString())
  useEffect(() => {
    const id = setInterval(() => setNow(new Date().toUTCString()), 1000)
    return () => clearInterval(id)
  }, [])

  const flows = summary.data?.flows ?? 0
  const exporters = summary.data?.exporters ?? 0
  const ifaceCount = ifaces.data?.count ?? 0
  // Status dot reflects live-vs-fixed mode plus connection health.
  // The mode part is what's actionable: in fixed mode the page is a
  // frozen snapshot and the operator should know nothing is moving
  // behind their back. "error" stays louder than "paused" because
  // it's a failure, not a deliberate state.
  const fixed = range.kind === 'absolute'
  const status: 'error' | 'connecting' | 'paused' | 'live' = summary.error
    ? 'error'
    : summary.isLoading
      ? 'connecting'
      : fixed
        ? 'paused'
        : 'live'
  const statusDot =
    status === 'error'
      ? 'bg-crit'
      : status === 'live'
        ? 'bg-ok'
        : status === 'paused'
          ? 'bg-dim'
          : 'bg-warn'
  const statusText =
    status === 'error'
      ? 'text-crit'
      : status === 'live'
        ? 'text-ok'
        : status === 'paused'
          ? 'text-dim'
          : 'text-warn'

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
    refetchInterval: useLiveInterval(2000),
  })
  const devices = useQuery({
    queryKey: ['devices', 'bar'],
    queryFn: () => api.devices(300),
    refetchInterval: useLiveInterval(10_000),
  })
  const alertSummary = useQuery({
    queryKey: ['alertSummary', 'bar'],
    queryFn: () => api.alertSummary(),
    refetchInterval: useLiveInterval(5_000),
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
          {getConfig().display_name}
          <span className="text-accent">.</span>
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
        <UserChip />
      </div>
    </div>
  )
}

/* ----------------------------- User chip ----------------------------- */

// UserChip fetches /auth/me on app boot. When an OIDC session exists
// it renders the operator's initials + email; otherwise it falls back
// to the unauthenticated marker. Clicking the chip when signed-in
// opens a small logout menu; when signed-out it doesn't render the
// menu at all (the Settings page is where you wire OIDC and click
// "Sign in with SSO").
function UserChip() {
  const me = useQuery({
    queryKey: ['auth-me'],
    queryFn: () => api.authMe(),
    // The /auth/me endpoint is cheap and stateless; refetching on
    // window focus keeps the chip honest if the operator just logged
    // out in another tab. The 2s default refetchInterval (set on the
    // QueryClient) is overkill for an identity check and causes a
    // visible chip flicker on retry — disable it here.
    refetchOnWindowFocus: true,
    refetchInterval: false,
    retry: false,
    staleTime: 30_000,
  })
  const [open, setOpen] = useState(false)
  if (me.isLoading) {
    return (
      <span className="flex items-center gap-2 px-2 py-1 border border-line text-[12px] text-faint">
        …
      </span>
    )
  }
  if (!me.data) {
    return (
      <span className="flex items-center gap-2 px-2 py-1 border border-line text-[12px] text-dim">
        <span className="font-normal">signed out</span>
      </span>
    )
  }
  const email = me.data.email || me.data.subject
  const initials = email
    .split(/[.@]/)
    .filter(Boolean)
    .slice(0, 2)
    .map((s) => s[0]?.toUpperCase())
    .join('') || '·'
  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-2 px-2 py-1 border border-line text-[12px] hover:bg-surface"
        aria-expanded={open}
      >
        <span className="font-medium">{initials}</span>
        <span className="text-dim">·</span>
        <span className="text-dim font-normal truncate max-w-[16ch]" title={email}>{email}</span>
      </button>
      {open && (
        <div className="absolute right-0 mt-1 w-56 border border-line bg-surface z-10">
          <div className="px-3 py-2 border-b border-line/60 text-[11px] text-faint font-mono">
            scope · {me.data.scope}
          </div>
          <button
            type="button"
            // Render-on-state-change rule: close the menu first, then
            // perform the async work. Otherwise the menu lingers
            // while the logout request flies and the dashboard feels
            // unresponsive.
            onClick={async () => {
              setOpen(false)
              try {
                await api.authLogout()
              } catch {
                /* clearing the cookie is best-effort */
              }
              // Reload so every cached query refetches without a stale
              // session in flight. The brand bar re-resolves /auth/me
              // and switches back to "signed out".
              window.location.assign('/')
            }}
            className="block w-full text-left px-3 py-2 text-[12px] hover:bg-raise"
          >
            sign out
          </button>
        </div>
      )}
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
      data-testid={`tab-${id}`}
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

