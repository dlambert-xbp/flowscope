import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { api, fmt } from '../api'
import type {
  TopTalker,
  TopService,
  TopProtocol,
  TopConversation,
} from '../api'

// Flows tab — top-N panels over the trailing 5 minutes. Filter chips
// are stubbed (the chip strip renders but doesn't yet compose into
// queries) — composable filters are a follow-up slice.
export function Flows() {
  return (
    <div>
      <Filters />
      <div className="grid grid-cols-1 lg:grid-cols-2 border-b border-line">
        <Panel title="Top talkers" sub="src → dst · by bytes" right="SOURCE · FLOWS">
          <TalkersList />
        </Panel>
        <Panel title="Top services" sub="dst port · by bytes" right="SOURCE · FLOWS">
          <ServicesList />
        </Panel>
        <Panel title="Top protocols" sub="share of total" right="SOURCE · FLOWS">
          <ProtocolsList />
        </Panel>
        <Panel title="Top conversations" sub="5-tuple · by bytes" right="SOURCE · FLOWS">
          <ConversationsList />
        </Panel>
      </div>
    </div>
  )
}

/* ----------------------------- Chrome ----------------------------- */

function Filters() {
  return (
    <div className="flex items-center gap-3 px-4 py-3 border-b border-line bg-surface">
      <span className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-semibold">
        Filters
      </span>
      <Chip>window · 5 min</Chip>
      <Chip>—</Chip>
      <span className="font-mono text-[11px] text-faint italic">
        composable filter chips arrive in a follow-up slice
      </span>
    </div>
  )
}

function Chip({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-[11px] px-2 py-1 border border-line text-dim hover:text-text">
      {children}
    </span>
  )
}

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

/* ----------------------------- Bar list ----------------------------- */

function Bar({
  pct,
  className = 'bg-accent',
}: {
  pct: number
  className?: string
}) {
  return (
    <div className="h-px bg-line w-full overflow-hidden">
      <div
        className={`h-full ${className}`}
        style={{ width: `${Math.min(100, Math.max(0, pct))}%` }}
      />
    </div>
  )
}

/* ----------------------------- Top talkers ----------------------------- */

function TalkersList() {
  const q = useQuery({
    queryKey: ['top-talkers'],
    queryFn: () => api.topTalkers(300, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
        keyOf={(r) => `${r.src_addr}>${r.dst_addr}`}
        renderLeft={(r: TopTalker) => (
          <span className="font-mono text-[12px]">
            {r.src_addr} <span className="text-faint">→</span> {r.dst_addr}
          </span>
        )}
        renderRight={(r: TopTalker) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

/* ----------------------------- Top services ----------------------------- */

function ServicesList() {
  const q = useQuery({
    queryKey: ['top-services'],
    queryFn: () => api.topServices(300, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
        keyOf={(r) => `${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopService) => (
          <span className="font-mono text-[12px]">
            <span className="text-text">{serviceFor(r.dst_port) ?? `port ${r.dst_port}`}</span>{' '}
            <span className="text-faint">·</span>{' '}
            <span className="text-faint">{fmt.proto(r.proto)} {r.dst_port}</span>
          </span>
        )}
        renderRight={(r: TopService) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

/* ----------------------------- Top protocols ----------------------------- */

function ProtocolsList() {
  const q = useQuery({
    queryKey: ['top-protocols'],
    queryFn: () => api.topProtocols(300),
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
            <span className="text-text">{fmt.proto(r.proto)}</span>{' '}
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

/* ----------------------------- Top conversations ----------------------------- */

function ConversationsList() {
  const q = useQuery({
    queryKey: ['top-conversations'],
    queryFn: () => api.topConversations(300, 12),
  })
  return (
    <ListShell loading={q.isLoading} empty={!q.data?.rows.length} error={q.error as Error | undefined}>
      <Rows
        rows={q.data?.rows ?? []}
        total={q.data?.rows.reduce((a, r) => a + r.bytes, 0) ?? 0}
        keyOf={(r) => `${r.src_addr}_${r.src_port}_${r.dst_addr}_${r.dst_port}_${r.proto}`}
        renderLeft={(r: TopConversation) => (
          <span className="font-mono text-[12px] text-text">
            {r.src_addr}:{r.src_port}{' '}
            <span className="text-faint">→</span> {r.dst_addr}:{r.dst_port}{' '}
            <span className="text-faint">· {fmt.proto(r.proto)}</span>
          </span>
        )}
        renderRight={(r: TopConversation) => fmt.bytes(r.bytes)}
        valueOf={(r) => r.bytes}
      />
    </ListShell>
  )
}

/* ----------------------------- Generic shell + rows ----------------------------- */

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
            <div className="mt-1.5">
              <Bar pct={pct} />
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
