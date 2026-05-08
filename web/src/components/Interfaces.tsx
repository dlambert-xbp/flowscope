import { Fragment, useState, type ReactNode } from 'react'
import { fmt, labelExporter, labelInterface } from '../api'
import type { InterfaceRow } from '../api'
import { InterfaceChart } from './InterfaceChart'
import { rangeLabel, type TimeRange, DEFAULT_TIME_RANGE } from '../timeRange'
import { Th, useTableSort, type SortColumns } from './sortable'

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

const IFACE_COLS: SortColumns<InterfaceRow> = {
  exporter: (r) => labelExporter(r).primary,
  interface: (r) => labelInterface(r).primary,
  in_latest: (r) => r.in_bps_latest,
  out_latest: (r) => r.out_bps_latest,
  in_peak: (r) => r.in_bps_peak,
  out_peak: (r) => r.out_bps_peak,
  last_seen: (r) => r.last_seen,
}

export function Interfaces({
  rows,
  loading,
  range = DEFAULT_TIME_RANGE,
}: {
  rows: InterfaceRow[]
  loading: boolean
  range?: TimeRange
}) {
  const [active, setActive] = useState<{ exporter: string; ifindex: number } | null>(null)
  const winLabel = rangeLabel(range)
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(rows, IFACE_COLS, {
    key: 'in_latest',
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
      <SectionHead
        title="Top interfaces"
        sub={loading ? 'loading…' : `${rows.length} seen · last ${winLabel}`}
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
              <Th {...thProps('exporter')}>exporter</Th>
              <Th {...thProps('interface')}>interface</Th>
              <Th {...thProps('in_latest')} align="r">in latest</Th>
              <Th {...thProps('out_latest')} align="r">out latest</Th>
              <Th {...thProps('in_peak')} align="r">in peak</Th>
              <Th {...thProps('out_peak')} align="r">out peak</Th>
              <Th {...thProps('last_seen')} align="r">last seen</Th>
              <th />
            </tr>
          </thead>
          <tbody>
            {sortedRows.map((r) => {
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
                        <InterfaceChart exporter={r.exporter} ifindex={r.ifindex} range={range} />
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
