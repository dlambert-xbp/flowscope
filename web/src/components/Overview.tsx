import { useQuery } from '@tanstack/react-query'
import { api, fmt } from '../api'

// Overview consumes /api/summary and /api/interfaces and renders the
// Tile #1–#3 of VISION.md §5.1 plus the per-interface bandwidth table
// with a click-to-expand timeseries chart from slice 7. The full six
// tiles + Flows + Devices + Alerts arrive in follow-up slices as the
// other api endpoints land.
export function Overview() {
  const summary = useQuery({
    queryKey: ['summary'],
    queryFn: () => api.summary(300),
  })
  const ifaces = useQuery({
    queryKey: ['interfaces'],
    queryFn: () => api.interfaces(300),
    refetchInterval: 5000,
  })

  return (
    <div className="space-y-7">
      <Eyebrow />
      <Standfirst summary={summary.data} loading={summary.isLoading} error={summary.error as Error | undefined} />
      <Kpis summary={summary.data} />
      <Interfaces rows={ifaces.data?.interfaces ?? []} loading={ifaces.isLoading} />
    </div>
  )
}

function Eyebrow() {
  const ts = new Date().toUTCString().replace('GMT', 'UTC')
  return (
    <div className="flex items-center gap-3 text-[10px] uppercase tracking-[0.22em] font-bold text-accent">
      <span>Overview</span>
      <span className="flex-1 h-px bg-line" />
      <span className="text-dim font-medium tracking-[0.16em]">{ts}</span>
    </div>
  )
}

function Standfirst({
  summary,
  loading,
  error,
}: {
  summary?: import('../api').Summary
  loading: boolean
  error?: Error
}) {
  if (loading) return <p className="text-[15px] text-dim">Connecting…</p>
  if (error) {
    return (
      <p className="text-[15px] border-l-2 border-crit pl-4 py-1 text-text">
        <span className="text-crit font-semibold">Connection error.</span>{' '}
        <span className="text-dim">{error.message}</span>
      </p>
    )
  }
  if (!summary || summary.flows === 0) {
    return (
      <p className="text-[15px] border-l-2 border-accent pl-4 py-1 text-dim leading-relaxed">
        Connected to ClickHouse but the <em className="not-italic font-semibold text-text">flows</em> table is empty.{' '}
        Drive synthetic traffic with{' '}
        <code className="bg-raise px-1.5 py-0.5 text-[12.5px] font-mono">go run ./cmd/synth -- --target localhost:2055 --rate 5000</code>
        .
      </p>
    )
  }
  return (
    <p className="text-[15px] border-l-2 border-accent pl-4 py-1 text-dim leading-relaxed">
      Trailing 5 min: <em className="not-italic font-semibold text-text tabular">{fmt.num(summary.flows)}</em> flows from{' '}
      <em className="not-italic font-semibold text-text tabular">{summary.exporters}</em> exporters carrying{' '}
      <em className="not-italic font-semibold text-text">{fmt.bytes(summary.bytes)}</em> across{' '}
      <em className="not-italic font-semibold text-text tabular">{fmt.num(summary.packets)}</em> packets.
    </p>
  )
}

function Kpis({ summary }: { summary?: import('../api').Summary }) {
  const tiles = [
    { k: 'flows · 5m', v: summary ? fmt.num(summary.flows) : '—', accent: true },
    { k: 'bytes · 5m', v: summary ? fmt.bytes(summary.bytes) : '—' },
    { k: 'packets · 5m', v: summary ? fmt.num(summary.packets) : '—' },
    { k: 'exporters seen', v: summary ? fmt.num(summary.exporters) : '—' },
  ]
  return (
    <div className="grid grid-cols-2 lg:grid-cols-4 border border-line">
      {tiles.map((t, i) => (
        <div
          key={i}
          className="p-5 border-r border-b border-line last:border-r-0 lg:[&:nth-child(4n)]:border-r-0 bg-surf"
        >
          <div className="text-[10.5px] font-mono font-semibold uppercase tracking-[0.16em] text-dim mb-2">
            {t.k}
          </div>
          <div className={`text-[28px] font-semibold tracking-tight tabular ${t.accent ? 'text-accent' : 'text-text'}`}>
            {t.v}
          </div>
        </div>
      ))}
    </div>
  )
}

function Interfaces({
  rows,
  loading,
}: {
  rows: import('../api').InterfaceRow[]
  loading: boolean
}) {
  return (
    <section>
      <h2 className="flex items-baseline justify-between text-[10.5px] font-mono font-bold uppercase tracking-[0.18em] text-dim border-b border-line pb-2 mb-3">
        <span>Top interfaces</span>
        <span className="text-accent text-[9.5px] tracking-[0.06em]">SOURCE · COUNTERS</span>
      </h2>
      {loading ? (
        <p className="text-dim text-sm font-mono">loading…</p>
      ) : rows.length === 0 ? (
        <div className="border border-dashed border-line py-6 text-center text-[12px] font-mono text-dim">
          no counter samples yet — sFlow exporters or synth-sFlow needed
        </div>
      ) : (
        <table className="w-full text-[12.5px]">
          <thead>
            <tr className="text-[10.5px] uppercase tracking-[0.12em] text-faint font-mono font-bold">
              <th className="text-left py-2 pr-4 border-b border-line">exporter</th>
              <th className="text-right py-2 pr-4 border-b border-line">ifindex</th>
              <th className="text-right py-2 pr-4 border-b border-line">in latest</th>
              <th className="text-right py-2 pr-4 border-b border-line">out latest</th>
              <th className="text-right py-2 pr-4 border-b border-line">in peak</th>
              <th className="text-right py-2 pr-4 border-b border-line">out peak</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={`${r.exporter}_${r.ifindex}`} className="hover:bg-hover">
                <td className="py-2 pr-4 border-b border-line font-mono">{r.exporter}</td>
                <td className="py-2 pr-4 border-b border-line text-right font-mono tabular">{r.ifindex}</td>
                <td className="py-2 pr-4 border-b border-line text-right font-mono tabular">{fmt.bps(r.in_bps_latest)}</td>
                <td className="py-2 pr-4 border-b border-line text-right font-mono tabular">{fmt.bps(r.out_bps_latest)}</td>
                <td className="py-2 pr-4 border-b border-line text-right font-mono tabular text-accent">{fmt.bps(r.in_bps_peak)}</td>
                <td className="py-2 pr-4 border-b border-line text-right font-mono tabular text-ok">{fmt.bps(r.out_bps_peak)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
