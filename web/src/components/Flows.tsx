import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { api, fmt } from '../api'
import type {
  FlowsListDir,
  FlowsListResponse,
  FlowsListSort,
  RecentFlow,
  TopASN,
  TopInterface,
  TopTalker,
  TopService,
  TopProtocol,
  TopConversation,
  TopNSort,
  TimeRangeArg,
} from '../api'
import { labelExporter, labelInterface } from '../api'
import { useFilters, toQuery, keyLabelFor, FILTER_KEYS, type Filter, type FilterKey } from '../filters'
import { rangeLabel, toApi, type TimeRange } from '../timeRange'
import { ServiceLabel, useServiceName } from './ServiceLabel'
import { LiveTail } from './LiveTail'
import { FlowDrawer, type FlowDrillDown } from './FlowDrawer'
import { Hostname, useReverseDNS } from './Hostname'

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

type TabId = 'talkers' | 'services' | 'protocols' | 'conversations' | 'asn' | 'interfaces'

const TAB_LABELS: Record<TabId, string> = {
  talkers: 'Top talkers',
  services: 'Top services',
  protocols: 'Top protocols',
  conversations: 'Top conversations',
  asn: 'Top ASN',
  interfaces: 'Top interfaces',
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
  const [drill, setDrill] = useState<FlowDrillDown | null>(null)
  return (
    <div>
      <LiveTail />
      <FilterBar
        filters={f.filters}
        onAdd={f.add}
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
        onDrill={setDrill}
      />
      <Investigate
        qs={qs}
        range={apiRange}
        rangeKey={rangeKey}
        onAdd={f.add}
        onDrill={setDrill}
      />
      <FlowDrawer
        drill={drill}
        pageFilters={qs}
        range={apiRange}
        onClose={() => setDrill(null)}
      />
    </div>
  )
}

/* ----------------------------- Filter bar ----------------------------- */

function FilterBar({
  filters,
  onAdd,
  onRemove,
  onClear,
  range,
}: {
  filters: Filter[]
  onAdd: (f: Filter) => void
  onRemove: (key: FilterKey, value?: string) => void
  onClear: () => void
  range: TimeRange
}) {
  const has = filters.length > 0
  const [building, setBuilding] = useState(false)
  return (
    <div className="border-b border-line bg-surface">
      <div className="flex items-center gap-2 px-4 py-3 flex-wrap">
        <span className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold mr-1">
          Filters
        </span>
        <Chip neutral>window · {rangeLabel(range)}</Chip>
        {filters.map((f) => (
          <Chip key={`${f.key}_${f.value}`} onRemove={() => onRemove(f.key, f.value)}>
            <span className="text-faint">{f.keyLabel ?? keyLabelFor(f.key)} ·</span>{' '}
            {f.label ?? f.value}
          </Chip>
        ))}
        <button
          onClick={() => setBuilding((b) => !b)}
          aria-expanded={building}
          aria-controls="filter-builder"
          className="font-mono text-[11px] inline-flex items-center gap-1.5 px-2 py-1 border border-line text-dim hover:border-accent hover:text-text"
        >
          <span aria-hidden className="text-accent">+</span>
          add filter
        </button>
        {has && (
          <button
            className="font-mono text-[11px] text-dim hover:text-text px-2 py-1 border border-line"
            onClick={onClear}
          >
            clear all
          </button>
        )}
        {!has && !building && (
          <span className="ml-auto font-mono text-[11px] text-faint italic">
            click any row, or use <span className="text-dim">+ add filter</span>
          </span>
        )}
      </div>
      {building && (
        <FilterBuilder
          existing={filters}
          onAdd={(f) => {
            onAdd(f)
            setBuilding(false)
          }}
          onCancel={() => setBuilding(false)}
        />
      )}
    </div>
  )
}

/* ----------------------------- Filter builder ----------------------------- */

// FilterBuilder is the cold-start filter form. Lets the operator
// type a value for any FilterKey without having to first see it on
// screen. Validates per-key (IP, port number, ifindex) and submits
// a Filter through onAdd. Pre-populates the value if the same key
// already has a chip — adding the form's value replaces that chip
// (one-value-per-key invariant in useFilters).
function FilterBuilder({
  existing,
  onAdd,
  onCancel,
}: {
  existing: Filter[]
  onAdd: (f: Filter) => void
  onCancel: () => void
}) {
  const [key, setKey] = useState<FilterKey>('exporter')
  const [value, setValue] = useState<string>('')
  const [label, setLabel] = useState<string>('')
  const [error, setError] = useState<string | null>(null)
  const placeholder = PLACEHOLDERS[key]
  const submit = () => {
    const v = value.trim()
    if (!v) {
      setError('value required')
      return
    }
    const validationError = validateValue(key, v)
    if (validationError) {
      setError(validationError)
      return
    }
    const f: Filter = { key, value: v }
    const lbl = label.trim()
    if (lbl && lbl !== v) f.label = lbl
    onAdd(f)
    setValue('')
    setLabel('')
    setError(null)
  }
  // Show a hint when the chosen key already has a chip so the
  // operator knows submitting will replace it.
  const replaces = existing.find((e) => e.key === key)
  return (
    <div
      id="filter-builder"
      className="px-4 py-3 border-t border-line-soft bg-ink flex items-center gap-2 flex-wrap"
    >
      <span className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold mr-1">
        New filter
      </span>
      <select
        value={key}
        onChange={(e) => {
          setKey(e.target.value as FilterKey)
          setError(null)
        }}
        className="font-mono text-[12px] bg-surface border border-line text-text px-2 py-1 outline-none focus:border-accent"
      >
        {FILTER_KEYS.map((k) => (
          <option key={k} value={k}>
            {keyLabelFor(k)}
          </option>
        ))}
      </select>
      <input
        value={value}
        onChange={(e) => {
          setValue(e.target.value)
          setError(null)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') submit()
          else if (e.key === 'Escape') onCancel()
        }}
        placeholder={placeholder}
        className="font-mono text-[12px] bg-surface border border-line text-text px-2 py-1 outline-none focus:border-accent w-[220px]"
        autoFocus
      />
      <input
        value={label}
        onChange={(e) => setLabel(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') submit()
          else if (e.key === 'Escape') onCancel()
        }}
        placeholder="display label (optional)"
        className="font-mono text-[12px] bg-surface border border-line text-dim placeholder:text-faint px-2 py-1 outline-none focus:border-accent w-[180px]"
      />
      <button
        onClick={submit}
        className="font-mono text-[11px] px-2.5 py-1 border border-accent bg-accent-wash text-text hover:bg-accent hover:text-ink"
      >
        add
      </button>
      <button
        onClick={onCancel}
        className="font-mono text-[11px] px-2.5 py-1 border border-line text-dim hover:text-text"
      >
        cancel
      </button>
      {error && (
        <span className="font-mono text-[11px] text-crit">· {error}</span>
      )}
      {!error && replaces && (
        <span className="font-mono text-[11px] text-faint italic">
          replaces {keyLabelFor(replaces.key)} · {replaces.label ?? replaces.value}
        </span>
      )}
    </div>
  )
}

const PLACEHOLDERS: Record<FilterKey, string> = {
  exporter: 'exporter IP (e.g. 10.110.0.182)',
  src_addr: 'source IP',
  dst_addr: 'destination IP',
  src_port: 'source port (1–65535)',
  dst_port: 'destination port',
  proto: 'protocol number (6=TCP, 17=UDP)',
  input_ifindex: 'input ifindex',
  output_ifindex: 'output ifindex',
  src_as: 'source ASN (e.g. 13335)',
  dst_as: 'destination ASN',
}

function validateValue(key: FilterKey, value: string): string | null {
  switch (key) {
    case 'exporter':
    case 'src_addr':
    case 'dst_addr':
      return /^[0-9a-fA-F:.]+$/.test(value) ? null : 'expected an IP address'
    case 'src_port':
    case 'dst_port': {
      const n = Number(value)
      if (!Number.isInteger(n) || n < 1 || n > 65535) return 'port 1–65535'
      return null
    }
    case 'proto': {
      const n = Number(value)
      if (!Number.isInteger(n) || n < 0 || n > 255) return 'proto 0–255'
      return null
    }
    case 'input_ifindex':
    case 'output_ifindex':
    case 'src_as':
    case 'dst_as': {
      const n = Number(value)
      if (!Number.isInteger(n) || n < 0) return 'non-negative integer'
      return null
    }
  }
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
  onDrill,
}: {
  tab: TabId
  qs: URLSearchParams
  range: TimeRangeArg
  rangeKey: unknown
  sortBy: TopNSort
  topN: number
  onAdd: (f: Filter) => void
  onDrill: (d: FlowDrillDown) => void
}) {
  return (
    <Panel title={TAB_LABELS[tab]} sub={subtitleFor(tab, sortBy)} right="SOURCE · FLOWS">
      {tab === 'talkers' && (
        <TalkersList
          qs={qs}
          onAdd={onAdd}
          onDrill={onDrill}
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
          onDrill={onDrill}
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
          onDrill={onDrill}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
      {tab === 'asn' && (
        <ASNList
          qs={qs}
          onAdd={onAdd}
          onDrill={onDrill}
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
          onDrill={onDrill}
          range={range}
          rangeKey={rangeKey}
          sortBy={sortBy}
          topN={topN}
        />
      )}
      {tab === 'interfaces' && (
        <InterfacesList
          qs={qs}
          onAdd={onAdd}
          onDrill={onDrill}
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
    case 'asn':
      return `src AS → dst AS · ${by}`
    case 'interfaces':
      return `per exporter ifindex · in + out · ${by}`
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
  onDrill: (d: FlowDrillDown) => void
  range: TimeRangeArg
  rangeKey: unknown
  sortBy: TopNSort
  topN: number
}

function TalkersList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-talkers', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topTalkers(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r) => `${r.src_addr}>${r.dst_addr}`}
        renderLeft={(r: TopTalker) => <TalkerLeft r={r} onAdd={onAdd} />}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        drillFor={(r) => ({
          title: `${r.src_addr} → ${r.dst_addr}`,
          subtitle: 'src→dst pair',
          filters: [
            { key: 'src_addr', value: r.src_addr },
            { key: 'dst_addr', value: r.dst_addr },
          ],
        })}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

// TalkerLeft renders one row of the Top talkers panel. Lifted out of
// renderLeft so it can call useReverseDNS — the same query <Hostname>
// already fires for these IPs, so TanStack dedupes and we get the
// rDNS string for free to use as the chip label. Falls back to the
// IP when no PTR resolves so the chip never flashes empty on click.
function TalkerLeft({ r, onAdd }: { r: TopTalker; onAdd: (f: Filter) => void }) {
  const srcRDNS = useReverseDNS(r.src_addr)?.hostname
  const dstRDNS = useReverseDNS(r.dst_addr)?.hostname
  return (
    <span className="font-mono text-[12px]">
      <FilterTrigger value={r.src_addr} onAdd={onAdd} k="src_addr" label={srcRDNS || undefined}>
        {r.src_addr}
      </FilterTrigger>
      <Hostname ip={r.src_addr} />{' '}
      <span className="text-faint">→</span>{' '}
      <FilterTrigger value={r.dst_addr} onAdd={onAdd} k="dst_addr" label={dstRDNS || undefined}>
        {r.dst_addr}
      </FilterTrigger>
      <Hostname ip={r.dst_addr} />
    </span>
  )
}

function ServicesList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
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
        drillFor={(r) => ({
          title: `${fmt.proto(r.proto)} port ${r.dst_port}`,
          subtitle: 'service',
          filters: [
            { key: 'dst_port', value: String(r.dst_port) },
            { key: 'proto', value: String(r.proto), label: fmt.proto(r.proto) },
          ],
        })}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ProtocolsList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
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
        drillFor={(r) => ({
          title: `protocol ${fmt.proto(r.proto)}`,
          subtitle: `IP proto ${r.proto}`,
          filters: [
            { key: 'proto', value: String(r.proto), label: fmt.proto(r.proto) },
          ],
        })}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ASNList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-asn', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topASN(qs, range, topN, sortBy),
  })
  const rows = (q.data?.rows ?? []).filter((r) => r.src_as !== 0 || r.dst_as !== 0)
  const dropped = (q.data?.rows.length ?? 0) - rows.length
  return (
    <ListShell loading={q.isLoading} empty={!rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={rows}
        keyOf={(r: TopASN) => `${r.src_as}>${r.dst_as}`}
        renderLeft={(r) => (
          <span className="font-mono text-[12px] text-text">
            {r.src_as !== 0 ? (
              <FilterTrigger
                k="src_as"
                value={String(r.src_as)}
                onAdd={onAdd}
                label={`AS${r.src_as}`}
                keyLabel="src AS"
              >
                AS{r.src_as}
              </FilterTrigger>
            ) : (
              <span className="text-faint">AS—</span>
            )}{' '}
            <span className="text-faint">→</span>{' '}
            {r.dst_as !== 0 ? (
              <FilterTrigger
                k="dst_as"
                value={String(r.dst_as)}
                onAdd={onAdd}
                label={`AS${r.dst_as}`}
                keyLabel="dst AS"
              >
                AS{r.dst_as}
              </FilterTrigger>
            ) : (
              <span className="text-faint">AS—</span>
            )}
            {dropped > 0 && r === rows[0] && (
              <span className="ml-3 font-mono text-[10.5px] text-faint italic">
                · {dropped} unknown AS row{dropped === 1 ? '' : 's'} hidden
              </span>
            )}
          </span>
        )}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        drillFor={(r) => {
          const filters: Filter[] = []
          if (r.src_as !== 0) {
            filters.push({
              key: 'src_as',
              value: String(r.src_as),
              label: `AS${r.src_as}`,
            })
          }
          if (r.dst_as !== 0) {
            filters.push({
              key: 'dst_as',
              value: String(r.dst_as),
              label: `AS${r.dst_as}`,
            })
          }
          return {
            title:
              r.src_as && r.dst_as
                ? `AS${r.src_as} → AS${r.dst_as}`
                : r.src_as
                  ? `AS${r.src_as} → unknown`
                  : `unknown → AS${r.dst_as}`,
            subtitle: 'src AS → dst AS',
            filters,
          }
        }}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function ConversationsList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-conversations', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topConversations(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r) => `${r.src_addr}_${r.src_port}_${r.dst_addr}_${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopConversation) => <ConversationLeft r={r} onAdd={onAdd} />}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => formatValue(sortBy, valueOfRow(sortBy, r))}
        drillFor={(r) => ({
          title: `${r.src_addr}:${r.src_port} → ${r.dst_addr}:${r.dst_port}`,
          subtitle: `5-tuple · ${fmt.proto(r.proto)}`,
          filters: [
            { key: 'src_addr', value: r.src_addr },
            { key: 'src_port', value: String(r.src_port) },
            { key: 'dst_addr', value: r.dst_addr },
            { key: 'dst_port', value: String(r.dst_port) },
            { key: 'proto', value: String(r.proto), label: fmt.proto(r.proto) },
          ],
        })}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

function InterfacesList({ qs, onAdd, onDrill, range, rangeKey, sortBy, topN }: ListBase) {
  const q = useQuery({
    queryKey: ['top-interfaces', qs.toString(), rangeKey, sortBy, topN],
    queryFn: () => api.topInterfaces(qs, range, topN, sortBy),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        keyOf={(r: TopInterface) => `${r.exporter}_${r.ifindex}`}
        renderLeft={(r) => <InterfaceLeft r={r} onAdd={onAdd} />}
        valueOf={(r) => valueOfRow(sortBy, r)}
        renderRight={(r) => <InterfaceRight r={r} sortBy={sortBy} />}
        drillFor={(r) => {
          // Default drill = traffic exiting this interface on this
          // exporter (the most operator-intuitive read of "this
          // interface's traffic"). Operator can pivot to ingress with
          // the dedicated "in" trigger on the left side.
          const exp = labelExporter(r)
          const ifLbl = labelInterface(r)
          return {
            title: `${exp.primary} · ${ifLbl.primary}`,
            subtitle: 'egress on this interface',
            filters: [
              { key: 'exporter', value: r.exporter, label: r.sys_name || undefined },
              {
                key: 'output_ifindex',
                value: String(r.ifindex),
                label: r.if_descr || `ifindex ${r.ifindex}`,
              },
            ],
          }
        }}
        onDrill={onDrill}
        sortBy={sortBy}
      />
    </ListShell>
  )
}

// InterfaceLeft renders the exporter + interface identity and the two
// FilterTriggers that pivot the rest of the page to ingress-only or
// egress-only traffic on this ifindex. Stacked label on top mirrors
// the Devices tab's TwoLine pattern.
function InterfaceLeft({ r, onAdd }: { r: TopInterface; onAdd: (f: Filter) => void }) {
  const exp = labelExporter(r)
  const ifLbl = labelInterface(r)
  const ifValue = String(r.ifindex)
  const ifLabel = r.if_descr || `ifindex ${r.ifindex}`
  return (
    <span className="font-mono text-[12px] inline-flex items-baseline gap-2 min-w-0">
      <FilterTrigger k="exporter" value={r.exporter} onAdd={onAdd} label={r.sys_name || undefined}>
        <span className="text-text truncate">{exp.primary}</span>
      </FilterTrigger>
      <span className="text-faint shrink-0">·</span>
      <span className="text-text truncate">{ifLbl.primary}</span>
      <span className="text-faint shrink-0">
        ifindex {r.ifindex}
      </span>
      <span className="ml-1 shrink-0 inline-flex items-baseline gap-1">
        <FilterTrigger
          k="input_ifindex"
          value={ifValue}
          onAdd={onAdd}
          label={ifLabel}
          keyLabel="in iface"
        >
          <span className="text-faint hover:text-accent">[in]</span>
        </FilterTrigger>
        <FilterTrigger
          k="output_ifindex"
          value={ifValue}
          onAdd={onAdd}
          label={ifLabel}
          keyLabel="out iface"
        >
          <span className="text-faint hover:text-accent">[out]</span>
        </FilterTrigger>
      </span>
    </span>
  )
}

// InterfaceRight shows the directional split for the active sort
// dimension. "← 1.2 MiB · → 800 KiB" reads naturally: arrow into
// the interface is ingress, arrow out is egress. Total (used for the
// row's progress bar) lives in the Rows component via valueOf.
function InterfaceRight({ r, sortBy }: { r: TopInterface; sortBy: TopNSort }) {
  const inV =
    sortBy === 'packets' ? r.in_packets : sortBy === 'flows' ? r.in_flows : r.in_bytes
  const outV =
    sortBy === 'packets' ? r.out_packets : sortBy === 'flows' ? r.out_flows : r.out_bytes
  return (
    <span className="font-mono text-[12px] tabular text-text inline-flex items-baseline gap-2">
      <span>
        <span className="text-faint">←</span> {formatValue(sortBy, inV)}
      </span>
      <span className="text-faint">·</span>
      <span>
        <span className="text-faint">→</span> {formatValue(sortBy, outV)}
      </span>
    </span>
  )
}

// ConversationLeft renders one row of the Top conversations panel.
// Same rationale as TalkerLeft — useReverseDNS dedupes against the
// <Hostname> queries already in flight, so we get rDNS strings to
// label src/dst chips for free.
function ConversationLeft({ r, onAdd }: { r: TopConversation; onAdd: (f: Filter) => void }) {
  const srcRDNS = useReverseDNS(r.src_addr)?.hostname
  const dstRDNS = useReverseDNS(r.dst_addr)?.hostname
  return (
    <span className="font-mono text-[12px] text-text">
      <FilterTrigger k="src_addr" value={r.src_addr} onAdd={onAdd} label={srcRDNS || undefined}>
        {r.src_addr}
      </FilterTrigger>
      :{r.src_port}
      <Hostname ip={r.src_addr} />{' '}
      <span className="text-faint">→</span>{' '}
      <FilterTrigger k="dst_addr" value={r.dst_addr} onAdd={onAdd} label={dstRDNS || undefined}>
        {r.dst_addr}
      </FilterTrigger>
      :{r.dst_port}
      <Hostname ip={r.dst_addr} />{' '}
      <FilterTrigger k="proto" value={String(r.proto)} onAdd={onAdd} label={fmt.proto(r.proto)}>
        <span className="text-faint">· {fmt.proto(r.proto)}</span>
      </FilterTrigger>
    </span>
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
  drillFor,
  onDrill,
  sortBy: _sortBy,
}: {
  rows: T[]
  keyOf: (r: T) => string
  renderLeft: (r: T) => ReactNode
  renderRight: (r: T) => ReactNode
  valueOf: (r: T) => number
  drillFor?: (r: T) => FlowDrillDown
  onDrill?: (d: FlowDrillDown) => void
  sortBy: TopNSort
}) {
  const total = rows.reduce((a, r) => a + valueOf(r), 0)
  return (
    <ul>
      {rows.map((r) => {
        const v = valueOf(r)
        const pct = total > 0 ? (v / total) * 100 : 0
        const drill = drillFor?.(r)
        return (
          <li
            key={keyOf(r)}
            className="px-4 py-2 border-b border-line-soft hover:bg-surface group"
          >
            <div className="flex items-baseline justify-between gap-3">
              <div className="min-w-0 truncate">{renderLeft(r)}</div>
              <div className="flex items-center gap-3 shrink-0">
                <div className="font-mono text-[12px] tabular text-text">{renderRight(r)}</div>
                {drill && onDrill && (
                  <button
                    type="button"
                    onClick={() => onDrill(drill)}
                    className="font-mono text-[10.5px] tracking-[0.06em] text-accent opacity-0 group-hover:opacity-100 hover:underline"
                  >
                    inspect →
                  </button>
                )}
              </div>
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

/* ----------------------------- Investigate ----------------------------- */

const PAGE_SIZE_OPTIONS = [50, 100, 200] as const
const COLLAPSE_KEY = 'flowscope.investigate.collapsed'

// Investigate is the bottom panel of the Flows tab. Paginated,
// server-sorted (observed / bytes / packets), filtered through the
// same chip set as the Top panels above. Click any cell value to add
// or replace a chip — the page scrolls back to the top of Investigate
// after a chip change so you don't lose context to a stale page
// number.
function Investigate({
  qs,
  range,
  rangeKey,
  onAdd,
  onDrill,
}: {
  qs: URLSearchParams
  range: TimeRangeArg
  rangeKey: unknown
  onAdd: (f: Filter) => void
  onDrill: (d: FlowDrillDown) => void
}) {
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    try {
      return localStorage.getItem(COLLAPSE_KEY) === '1'
    } catch {
      return false
    }
  })
  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSE_KEY, collapsed ? '1' : '0')
    } catch {
      // ignore
    }
  }, [collapsed])
  const [pageSize, setPageSize] = useState<number>(50)
  const [page, setPage] = useState<number>(0)
  const [sort, setSort] = useState<FlowsListSort>('observed')
  const [dir, setDir] = useState<FlowsListDir>('desc')

  // Reset to page 0 whenever the underlying narrowing changes — paged
  // offset against a different result set is meaningless.
  const filterKey = qs.toString()
  useEffect(() => {
    setPage(0)
  }, [filterKey, pageSize, sort, dir])

  const offset = page * pageSize
  // Investigate is for studying — no auto-refresh, no window-focus
  // refetch. Operator triggers refresh by changing filters, sort,
  // page size, or hitting next/prev.
  const q = useQuery({
    queryKey: ['flows-list', filterKey, rangeKey, pageSize, offset, sort, dir],
    queryFn: () =>
      api.flowsList(qs, range, {
        limit: pageSize,
        offset,
        sort,
        dir,
      }),
    enabled: !collapsed,
    refetchOnWindowFocus: false,
  })
  const data: FlowsListResponse | undefined = q.data
  const flows: RecentFlow[] = data?.flows ?? []
  const hasNext = flows.length === pageSize
  const showingFrom = flows.length === 0 ? 0 : offset + 1
  const showingTo = offset + flows.length
  const cycleSort = (k: FlowsListSort) => {
    if (k === sort) {
      setDir((d) => (d === 'asc' ? 'desc' : 'asc'))
    } else {
      setSort(k)
      setDir('desc')
    }
  }

  // j/k row navigation. The highlighted index is reset whenever the
  // result set changes shape (filters, page, sort) so we don't carry
  // a stale row pointer past results that no longer exist. Refs to
  // each <tr> let us call scrollIntoView when the highlight moves
  // off-screen — keyboard users shouldn't have to mouse to follow
  // their own cursor.
  const [highlight, setHighlight] = useState(0)
  const rowRefs = useRef<Array<HTMLTableRowElement | null>>([])
  useEffect(() => {
    setHighlight(0)
    rowRefs.current = []
  }, [filterKey, rangeKey, pageSize, offset, sort, dir])
  // Clamp if flow count shrinks under the highlighted index between
  // refetches with the same shape — defensive, normally a no-op.
  useEffect(() => {
    if (flows.length === 0) {
      if (highlight !== 0) setHighlight(0)
    } else if (highlight >= flows.length) {
      setHighlight(flows.length - 1)
    }
  }, [flows.length, highlight])
  useEffect(() => {
    if (collapsed) return
    const onKey = (e: KeyboardEvent) => {
      // Don't hijack typing in inputs/textareas/contenteditable. The
      // filter builder, page-size selector, and search all live above
      // this table and the operator hits j/k constantly while typing.
      const ae = document.activeElement as HTMLElement | null
      if (ae) {
        const tag = ae.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
        if (ae.isContentEditable) return
      }
      // Bare j/k/Enter only — let modifiers (Cmd-Enter etc.) pass
      // through to the browser / other handlers.
      if (e.metaKey || e.ctrlKey || e.altKey) return
      if (flows.length === 0) return
      if (e.key === 'j') {
        e.preventDefault()
        setHighlight((h) => (h + 1) % flows.length)
      } else if (e.key === 'k') {
        e.preventDefault()
        setHighlight((h) => (h - 1 + flows.length) % flows.length)
      } else if (e.key === 'Enter') {
        const f = flows[highlight]
        if (!f) return
        e.preventDefault()
        onDrill({
          title: `${f.src_addr}:${f.src_port} → ${f.dst_addr}:${f.dst_port}`,
          subtitle: `5-tuple · ${fmt.proto(f.proto)}`,
          filters: [
            { key: 'src_addr', value: f.src_addr },
            { key: 'src_port', value: String(f.src_port) },
            { key: 'dst_addr', value: f.dst_addr },
            { key: 'dst_port', value: String(f.dst_port) },
            { key: 'proto', value: String(f.proto), label: fmt.proto(f.proto) },
          ],
        })
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [collapsed, flows, highlight, onDrill])
  useEffect(() => {
    const row = rowRefs.current[highlight]
    if (row) row.scrollIntoView({ block: 'nearest' })
  }, [highlight])

  return (
    <section className="border-t border-line">
      <div className="flex items-center gap-3 px-4 py-3 border-b border-line bg-surface">
        <button
          type="button"
          onClick={() => setCollapsed((c) => !c)}
          aria-expanded={!collapsed}
          aria-controls="investigate-body"
          className="flex items-baseline gap-2 text-[11px] uppercase tracking-[0.1em] text-dim font-semibold hover:text-text"
        >
          <span
            aria-hidden
            className={`inline-block text-faint text-[9px] transition-transform ${collapsed ? '' : 'rotate-90'}`}
          >
            ▶
          </span>
          <span>Investigate</span>
        </button>
        <span className="font-mono text-[11px] text-faint">
          paginated · sortable · filtered by chips above
        </span>
        {!collapsed && (
          <div className="ml-auto flex items-center gap-3">
            <Selector
              label="rows"
              value={String(pageSize)}
              onChange={(v) => setPageSize(Number(v))}
              options={PAGE_SIZE_OPTIONS.map((n) => ({
                value: String(n),
                label: String(n),
              }))}
            />
          </div>
        )}
      </div>
      {!collapsed && (
        <div id="investigate-body">
          {q.isLoading ? (
            <div className="px-4 py-6 text-faint font-mono text-[12px]">loading…</div>
          ) : q.error ? (
            <div className="px-4 py-6 text-crit font-mono text-[12px]">
              error · {(q.error as Error).message}
            </div>
          ) : flows.length === 0 ? (
            <div className="px-4 py-8 text-center text-[12px] font-mono text-dim">
              no flows match these filters in the window
            </div>
          ) : (
            <table className="w-full table-fixed">
              <colgroup>
                <col style={{ width: '110px' }} />
                <col style={{ width: '160px' }} />
                <col />
                <col style={{ width: '70px' }} />
                <col style={{ width: '80px' }} />
                <col style={{ width: '90px' }} />
                <col style={{ width: '100px' }} />
                <col style={{ width: '76px' }} />
              </colgroup>
              <thead>
                <tr>
                  <ServerTh
                    sortKey="observed"
                    active={sort}
                    dir={dir}
                    onToggle={cycleSort}
                  >
                    time
                  </ServerTh>
                  <th>exporter</th>
                  <th>src → dst</th>
                  <th>proto</th>
                  <th>service</th>
                  <ServerTh
                    sortKey="packets"
                    active={sort}
                    dir={dir}
                    onToggle={cycleSort}
                    align="r"
                  >
                    packets
                  </ServerTh>
                  <ServerTh
                    sortKey="bytes"
                    active={sort}
                    dir={dir}
                    onToggle={cycleSort}
                    align="r"
                  >
                    bytes
                  </ServerTh>
                  <th />
                </tr>
              </thead>
              <tbody>
                {flows.map((f, i) => (
                  <InvestigateRow
                    key={i}
                    f={f}
                    onAdd={onAdd}
                    onDrill={onDrill}
                    highlighted={i === highlight}
                    rowRef={(el) => {
                      rowRefs.current[i] = el
                    }}
                  />
                ))}
              </tbody>
            </table>
          )}
          <div className="flex items-center gap-3 px-4 py-2.5 border-t border-line bg-ink">
            <span className="font-mono text-[11px] text-dim tabular">
              {flows.length === 0
                ? '0 rows'
                : `showing ${fmt.num(showingFrom)}–${fmt.num(showingTo)}`}
            </span>
            <span className="ml-auto flex items-center gap-2">
              <PageBtn disabled={page === 0} onClick={() => setPage((p) => Math.max(0, p - 1))}>
                ← prev
              </PageBtn>
              <span className="font-mono text-[11px] text-faint tabular">
                page {page + 1}
              </span>
              <PageBtn disabled={!hasNext} onClick={() => setPage((p) => p + 1)}>
                next →
              </PageBtn>
            </span>
          </div>
        </div>
      )}
    </section>
  )
}

// InvestigateRow renders one row of the Investigate flow list.
// Lifted from Investigate.tbody.map so it can call useReverseDNS for
// src/dst (dedupes against the <Hostname> queries already firing in
// the same row) and useServiceName for the dst_port chip — populating
// the human label ("service · https") for the chip the trigger emits.
// Renders the full row markup unchanged from the previous inline JSX.
function InvestigateRow({
  f,
  onAdd,
  onDrill,
  highlighted,
  rowRef,
}: {
  f: RecentFlow
  onAdd: (filter: Filter) => void
  onDrill: (d: FlowDrillDown) => void
  highlighted: boolean
  rowRef: (el: HTMLTableRowElement | null) => void
}) {
  const srcRDNS = useReverseDNS(f.src_addr)?.hostname
  const dstRDNS = useReverseDNS(f.dst_addr)?.hostname
  const dstService = useServiceName(f.proto, f.dst_port)
  const dstPortLabel = dstService.data?.found ? dstService.data.primary.name : undefined
  return (
    <tr
      ref={rowRef}
      className={`hover:bg-surface group ${highlighted ? 'bg-accent-wash' : ''}`}
    >
      <td className="n text-faint">{fmt.time(f.observed).slice(11, 23)}</td>
      <td>
        <FilterTrigger
          k="exporter"
          value={f.exporter}
          onAdd={onAdd}
          label={f.exporter_name || f.exporter}
        >
          <div className="font-mono truncate">
            {f.exporter_name || f.exporter}
          </div>
        </FilterTrigger>
        {f.exporter_name && (
          <div className="font-mono italic text-faint text-[10.5px] truncate">
            {f.exporter}
          </div>
        )}
      </td>
      <td className="n truncate">
        <FilterTrigger k="src_addr" value={f.src_addr} onAdd={onAdd} label={srcRDNS || undefined}>
          {f.src_addr}
        </FilterTrigger>
        :
        <FilterTrigger
          k="src_port"
          value={String(f.src_port)}
          onAdd={onAdd}
        >
          {f.src_port}
        </FilterTrigger>
        <Hostname ip={f.src_addr} />{' '}
        <span className="text-faint">→</span>{' '}
        <FilterTrigger k="dst_addr" value={f.dst_addr} onAdd={onAdd} label={dstRDNS || undefined}>
          {f.dst_addr}
        </FilterTrigger>
        :
        <FilterTrigger
          k="dst_port"
          value={String(f.dst_port)}
          onAdd={onAdd}
          label={dstPortLabel}
          keyLabel={dstPortLabel ? 'service' : undefined}
        >
          {f.dst_port}
        </FilterTrigger>
        <Hostname ip={f.dst_addr} />
      </td>
      <td>
        <FilterTrigger
          k="proto"
          value={String(f.proto)}
          onAdd={onAdd}
          label={fmt.proto(f.proto)}
        >
          <span className="font-mono text-accent">{fmt.proto(f.proto)}</span>
        </FilterTrigger>
      </td>
      <td className="n text-dim">
        <ServiceLabel proto={f.proto} port={f.dst_port} fallback="—" />
      </td>
      <td className="r n">{fmt.num(f.packets)}</td>
      <td className="r n">{fmt.bytes(f.bytes)}</td>
      <td className="r">
        <button
          type="button"
          onClick={() =>
            onDrill({
              title: `${f.src_addr}:${f.src_port} → ${f.dst_addr}:${f.dst_port}`,
              subtitle: `5-tuple · ${fmt.proto(f.proto)}`,
              filters: [
                { key: 'src_addr', value: f.src_addr },
                { key: 'src_port', value: String(f.src_port) },
                { key: 'dst_addr', value: f.dst_addr },
                { key: 'dst_port', value: String(f.dst_port) },
                { key: 'proto', value: String(f.proto), label: fmt.proto(f.proto) },
              ],
            })
          }
          className="font-mono text-[10.5px] tracking-[0.06em] text-accent opacity-0 group-hover:opacity-100 hover:underline"
        >
          inspect →
        </button>
      </td>
    </tr>
  )
}

function ServerTh({
  sortKey,
  active,
  dir,
  onToggle,
  align,
  children,
}: {
  sortKey: FlowsListSort
  active: FlowsListSort
  dir: FlowsListDir
  onToggle: (k: FlowsListSort) => void
  align?: 'r'
  children: ReactNode
}) {
  const isActive = active === sortKey
  return (
    <th className={align === 'r' ? 'r' : undefined}>
      <button
        type="button"
        onClick={() => onToggle(sortKey)}
        className={`th-sort ${align === 'r' ? 'th-sort-r' : ''} ${isActive ? 'is-active' : ''}`}
      >
        <span>{children}</span>
        <span className="th-arrow" aria-hidden>
          {isActive ? (dir === 'asc' ? '▲' : '▼') : '↕'}
        </span>
      </button>
    </th>
  )
}

function PageBtn({
  disabled,
  onClick,
  children,
}: {
  disabled?: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className="font-mono text-[11px] px-2 py-1 border border-line text-text hover:border-accent disabled:text-faint disabled:hover:border-line disabled:cursor-not-allowed"
    >
      {children}
    </button>
  )
}
