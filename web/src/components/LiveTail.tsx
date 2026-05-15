import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { api, fmt } from '../api'
import type { RecentFlow } from '../api'
import type { Filter } from '../filters'
import { ServiceLabel, useServiceName } from './ServiceLabel'
import { Th, useTableSort, type SortColumns } from './sortable'
import { Hostname } from './Hostname'
import { FilterTrigger } from './FilterTrigger'

const FLOW_COLS: SortColumns<RecentFlow> = {
  observed: (r) => r.observed,
  exporter: (r) => r.exporter_name || r.exporter,
  source: (r) => r.source,
  src_dst: (r) => `${r.src_addr}:${r.src_port} ${r.dst_addr}:${r.dst_port}`,
  proto: (r) => r.proto,
  service: (r) => r.dst_port,
  packets: (r) => r.packets,
  bytes: (r) => r.bytes,
}

export function LiveTail({
  storageKey = 'flowscope.liveTail.collapsed',
  defaultCollapsed = false,
  onAdd,
}: {
  storageKey?: string
  defaultCollapsed?: boolean
  // onAdd, when supplied, makes the row values clickable: clicking
  // a value chips it into the parent's filter set. The Flows page
  // wires this to also switch the sub-tab to Investigate so the
  // operator lands directly on the filtered slice.
  onAdd?: (f: Filter) => void
} = {}) {
  const [collapsed, setCollapsed] = useState<boolean>(() => {
    try {
      const raw = localStorage.getItem(storageKey)
      if (raw === '1') return true
      if (raw === '0') return false
    } catch {
      // localStorage unavailable
    }
    return defaultCollapsed
  })
  useEffect(() => {
    try {
      localStorage.setItem(storageKey, collapsed ? '1' : '0')
    } catch {
      // ignore
    }
  }, [storageKey, collapsed])
  // Pause is in-memory only — when the operator switches Flows
  // sub-tabs the component unmounts and pause state is dropped,
  // which is intentional. Chip-and-investigate is forward motion;
  // coming back to Live tail should resume streaming.
  const [paused, setPaused] = useState(false)
  // Live tail must keep streaming regardless of the global time-range
  // mode. The QueryClient's default refetchInterval gates polling on
  // isLive() — perfect for window-scoped queries, exactly wrong for
  // this component. We override both refetchInterval and the
  // window-focus refetch so that:
  //   - 2s cadence regardless of isLive() (operator picked an
  //     absolute range? doesn't matter; live tail stays live)
  //   - Pause is honored on BOTH the timer AND on window focus — if
  //     the operator paused to read a row, alt-tabbing away and back
  //     won't snap fresh data in under them.
  // Also kept as a belt-and-suspenders: a setInterval that fires an
  // explicit invalidate. Either path alone would suffice; together
  // they survive timing edge cases at isLive() transitions.
  const qc = useQueryClient()
  const recent = useQuery({
    queryKey: ['recent', 20],
    queryFn: () => api.recentFlows(20),
    refetchOnMount: 'always',
    refetchOnWindowFocus: !paused,
    refetchInterval: collapsed || paused ? false : 2000,
    staleTime: 0,
  })
  useEffect(() => {
    if (collapsed || paused) return
    const id = window.setInterval(() => {
      qc.invalidateQueries({ queryKey: ['recent', 20] })
    }, 2000)
    return () => window.clearInterval(id)
  }, [collapsed, paused, qc])
  const flows = recent.data?.flows ?? []
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(flows, FLOW_COLS, {
    key: 'observed',
    dir: 'desc',
  })
  // Tiny pulse next to the title every time fresh data lands — gives
  // the operator a visible cue that the stream is moving even when
  // the rows themselves haven't changed in the last tick (e.g. quiet
  // window of synth traffic).
  const [pulse, setPulse] = useState(0)
  const lastUpdatedRef = useRef<string | undefined>(undefined)
  useEffect(() => {
    const ts = recent.dataUpdatedAt
    if (ts && String(ts) !== lastUpdatedRef.current) {
      lastUpdatedRef.current = String(ts)
      setPulse((n) => n + 1)
    }
  }, [recent.dataUpdatedAt])
  const thProps = (k: string) => ({
    sortKey: k,
    active: sortKey === k,
    dir: sortDir,
    onToggle: toggle,
  })
  return (
    <section>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <button
          type="button"
          onClick={() => setCollapsed((c) => !c)}
          aria-expanded={!collapsed}
          aria-controls="live-tail-body"
          className="flex items-baseline gap-2 text-[11px] uppercase tracking-[0.1em] text-dim font-semibold hover:text-text"
        >
          <span
            aria-hidden
            className={`inline-block text-faint text-[9px] transition-transform ${collapsed ? '' : 'rotate-90'}`}
          >
            ▶
          </span>
          <span>Live tail</span>
        </button>
        <span className="font-mono text-[11px] text-faint tabular">
          {recent.isLoading ? 'loading…' : `${flows.length} most recent`}
        </span>
        {!collapsed && !paused && (
          <span
            key={pulse}
            aria-hidden
            className="w-1.5 h-1.5 rounded-full bg-ok animate-pulse"
            title="streaming"
          />
        )}
        {!collapsed && (
          <button
            type="button"
            onClick={() => setPaused((p) => !p)}
            data-testid="live-tail-pause"
            aria-pressed={paused}
            className={`font-mono text-[11px] tracking-[0.06em] px-2 py-0.5 border ${
              paused
                ? 'border-warn text-warn hover:bg-warn/10'
                : 'border-line text-dim hover:text-text hover:border-accent'
            }`}
          >
            {paused ? '▶ Resume' : '⏸ Pause'}
          </button>
        )}
        <span className="ml-auto font-mono text-[10px] tracking-[0.06em]">
          {paused ? (
            <span className="text-warn">PAUSED · click Resume</span>
          ) : (
            <span className="text-ok">STREAMING · 2s</span>
          )}
        </span>
      </div>
      {!collapsed && (
        <div id="live-tail-body">
          {!recent.isLoading && flows.length === 0 ? (
            <div className="px-4 py-8 text-center text-[12px] font-mono text-dim border-b border-line">
              no flows yet · drive synth to populate
            </div>
          ) : (
            <>
              {onAdd && (
                <div className="px-4 py-1.5 border-b border-line-soft bg-surface font-mono text-[10.5px] text-faint italic">
                  click any value to filter & investigate
                </div>
              )}
              <table className="w-full table-fixed">
              <colgroup>
                <col style={{ width: '110px' }} />
                <col style={{ width: '160px' }} />
                <col style={{ width: '90px' }} />
                <col />
                <col style={{ width: '70px' }} />
                <col style={{ width: '70px' }} />
                <col style={{ width: '90px' }} />
                <col style={{ width: '90px' }} />
              </colgroup>
              <thead>
                <tr>
                  <Th {...thProps('observed')}>time</Th>
                  <Th {...thProps('exporter')}>exporter</Th>
                  <Th {...thProps('source')}>source</Th>
                  <Th {...thProps('src_dst')}>src → dst</Th>
                  <Th {...thProps('proto')}>proto</Th>
                  <Th {...thProps('service')}>service</Th>
                  <Th {...thProps('packets')} align="r">packets</Th>
                  <Th {...thProps('bytes')} align="r">bytes</Th>
                </tr>
              </thead>
              <tbody>
                {sortedRows.map((f, i) => (
                  <FlowRow key={i} f={f} onAdd={onAdd} />
                ))}
              </tbody>
            </table>
            </>
          )}
        </div>
      )}
    </section>
  )
}

// FlowRow renders a single Live tail row. Lifted into its own
// component so it can call useServiceName for the dst port chip
// label — the resolver dedupes against the Top services panel's
// existing queries so we don't double-fetch.
function FlowRow({ f, onAdd }: { f: RecentFlow; onAdd?: (f: Filter) => void }) {
  const svc = useServiceName(f.proto, f.dst_port)
  const svcLabel = svc.data?.found ? svc.data.primary.name : undefined
  if (!onAdd) {
    return (
      <tr className="hover:bg-surface">
        <td className="n text-faint">{fmt.time(f.observed).slice(11, 23)}</td>
        <td>
          <div className="font-mono truncate">{f.exporter_name || f.exporter}</div>
          {f.exporter_name && (
            <div className="font-mono italic text-faint text-[10.5px] truncate">
              {f.exporter}
            </div>
          )}
        </td>
        <td className="n text-dim">{f.source}</td>
        <td className="n truncate">
          {f.src_addr}:{f.src_port}
          <Hostname ip={f.src_addr} />{' '}
          <span className="text-faint">→</span>{' '}
          {f.dst_addr}:{f.dst_port}
          <Hostname ip={f.dst_addr} />
        </td>
        <td>
          <span className="font-mono text-accent">{fmt.proto(f.proto)}</span>
        </td>
        <td className="n text-dim">
          <ServiceLabel proto={f.proto} port={f.dst_port} fallback="—" />
        </td>
        <td className="r n">{fmt.num(f.packets)}</td>
        <td className="r n">{fmt.bytes(f.bytes)}</td>
      </tr>
    )
  }
  return (
    <tr className="hover:bg-surface">
      <td className="n text-faint">{fmt.time(f.observed).slice(11, 23)}</td>
      <td>
        <FilterTrigger
          k="exporter"
          value={f.exporter}
          onAdd={onAdd}
          label={f.exporter_name || undefined}
          block
        >
          <div className="font-mono truncate text-text">
            {f.exporter_name || f.exporter}
          </div>
          {f.exporter_name && (
            <div className="font-mono italic text-faint text-[10.5px] truncate">
              {f.exporter}
            </div>
          )}
        </FilterTrigger>
      </td>
      <td className="n text-dim">{f.source}</td>
      <td className="n truncate">
        <FilterTrigger k="src_addr" value={f.src_addr} onAdd={onAdd}>
          {f.src_addr}
        </FilterTrigger>
        :
        <FilterTrigger k="src_port" value={String(f.src_port)} onAdd={onAdd}>
          {f.src_port}
        </FilterTrigger>
        <Hostname ip={f.src_addr} />{' '}
        <span className="text-faint">→</span>{' '}
        <FilterTrigger k="dst_addr" value={f.dst_addr} onAdd={onAdd}>
          {f.dst_addr}
        </FilterTrigger>
        :
        <FilterTrigger k="dst_port" value={String(f.dst_port)} onAdd={onAdd}>
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
        <FilterTrigger
          k="dst_port"
          value={String(f.dst_port)}
          onAdd={onAdd}
          label={svcLabel}
          keyLabel="service"
        >
          <ServiceLabel proto={f.proto} port={f.dst_port} fallback="—" />
        </FilterTrigger>
      </td>
      <td className="r n">{fmt.num(f.packets)}</td>
      <td className="r n">{fmt.bytes(f.bytes)}</td>
    </tr>
  )
}
