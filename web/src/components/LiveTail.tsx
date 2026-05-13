import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { api, fmt } from '../api'
import type { RecentFlow } from '../api'
import { ServiceLabel } from './ServiceLabel'
import { Th, useTableSort, type SortColumns } from './sortable'
import { Hostname } from './Hostname'

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
}: {
  storageKey?: string
  defaultCollapsed?: boolean
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
  // Live tail must keep streaming regardless of the global time-range
  // mode. The QueryClient's default refetchInterval gates polling on
  // isLive() — perfect for window-scoped queries, exactly wrong for
  // this component. Per-query option merging proved unreliable when
  // the operator was on an absolute range, so we drive the refetch
  // explicitly via a setInterval + invalidateQueries. Belt-and-
  // suspenders, but it gives the operator the guarantee that "Live
  // tail" actually means live no matter what.
  const qc = useQueryClient()
  const recent = useQuery({
    queryKey: ['recent', 20],
    queryFn: () => api.recentFlows(20),
    refetchOnMount: 'always',
    refetchOnWindowFocus: true,
    staleTime: 0,
  })
  useEffect(() => {
    if (collapsed) return
    const id = window.setInterval(() => {
      qc.invalidateQueries({ queryKey: ['recent', 20] })
    }, 2000)
    return () => window.clearInterval(id)
  }, [collapsed, qc])
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
        <span
          key={pulse}
          aria-hidden
          className="w-1.5 h-1.5 rounded-full bg-ok animate-pulse"
          title="streaming"
        />
        <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-ok">
          STREAMING · 2s
        </span>
      </div>
      {!collapsed && (
        <div id="live-tail-body">
          {!recent.isLoading && flows.length === 0 ? (
            <div className="px-4 py-8 text-center text-[12px] font-mono text-dim border-b border-line">
              no flows yet · drive synth to populate
            </div>
          ) : (
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
                  <tr key={i} className="hover:bg-surface">
                    <td className="n text-faint">{fmt.time(f.observed).slice(11, 23)}</td>
                    <td>
                      <div className="font-mono truncate">
                        {f.exporter_name || f.exporter}
                      </div>
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
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </section>
  )
}
