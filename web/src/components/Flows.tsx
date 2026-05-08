import { useQuery } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { api, fmt } from '../api'
import type {
  TopTalker,
  TopService,
  TopProtocol,
  TopConversation,
  TopNSort,
  TimeRangeArg,
} from '../api'
import { useFilters, toQuery, keyLabelFor, type Filter, type FilterKey } from '../filters'
import { rangeLabel, toApi, type TimeRange } from '../timeRange'
import { ServiceLabel, useServiceName } from './ServiceLabel'
import { LiveTail } from './LiveTail'

// Flows tab — page chrome:
//   Live tail (collapsible, expanded by default)
//   Filter bar (URL-synced filter chips)
//   Tab bar (Talkers / Services / Protocols / Conversations)
//     + sort selector (bytes / packets / flows)
//     + top-N selector (10 / 25 / 50, default 25)
//   Active panel
//
// Click any value (talker src, talker dst, service port, protocol,
// full 5-tuple) to add or replace a filter chip; chips re-narrow
// every panel's query and persist in the URL.

type TabId = 'talkers' | 'services' | 'protocols' | 'conversations'

const TAB_LABELS: Record<TabId, string> = {
  talkers: 'Top talkers',
  services: 'Top services',
  protocols: 'Top protocols',
  conversations: 'Top conversations',
}

const TOP_N_OPTIONS = [10, 25, 50] as const
const SORT_OPTIONS: { id: TopNSort; label: string }[] = [
  { id: 'bytes', label: 'bytes' },
  { id: 'packets', label: 'packets' },
  { id: 'flows', label: 'flows' },
]

export function Flows({
  range,
  rangeKey,
}: {
  range: TimeRange
  rangeKey: unknown
}) {
  const f = useFilters()
  const qs = toQuery(f.filters)
  const apiRange = toApi(range)
  const [tab, setTab] = useState<TabId>('talkers')
  const [sortBy, setSortBy] = useState<TopNSort>('bytes')
  const [topN, setTopN] = useState<number>(25)
  return (
    <div>
      <LiveTail />
      <FilterBar
        filters={f.filters}
        onRemove={f.remove}
        onClear={f.clear}
        range={range}
      />
      <TabBar
        tab={tab}
        onChangeTab={setTab}
        sortBy={sortBy}
        onChangeSort={setSortBy}
        topN={topN}
        onChangeTopN={setTopN}
      />
      <ActivePanel
        tab={tab}
        qs={qs}
        range={apiRange}
        rangeKey={rangeKey}
        sortBy={sortBy}
        topN={topN}
        onAdd={f.add}
      />
    </div>
  )
}

/* ----------------------------- Filter bar ----------------------------- */

function FilterBar({
  filters,
  onRemove,
  onClear,
  range,
}: {
  filters: Filter[]
  onRemove: (key: FilterKey, value?: string) => void
  onClear: () => void
  range: TimeRange
}) {
  const has = filters.length > 0
  return (
    <div className="flex items-center gap-2 px-4 py-3 border-b border-line bg-surface flex-wrap">
      <span className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold mr-1">
        Filters
      </span>
      <Chip neutral>window · {rangeLabel(range)}</Chip>
      {filters.map((f) => (
        <Chip key={`${f.key}_${f.value}`} onRemove={() => onRemove(f.key, f.value)}>
          <span className="text-faint">{f.keyLabel ?? keyLabelFor(f.key)} ·</span> {f.label ?? f.value}
        </Chip>
      ))}
      {has ? (
        <button
          className="ml-auto font-mono text-[11px] text-dim hover:text-text px-2 py-1 border border-line"
          onClick={onClear}
        >
          clear all
        </button>
      ) : (
        <span className="ml-auto font-mono text-[11px] text-faint italic">
          click any row to add a filter
        </span>
      )}
    </div>
  )
}

function Chip({
  children,
  neutral,
  onRemove,
}: {
  children: ReactNode
  neutral?: boolean
  onRemove?: () => void
}) {
  if (neutral) {
    return (
      <span className="font-mono text-[11px] px-2 py-1 border border-line text-dim">
        {children}
      </span>
    )
  }
  return (
    <span className="font-mono text-[11px] inline-flex items-center gap-1.5 pl-2 pr-1 py-1 border border-accent/40 bg-accent-wash text-text">
      <span>{children}</span>
      {onRemove && (
        <button
          onClick={onRemove}
          className="text-faint hover:text-crit text-[12px] leading-none px-1"
          aria-label="remove filter"
        >
          ×
        </button>
      )}
    </span>
  )
}

/* ----------------------------- Tab bar ----------------------------- */

function TabBar({
  tab,
  onChangeTab,
  sortBy,
  onChangeSort,
  topN,
  onChangeTopN,
}: {
  tab: TabId
  onChangeTab: (t: TabId) => void
  sortBy: TopNSort
  onChangeSort: (s: TopNSort) => void
  topN: number
  onChangeTopN: (n: number) => void
}) {
  return (
    <div className="flex items-center border-b border-line bg-ink">
      {(Object.keys(TAB_LABELS) as TabId[]).map((id) => (
        <Tab key={id} id={id} active={tab} onChange={onChangeTab}>
          {TAB_LABELS[id]}
        </Tab>
      ))}
      <div className="ml-auto flex items-center gap-3 px-4 py-2">
        <Selector
          label="sort"
          value={sortBy}
          onChange={(v) => onChangeSort(v as TopNSort)}
          options={SORT_OPTIONS.map((o) => ({ value: o.id, label: o.label }))}
        />
        <Selector
          label="show"
          value={String(topN)}
          onChange={(v) => onChangeTopN(Number(v))}
          options={TOP_N_OPTIONS.map((n) => ({ value: String(n), label: String(n) }))}
        />
      </div>
    </div>
  )
}

function Tab({
  id,
  active,
  onChange,
  children,
}: {
  id: TabId
  active: TabId
  onChange: (t: TabId) => void
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

function Selector({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string }[]
}) {
  return (
    <label className="flex items-center gap-1.5 font-mono text-[11px] text-faint">
      <span className="uppercase tracking-[0.1em]">{label}</span>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="bg-ink border border-line text-text px-1.5 py-0.5 text-[11.5px] outline-none focus:border-accent"
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  )
}

/* ----------------------------- Active panel ----------------------------- */

function ActivePanel({
  tab,
  qs,
  range,
  rangeKey,
  sortBy,
  topN,
  onAdd,
}: {
  tab: TabId
  qs: URLSearchParams
  range: TimeRangeArg
  rangeKey: unknown
  sortBy: TopNSort
  topN: number
  onAdd: (f: Filter) => void
}) {
  return (
    <Panel title={TAB_LABELS[tab]} sub={subtitleFor(tab, sortBy)} right="SOURCE · FLOWS">
      {tab === 'talkers' && (
        <TalkersList
          qs={qs}
          onAdd={onAdd}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
      {tab === 'services' && (
        <ServicesList
          qs={qs}
          onAdd={onAdd}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
      {tab === 'protocols' && (
        <ProtocolsList
          qs={qs}
          onAdd={onAdd}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
      {tab === 'conversations' && (
        <ConversationsList
          qs={qs}
          onAdd={onAdd}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
    </Panel>
  )
}

function subtitleFor(tab: TabId, sortBy: TopNSort): string {
  const by = `by ${sortBy}`
  switch (tab) {
    case 'talkers':
      return `src → dst · ${by}`
    case 'services':
      return `dst port · ${by}`
    case 'protocols':
      return `share of ${sortBy} total`
    case 'conversations':
      return `5-tuple · ${by}`
  }
}

/* ----------------------------- Panel chrome ----------------------------- */

function Panel({
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
    <section className="border-b border-line">
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
        {sub && <span className="font-mono text-[11px] text-faint">{sub}</span>}
        {right && (
          <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-accent">{right}</span>
        )}
      </div>
      <div>{children}</div>
    </section>
  )
}

/* ----------------------------- Per-panel lists ----------------------------- */

type ListBase = {
  qs: URLSearchParams
  onAdd: (f: Filter) => void
  range: TimeRangeArg
  rangeKey: unknown
  sortBy: TopNSort
  topN: number
}

function TalkersList({ qs, onAdd, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-talkers', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topTalkers(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r) => `${r.src_addr}>${r.dst_addr}`}
        renderLeft={(r: TopTalker) => (
          <span className="font-mono text-[12px]">
            <FilterTrigger value={r.src_addr} onAdd={onAdd} k="src_addr">
              {r.src_addr}
            </FilterTrigger>{' '}
            <span className="text-faint">→</span>{' '}
            <FilterTrigger value={r.dst_addr} onAdd={onAdd} k="dst_addr">
              {r.dst_addr}
            </FilterTrigger>
          </span>
        )}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ServicesList({ qs, onAdd, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-services', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topServices(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r) => `${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopService) => <TopServiceLeft r={r} onAdd={onAdd} />}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ProtocolsList({ qs, onAdd, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-protocols', qs.toString(), rangeKey, sortBy],
    queryFn: () => api.topProtocols(qs, range, sortBy),
  })
  // Protocols return all rows already sorted by the chosen dimension
  // server-side; we still slice to topN client-side so the visual
  // density matches Talkers / Services / Conversations.
  const rows = (q.data?.rows ?? []).slice(0, topN)
  const total = rows.reduce((a, r) => a + valueOfRow(sortBy, r), 0)
  return (
    <ListShell loading={q.isLoading} empty={!rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={rows}
        keyOf={(r) => String(r.proto)}
        renderLeft={(r: TopProtocol) => (
          <span className="font-mono text-[12px]">
            <FilterTrigger k="proto" value={String(r.proto)} onAdd={onAdd} label={fmt.proto(r.proto)}>
              <span className="text-text">{fmt.proto(r.proto)}</span>
            </FilterTrigger>{' '}
            <span className="text-faint">· {r.proto}</span>
          </span>
        )}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => {
          const v = valueOfRow(sortBy, r)
          return total > 0 ? `${((v / total) * 100).toFixed(1)}%` : '—'
        }}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ConversationsList({ qs, onAdd, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-conversations', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topConversations(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r) => `${r.src_addr}_${r.src_port}_${r.dst_addr}_${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopConversation) => (
          <span className="font-mono text-[12px] text-text">
            <FilterTrigger k="src_addr" value={r.src_addr} onAdd={onAdd}>
              {r.src_addr}
            </FilterTrigger>
            :{r.src_port}{' '}
            <span className="text-faint">→</span>{' '}
            <FilterTrigger k="dst_addr" value={r.dst_addr} onAdd={onAdd}>
              {r.dst_addr}
            </FilterTrigger>
            :{r.dst_port}{' '}
            <FilterTrigger k="proto" value={String(r.proto)} onAdd={onAdd} label={fmt.proto(r.proto)}>
              <span className="text-faint">· {fmt.proto(r.proto)}</span>
            </FilterTrigger>
          </span>
        )}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

/* ----------------------------- Filter trigger ----------------------------- */

function FilterTrigger({
  k,
  value,
  label,
  keyLabel,
  onAdd,
  children,
}: {
  k: FilterKey
  value: string
  label?: string
  keyLabel?: string
  onAdd: (f: Filter) => void
  children: ReactNode
}) {
  return (
    <button
      onClick={() => onAdd({ key: k, value, label, keyLabel })}
      className="hover:text-accent hover:underline decoration-dotted underline-offset-2"
    >
      {children}
    </button>
  )
}

/* ----------------------------- Generic list ----------------------------- */

function ListShell({
  loading,
  empty,
  error,
  children,
}: {
  loading: boolean
  empty: boolean
  error?: Error
  children: ReactNode
}) {
  if (loading) {
    return <div className="px-4 py-6 text-faint font-mono text-[12px]">loading…</div>
  }
  if (error) {
    return (
      <div className="px-4 py-6 text-crit font-mono text-[12px]">
        error · {error.message}
      </div>
    )
  }
  if (empty) {
    return (
      <div className="px-4 py-8 text-center text-[12px] font-mono text-dim">
        no data in window · drive synth or wait for exporter traffic
      </div>
    )
  }
  return <>{children}</>
}

function Rows<T>({
  rows,
  keyOf,
  renderLeft,
  renderRight,
  valueOf,
  sortBy: _sortBy,
}: {
  rows: T[]
  keyOf: (r: T) => string
  renderLeft: (r: T) => ReactNode
  renderRight: (r: T) => ReactNode
  valueOf: (r: T) => number
  sortBy: TopNSort
}) {
  const total = rows.reduce((a, r) => a + valueOf(r), 0)
  return (
    <ul>
      {rows.map((r) => {
        const v = valueOf(r)
        const pct = total > 0 ? (v / total) * 100 : 0
        return (
          <li key={keyOf(r)} className="px-4 py-2 border-b border-line-soft hover:bg-surface">
            <div className="flex items-baseline justify-between gap-3">
              <div className="min-w-0 truncate">{renderLeft(r)}</div>
              <div className="font-mono text-[12px] tabular text-text shrink-0">{renderRight(r)}</div>
            </div>
            <div className="mt-1.5 h-px bg-line w-full overflow-hidden">
              <div className="h-full bg-accent" style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
            </div>
          </li>
        )
      })}
    </ul>
  )
}

/* ----------------------------- Sort helpers ----------------------------- */

type Sortable = { bytes: number; packets?: number; flows?: number }

function valueOfRow(sortBy: TopNSort, r: Sortable): number {
  switch (sortBy) {
    case 'packets':
      return r.packets ?? 0
    case 'flows':
      return r.flows ?? 0
    default:
      return r.bytes
  }
}

function formatValue(sortBy: TopNSort, v: number): string {
  switch (sortBy) {
    case 'packets':
      return `${fmt.num(v)} pkts`
    case 'flows':
      return `${fmt.num(v)} flows`
    default:
      return fmt.bytes(v)
  }
}

/* ----------------------------- TopServiceLeft ----------------------------- */

// TopServiceLeft renders one row of the Top services panel. Lifted
// out of the renderLeft callback so we can call useServiceName here
// — the resolver uses the same /api/services/lookup path the rest of
// the app uses, so custom services land here without a UI change.
function TopServiceLeft({ r, onAdd }: { r: TopService; onAdd: (f: Filter) => void }) {
  const lookup = useServiceName(r.proto, r.dst_port)
  const labelText = lookup.data?.found
    ? lookup.data.primary.name
    : `port ${r.dst_port}`
  return (
    <span className="font-mono text-[12px]">
      <FilterTrigger
        k="dst_port"
        value={String(r.dst_port)}
        onAdd={onAdd}
        label={labelText}
        keyLabel="service"
      >
        <span className="text-text">
          <ServiceLabel proto={r.proto} port={r.dst_port} />
        </span>
      </FilterTrigger>{' '}
      <span className="text-faint">·</span>{' '}
      <FilterTrigger
        k="proto"
        value={String(r.proto)}
        onAdd={onAdd}
        label={fmt.proto(r.proto)}
      >
        <span className="text-faint">{fmt.proto(r.proto)}</span>
      </FilterTrigger>{' '}
      <span className="text-faint">{r.dst_port}</span>
    </span>
  )
}
