import { useQuery } from '@tanstack/react-query'
import { api, fmt } from '../api'
import { ServiceLabel } from './ServiceLabel'

export function LiveTail() {
  const recent = useQuery({
    queryKey: ['recent'],
    queryFn: () => api.recentFlows(20),
    refetchInterval: 2000,
  })
  const flows = recent.data?.flows ?? []
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
              <th>time</th>
              <th>exporter</th>
              <th>source</th>
              <th>src → dst</th>
              <th>proto</th>
              <th>service</th>
              <th className="r">packets</th>
              <th className="r">bytes</th>
            </tr>
          </thead>
          <tbody>
            {flows.map((f, i) => (
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

