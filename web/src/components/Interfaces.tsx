import { Fragment, useState, type ReactNode } from 'react'
import { fmt, labelExporter, labelInterface } from '../api'
import type { InterfaceRow } from '../api'
import { InterfaceChart } from './InterfaceChart'

function TwoLine({
  primary,
  secondary,
}: {
  primary: ReactNode
  secondary?: ReactNode
}) {
  return (
    <div className="min-w-0 max-w-full">
      <div className="font-mono truncate">{primary}</div>
      {secondary && (
        <div className="font-mono italic text-faint text-[10.5px] truncate">
          {secondary}
        </div>
      )}
    </div>
  )
}

export function Interfaces({
  rows,
  loading,
}: {
  rows: InterfaceRow[]
  loading: boolean
}) {
  const [active, setActive] = useState<{ exporter: string; ifindex: number } | null>(null)

  return (
    <section>
      <SectionHead
        title="Top interfaces"
        sub={loading ? 'loading…' : `${rows.length} seen · last 5 min`}
        right={<SourceBadge>SOURCE · COUNTERS</SourceBadge>}
      />
      {!loading && rows.length === 0 && (
        <div className="px-4 py-8 text-center text-[12px] font-mono text-dim border-b border-line">
          no counter samples yet · sFlow exporters or synth-sFlow needed
        </div>
      )}
      {rows.length > 0 && (
        <table className="w-full table-fixed">
          <colgroup>
            <col style={{ width: '24%' }} />
            <col style={{ width: '24%' }} />
            <col />
            <col />
            <col />
            <col />
            <col style={{ width: '92px' }} />
            <col style={{ width: '80px' }} />
          </colgroup>
          <thead>
            <tr>
              <th>exporter</th>
              <th>interface</th>
              <th className="r">in latest</th>
              <th className="r">out latest</th>
              <th className="r">in peak</th>
              <th className="r">out peak</th>
              <th className="r">last seen</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => {
              const isActive = active?.exporter === r.exporter && active?.ifindex === r.ifindex
              const eLbl = labelExporter(r)
              const iLbl = labelInterface(r)
              return (
                <Fragment key={`${r.exporter}_${r.ifindex}`}>
                  <tr className="hover:bg-surface">
                    <td>
                      <TwoLine primary={eLbl.primary} secondary={eLbl.secondary || undefined} />
                    </td>
                    <td>
                      <TwoLine primary={iLbl.primary} secondary={iLbl.secondary || undefined} />
                    </td>
                    <td className="r n">{fmt.bps(r.in_bps_latest)}</td>
                    <td className="r n">{fmt.bps(r.out_bps_latest)}</td>
                    <td className="r n text-accent">{fmt.bps(r.in_bps_peak)}</td>
                    <td className="r n text-ok">{fmt.bps(r.out_bps_peak)}</td>
                    <td className="r n text-faint">{fmt.time(r.last_seen).slice(11, 19)}</td>
                    <td className="r">
                      <button
                        className={`text-[11px] font-mono ${isActive ? 'text-text' : 'text-accent hover:underline'}`}
                        onClick={() =>
                          setActive(
                            isActive ? null : { exporter: r.exporter, ifindex: r.ifindex },
                          )
                        }
                      >
                        {isActive ? '× close' : 'chart →'}
                      </button>
                    </td>
                  </tr>
                  {isActive && (
                    <tr>
                      <td colSpan={8} style={{ padding: 0, borderBottom: 'none' }}>
                        <InterfaceChart exporter={r.exporter} ifindex={r.ifindex} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      )}
    </section>
  )
}

function SectionHead({
  title,
  sub,
  right,
}: {
  title: string
  sub?: string
  right?: ReactNode
}) {
  return (
    <div className="flex items-baseline gap-3 px-4 py-3 border-b border-line">
      <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">{title}</span>
      {sub && <span className="font-mono text-[11px] text-faint tabular">{sub}</span>}
      {right && <span className="ml-auto">{right}</span>}
    </div>
  )
}

function SourceBadge({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-[10px] tracking-[0.06em] text-accent">{children}</span>
  )
}

