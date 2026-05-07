import { useQuery } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { api, fmt } from '../api'
import type { InterfaceRow, InterfaceTimeseriesPoint } from '../api'

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
        <table className="w-full">
          <thead>
            <tr>
              <th>exporter</th>
              <th className="r">ifindex</th>
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
              return (
                <tr key={`${r.exporter}_${r.ifindex}`} className="hover:bg-surface">
                  <td className="n">{r.exporter}</td>
                  <td className="r n">{r.ifindex}</td>
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
              )
            })}
          </tbody>
        </table>
      )}
      {active && <InterfaceChart exporter={active.exporter} ifindex={active.ifindex} />}
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

function InterfaceChart({ exporter, ifindex }: { exporter: string; ifindex: number }) {
  const ts = useQuery({
    queryKey: ['iface_ts', exporter, ifindex],
    queryFn: () => api.interfaceTimeseries(exporter, ifindex, 300),
    refetchInterval: 5000,
  })
  const points = ts.data?.points ?? []
  return (
    <div className="border-b border-line">
      <SectionHead
        title={`${exporter} · ifindex ${ifindex}`}
        sub="counter timeseries · 5 min"
        right={<SourceBadge>SOURCE · COUNTERS · 1s SAMPLE</SourceBadge>}
      />
      <div className="px-4 py-3 bg-surface">
        <ChartSVG points={points} loading={ts.isLoading} error={ts.error as Error | undefined} />
        <Legend points={points} />
      </div>
    </div>
  )
}

function ChartSVG({
  points,
  loading,
  error,
}: {
  points: InterfaceTimeseriesPoint[]
  loading: boolean
  error?: Error
}) {
  const W = 800
  const H = 200
  const pad = 8

  if (loading) {
    return (
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="w-full h-[200px] block bg-ink border border-line">
        <text x="10" y="20" fill="#5a5b5e" fontFamily="IBM Plex Mono" fontSize="10">loading…</text>
      </svg>
    )
  }
  if (error) {
    return (
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="w-full h-[200px] block bg-ink border border-line">
        <text x="10" y="20" fill="#e04646" fontFamily="IBM Plex Mono" fontSize="10">timeseries: {error.message}</text>
      </svg>
    )
  }
  if (points.length === 0) {
    return (
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="w-full h-[200px] block bg-ink border border-line">
        <text x="10" y="20" fill="#8b8a85" fontFamily="IBM Plex Mono" fontSize="10">
          no points yet · waiting for the next sample interval
        </text>
      </svg>
    )
  }

  const xs = points.map((p) => new Date(p.ts).getTime())
  const xMin = xs[0]
  const xMax = xs[xs.length - 1] || xMin + 1
  const yMax = Math.max(1, ...points.map((p) => Math.max(p.in_bps, p.out_bps)))
  const sx = (x: number) => pad + ((x - xMin) / (xMax - xMin || 1)) * (W - 2 * pad)
  const sy = (y: number) => H - pad - (y / yMax) * (H - 2 * pad)

  const inPath = points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${sx(xs[i]).toFixed(1)},${sy(p.in_bps).toFixed(1)}`)
    .join('')
  const outPath = points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${sx(xs[i]).toFixed(1)},${sy(p.out_bps).toFixed(1)}`)
    .join('')

  const grid = [1, 2, 3].map((i) => (
    <line key={i} x1="0" x2={W} y1={(H / 4) * i} y2={(H / 4) * i} stroke="#1c1e22" strokeDasharray="2 4" />
  ))

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      className="w-full h-[200px] block bg-ink border border-line"
    >
      {grid}
      <path d={inPath} fill="none" stroke="#ff5b1f" strokeWidth="1.5" />
      <path d={outPath} fill="none" stroke="#5a9c5f" strokeWidth="1.5" />
    </svg>
  )
}

function Legend({ points }: { points: InterfaceTimeseriesPoint[] }) {
  const last = points[points.length - 1]
  return (
    <div className="mt-2 flex gap-5 font-mono text-[11px] text-dim tabular">
      <span className="flex items-center gap-2">
        <span className="inline-block w-3 h-0.5 bg-accent" />
        in · <strong className="text-text">{last ? fmt.bps(last.in_bps) : '—'}</strong>
      </span>
      <span className="flex items-center gap-2">
        <span className="inline-block w-3 h-0.5 bg-ok" />
        out · <strong className="text-text">{last ? fmt.bps(last.out_bps) : '—'}</strong>
      </span>
    </div>
  )
}
