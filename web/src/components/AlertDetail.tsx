import { useQuery } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { api, fmt } from '../api'
import type { Alert, AlertEvent, RecentFlow } from '../api'

// AlertDetail is the click-through modal for a single alert. The
// Alerts list opens this with the row's summary data already in hand
// — that data renders immediately so the modal feels responsive
// (render-on-state-change rule). The /api/alerts/{id} fetch follows
// and populates the timeline + linked-flows sections in place.
//
// Props:
//   open    — controls visibility; the parent flips this synchronously
//             on click so the modal appears in the same paint as the
//             click event, before the fetch returns.
//   alert   — the row's summary, already known by the caller. Used
//             as the initial header content; replaced by the freshly
//             fetched alert when the request resolves.
//   onClose — close handler. Esc, click-on-backdrop, and the X button
//             all call this.
export function AlertDetail({
  open,
  alert,
  onClose,
}: {
  open: boolean
  alert: Alert | null
  onClose: () => void
}) {
  const ref = useRef<HTMLDivElement>(null)
  const id = alert?.id ?? ''

  // Fetch detail when the modal is open and we have an id. Disabled
  // when closed so query state doesn't accumulate, and gated on id so
  // an empty fetch doesn't fire during the close-then-reopen flicker.
  const detail = useQuery({
    queryKey: ['alert-detail', id],
    queryFn: () => api.alertDetail(id),
    enabled: open && id !== '',
    refetchOnWindowFocus: false,
  })

  // Esc closes; lock body scroll while open. Same pattern as
  // FlowDrawer so muscle memory carries across surfaces.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    ref.current?.focus()
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [open, onClose])

  if (!open || !alert) return null

  const live = detail.data?.alert ?? alert

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={`Alert detail: ${alert.title}`}
      data-testid="alert-detail-modal"
      className="fixed inset-0 z-40"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="absolute inset-0 bg-black/60" />
      <div
        ref={ref}
        tabIndex={-1}
        className="relative ml-auto h-full bg-surface border-l border-line shadow-xl outline-none flex flex-col w-[840px] max-w-[100vw]"
        onClick={(e) => e.stopPropagation()}
      >
        <Header alert={live} onClose={onClose} />
        <div className="flex-1 overflow-y-auto">
          <Section title="Timeline">
            <Timeline
              events={detail.data?.timeline}
              loading={detail.isLoading}
              error={detail.error as Error | undefined}
            />
          </Section>
          <Section title="Linked flows" subtitle={detail.data?.flows_source}>
            <Flows
              flows={detail.data?.flows}
              loading={detail.isLoading}
              error={detail.error as Error | undefined}
            />
          </Section>
          <Section title="Labels">
            <Labels labels={live.labels} />
          </Section>
        </div>
      </div>
    </div>
  )
}

/* ----------------------------- Header ----------------------------- */

function Header({ alert, onClose }: { alert: Alert; onClose: () => void }) {
  const sevColor =
    alert.severity === 'critical'
      ? 'text-crit'
      : alert.severity === 'warning'
        ? 'text-warn'
        : 'text-accent'
  const sevBar =
    alert.severity === 'critical'
      ? 'bg-crit'
      : alert.severity === 'warning'
        ? 'bg-warn'
        : 'bg-accent'
  return (
    <div className="relative border-b border-line">
      <div className={`absolute left-0 top-0 bottom-0 w-[3px] ${sevBar}`} aria-hidden />
      <div className="grid grid-cols-[1fr_auto] gap-4 pl-7 pr-6 py-4">
        <div className="min-w-0">
          <div className="flex items-baseline gap-3 mb-1">
            <span
              className={`font-mono text-[10px] uppercase tracking-[0.18em] font-semibold ${sevColor}`}
            >
              {alert.severity}
            </span>
            <span className="font-mono text-[10.5px] text-faint">
              ·{' '}
              {alert.state === 'acknowledged'
                ? 'acked'
                : alert.state === 'closed'
                  ? 'closed'
                  : 'active'}
            </span>
            <span className="font-mono text-[10.5px] text-faint tabular">
              opened {fmt.time(alert.opened_at).slice(11, 19)}Z
            </span>
          </div>
          <div className="text-[15px] font-medium text-text leading-[1.3]">{alert.title}</div>
          <p className="text-[13px] text-dim mt-1.5 max-w-[78ch] leading-[1.5]">{alert.body}</p>
          <div className="flex items-center gap-4 mt-3 font-mono text-[11px] text-faint">
            <span>
              <span className="text-faint">scope ·</span>{' '}
              <span className="text-dim">{alert.scope_display || alert.scope}</span>
            </span>
            <span>
              <span className="text-faint">rule ·</span>{' '}
              <span className="text-dim">{alert.rule_id}</span>
            </span>
            {alert.runbook && (
              <span>
                <span className="text-faint">runbook ·</span>{' '}
                <span className="text-accent">{alert.runbook}</span>
              </span>
            )}
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="text-faint hover:text-text font-mono text-[14px]"
          aria-label="close"
        >
          ✕
        </button>
      </div>
    </div>
  )
}

/* ----------------------------- Section ----------------------------- */

function Section({
  title,
  subtitle,
  children,
}: {
  title: string
  subtitle?: string
  children: React.ReactNode
}) {
  return (
    <div className="border-b border-line">
      <div className="flex items-baseline px-6 py-2.5 bg-ink border-b border-line">
        <span className="text-[10px] uppercase tracking-[0.18em] font-mono font-semibold text-faint">
          {title}
        </span>
        {subtitle && (
          <span className="ml-3 font-mono text-[10.5px] text-faint">· {subtitle}</span>
        )}
      </div>
      <div>{children}</div>
    </div>
  )
}

/* ----------------------------- Timeline ----------------------------- */

function Timeline({
  events,
  loading,
  error,
}: {
  events?: AlertEvent[]
  loading: boolean
  error?: Error
}) {
  if (loading) {
    // Skeleton — three placeholder rows so the panel doesn't
    // collapse to "loading…" text for what feels like an empty
    // panel.
    return (
      <ul>
        {[0, 1, 2].map((i) => (
          <li key={i} className="px-6 py-3 border-b border-line last:border-b-0">
            <div className="h-3 w-24 bg-line/60 mb-2 animate-pulse" />
            <div className="h-2 w-full bg-line/40 animate-pulse" />
          </li>
        ))}
      </ul>
    )
  }
  if (error) {
    return (
      <div className="px-6 py-4 text-crit font-mono text-[12px]">error · {error.message}</div>
    )
  }
  if (!events || events.length === 0) {
    return (
      <div className="px-6 py-4 text-faint font-mono text-[12px]">no events recorded</div>
    )
  }
  // Newest at the top is the operator's expectation when triaging an
  // active incident; reverse the chronological order returned by the
  // API for display.
  const ordered = [...events].reverse()
  return (
    <ul>
      {ordered.map((ev, i) => (
        <TimelineRow key={`${ev.ts}-${i}`} ev={ev} />
      ))}
    </ul>
  )
}

function TimelineRow({ ev }: { ev: AlertEvent }) {
  const isTransition = ev.state !== 'heartbeat'
  // Severity color drives state badges; transitions get a brighter
  // bar to stand out from the heartbeat stream.
  const stateColor =
    ev.state === 'opened'
      ? 'text-warn'
      : ev.state === 'closed'
        ? 'text-ok'
        : ev.state === 'acknowledged'
          ? 'text-accent'
          : 'text-faint'
  return (
    <li className="px-6 py-3 border-b border-line last:border-b-0">
      <div className="flex items-baseline gap-3">
        <span
          className={`font-mono text-[10px] uppercase tracking-[0.18em] font-semibold ${stateColor}`}
        >
          {ev.state}
        </span>
        <span className="font-mono text-[11px] tabular text-dim">
          {fmt.time(ev.ts).slice(11, 19)}Z
        </span>
        {ev.actor && ev.actor !== 'engine' && (
          <span className="font-mono text-[10.5px] text-faint">· by {ev.actor}</span>
        )}
        {!isTransition && (
          <span className="font-mono text-[10.5px] text-faint">· sample</span>
        )}
      </div>
      {ev.body && (
        <p className="text-[12.5px] text-dim mt-1 max-w-[78ch] leading-[1.5]">{ev.body}</p>
      )}
    </li>
  )
}

/* ----------------------------- Linked flows ----------------------------- */

function Flows({
  flows,
  loading,
  error,
}: {
  flows?: RecentFlow[]
  loading: boolean
  error?: Error
}) {
  if (loading) {
    return (
      <ul>
        {[0, 1, 2].map((i) => (
          <li key={i} className="px-6 py-3 border-b border-line last:border-b-0">
            <div className="h-3 w-48 bg-line/60 mb-2 animate-pulse" />
            <div className="h-2 w-32 bg-line/40 animate-pulse" />
          </li>
        ))}
      </ul>
    )
  }
  if (error) {
    return (
      <div className="px-6 py-4 text-crit font-mono text-[12px]">error · {error.message}</div>
    )
  }
  if (!flows || flows.length === 0) {
    return (
      <div className="px-6 py-4 text-faint font-mono text-[12px]">
        no flows match this alert's labels in the alert window
      </div>
    )
  }
  return (
    <ul>
      {flows.map((f, i) => (
        <li
          key={`${f.observed}-${f.src_addr}-${f.dst_addr}-${i}`}
          className="px-6 py-3 border-b border-line last:border-b-0"
        >
          <div className="font-mono text-[12px] text-dim tabular">
            <span className="text-text">{f.src_addr}</span>
            {f.src_port > 0 && <span className="text-faint">:{f.src_port}</span>}
            <span className="text-faint mx-2">→</span>
            <span className="text-text">{f.dst_addr}</span>
            {f.dst_port > 0 && <span className="text-faint">:{f.dst_port}</span>}
            <span className="text-faint ml-3">· {fmt.proto(f.proto)}</span>
          </div>
          <div className="flex items-center gap-4 mt-1 font-mono text-[10.5px] text-faint tabular">
            <span>{fmt.time(f.observed).slice(11, 19)}Z</span>
            <span>{fmt.bytes(f.bytes)}</span>
            <span>{fmt.num(f.packets)} pkts</span>
            <span>via {f.exporter_name || f.exporter}</span>
          </div>
        </li>
      ))}
    </ul>
  )
}

/* ----------------------------- Labels ----------------------------- */

function Labels({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels ?? {}).sort(([a], [b]) => a.localeCompare(b))
  if (entries.length === 0) {
    return <div className="px-6 py-4 text-faint font-mono text-[12px]">no labels</div>
  }
  return (
    <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 px-6 py-3 font-mono text-[11.5px]">
      {entries.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-faint">{k}</dt>
          <dd className="text-dim">{v}</dd>
        </div>
      ))}
    </dl>
  )
}
