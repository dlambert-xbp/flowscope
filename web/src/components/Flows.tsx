import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { api, fmt } from '../api'
import type {
  TopTalker,
  TopService,
  TopProtocol,
  TopConversation,
  TimeRangeArg,
} from '../api'
import { useFilters, toQuery, keyLabelFor, type Filter, type FilterKey } from '../filters'
import {
  rangeLabel,
  toApi,
  useTimeRange,
  type TimeRange,
} from '../timeRange'
import { TimeRangeSelector } from './TimeRangeSelector'

// Flows tab — top-N panels narrowed by a composable filter set. Click
// any value (talker src, talker dst, service port, protocol, full
// 5-tuple) to add or replace a filter chip; chips re-narrow every
// panel's query and persist in the URL.
export function Flows() {
  const f = useFilters()
  const tr = useTimeRange('fl')
  const qs = toQuery(f.filters)
  const apiRange = toApi(tr.range)
  return (
    <div>
      <FilterBar
        filters={f.filters}
        onRemove={f.remove}
        onClear={f.clear}
        range={tr.range}
        onRangeChange={tr.set}
      />
      <div className="grid grid-cols-1 lg:grid-cols-2 border-b border-line">
        <Panel title="Top talkers" sub="src → dst · by bytes" right="SOURCE · FLOWS">
          <TalkersList qs={qs} onAdd={f.add} range={apiRange} rangeKey={tr.queryKey} />
        </Panel>
        <Panel title="Top services" sub="dst port · by bytes" right="SOURCE · FLOWS">
          <ServicesList qs={qs} onAdd={f.add} range={apiRange} rangeKey={tr.queryKey} />
        </Panel>
        <Panel title="Top protocols" sub="share of total" right="SOURCE · FLOWS">
          <ProtocolsList qs={qs} onAdd={f.add} range={apiRange} rangeKey={tr.queryKey} />
        </Panel>
        <Panel title="Top conversations" sub="5-tuple · by bytes" right="SOURCE · FLOWS">
          <ConversationsList qs={qs} onAdd={f.add} range={apiRange} rangeKey={tr.queryKey} />
        </Panel>
      </div>
    </div>
  )
}

/* ----------------------------- Filter bar ----------------------------- */

function FilterBar({
  filters,
  onRemove,
  onClear,
  range,
  onRangeChange,
}: {
  filters: Filter[]
  onRemove: (key: FilterKey, value?: string) => void
  onClear: () => void
  range: TimeRange
  onRangeChange: (r: TimeRange) => void
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
      <span className="ml-auto flex items-center gap-3">
        {has ? (
          <button
            className="font-mono text-[11px] text-dim hover:text-text px-2 py-1 border border-line"
            onClick={onClear}
          >
            clear all
          </button>
        ) : (
          <span className="font-mono text-[11px] text-faint italic">
            click any row to add a filter
          </span>
        )}
        <TimeRangeSelector range={range} onChange={onRangeChange} />
      </span>
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
    <section className="border-r border-line border-b last:border-r-0 lg:[&:nth-child(2n)]:border-r-0">
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

function TalkersList({
  qs,
  onAdd,
  range,
  rangeKey,
}: {
  qs: URLSearchParams
  onAdd: (f: Filter) => void
  range: TimeRangeArg
  rangeKey: unknown
}) {
  const q = useQuery({
    queryKey: ['top-talkers', qs.toString(), rangeKey],
    queryFn: () => api.topTalkers(qs, range, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
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
        renderRight={(r: TopTalker) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

function ServicesList({
  qs,
  onAdd,
  range,
  rangeKey,
}: {
  qs: URLSearchParams
  onAdd: (f: Filter) => void
  range: TimeRangeArg
  rangeKey: unknown
}) {
  const q = useQuery({
    queryKey: ['top-services', qs.toString(), rangeKey],
    queryFn: () => api.topServices(qs, range, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
        keyOf={(r) => `${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopService) => (
          <span className="font-mono text-[12px]">
            <FilterTrigger
              k="dst_port"
              value={String(r.dst_port)}
              onAdd={onAdd}
              label={`${serviceFor(r.dst_port) ?? `port ${r.dst_port}`}`}
              keyLabel="service"
            >
              <span className="text-text">{serviceFor(r.dst_port) ?? `port ${r.dst_port}`}</span>
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
        )}
        renderRight={(r: TopService) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

function ProtocolsList({
  qs,
  onAdd,
  range,
  rangeKey,
}: {
  qs: URLSearchParams
  onAdd: (f: Filter) => void
  range: TimeRangeArg
  rangeKey: unknown
}) {
  const q = useQuery({
    queryKey: ['top-protocols', qs.toString(), rangeKey],
    queryFn: () => api.topProtocols(qs, range),
  })
  const total = q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={total}
        keyOf={(r) => String(r.proto)}
        renderLeft={(r: TopProtocol) => (
          <span className="font-mono text-[12px]">
            <FilterTrigger k="proto" value={String(r.proto)} onAdd={onAdd} label={fmt.proto(r.proto)}>
              <span className="text-text">{fmt.proto(r.proto)}</span>
            </FilterTrigger>{' '}
            <span className="text-faint">· {r.proto}</span>
          </span>
        )}
        renderRight={(r: TopProtocol) =>
          total > 0 ? `${((r.bytes / total) * 100).toFixed(1)}%` : '—'
        }
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

function ConversationsList({
  qs,
  onAdd,
  range,
  rangeKey,
}: {
  qs: URLSearchParams
  onAdd: (f: Filter) => void
  range: TimeRangeArg
  rangeKey: unknown
}) {
  const q = useQuery({
    queryKey: ['top-conversations', qs.toString(), rangeKey],
    queryFn: () => api.topConversations(qs, range, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
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
        renderRight={(r: TopConversation) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
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
  total,
  keyOf,
  renderLeft,
  renderRight,
  valueOf,
}: {
  rows: T[]
  total: number
  keyOf: (r: T) => string
  renderLeft: (r: T) => ReactNode
  renderRight: (r: T) => ReactNode
  valueOf: (r: T) => number
}) {
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

/* ----------------------------- service map ----------------------------- */

function serviceFor(port: number): string | undefined {
  return (
    {
      22: 'ssh',
      53: 'dns',
      80: 'http',
      443: 'https',
      445: 'smb',
      3389: 'rdp',
      161: 'snmp',
      162: 'snmp-trap',
      2055: 'netflow',
      6343: 'sflow',
      25: 'smtp',
      587: 'submission',
      993: 'imaps',
      995: 'pop3s',
      123: 'ntp',
      389: 'ldap',
      636: 'ldaps',
      8080: 'http-alt',
    } as Record<number, string>
  )[port]
}
