import { useQuery } from '@tanstack/react-query'
import { api, fmt } from '../api'
import type { RecentFlow } from '../api'
import { ServiceLabel } from './ServiceLabel'
import { Th, useTableSort, type SortColumns } from './sortable'

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

export function LiveTail() {
  const recent = useQuery({
    queryKey: ['recent'],
    queryFn: () => api.recentFlows(20),
    refetchInterval: 2000,
  })
  const flows = recent.data?.flows ?? []
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(flows, FLOW_COLS, {
    key: 'observed',
    dir: 'desc',
  })
  const thProps = (k: string) => ({
    sortKey: k,
    active: sortKey === k,
    dir: sortDir,
    onToggle: toggle,
  })
  return (
    <section>
      <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          Live tail
        </span>
        <span className="font-mono text-[11px] text-faint tabular">
          {recent.isLoading ? 'loading…' : `${flows.length} most recent`}
        </span>
        <span className="ml-auto font-mono text-[10px] tracking-[0.06em] text-faint">
          REFRESH · 2s
        </span>
      </div>
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
                  {f.src_addr}:{f.src_port} <span className="text-faint">→</span>{' '}
                  {f.dst_addr}:{f.dst_port}
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
    </section>
  )
}
