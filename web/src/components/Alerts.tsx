import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { api, fmt } from '../api'
import type { Alert, AlertSummary } from '../api'
import { AlertDetail } from './AlertDetail'

type StateFilter = 'open' | 'acknowledged' | 'closed'

// Alerts tab — summary stats up top, filter tabs (Open / Acknowledged
// / Closed 24h), and a list of alerts each rendered as its own
// "story" with severity, scope, runbook, and ack/close actions.
export function Alerts() {
  const [state, setState] = useState<StateFilter>('open')
  // Selected alert for the detail modal. Setting this is the
  // synchronous state flip the click handler does *before* the fetch
  // — that's what makes the modal feel immediate (memory:
  // render-on-state-change).
  const [selected, setSelected] = useState<Alert | null>(null)

  const summary = useQuery({
    queryKey: ['alerts-summary'],
    queryFn: () => api.alertSummary(),
    refetchInterval: 5000,
  })
  const list = useQuery({
    queryKey: ['alerts', state],
    queryFn: () => api.alerts(state),
    refetchInterval: 5000,
  })
  const counts = summary.data
  const totalOpen = counts ? counts.open_critical + counts.open_warning + counts.open_info : 0

  return (
    <div>
      <SummaryBar counts={counts} />
      <FilterTabs
        active={state}
        onChange={setState}
        openCount={totalOpen}
        ackCount={counts?.acknowledged ?? 0}
        closedCount={counts?.closed_last_24h ?? 0}
      />
      <List
        alerts={list.data?.alerts ?? []}
        loading={list.isLoading}
        error={list.error as Error | undefined}
        onSelect={setSelected}
      />
      <AlertDetail
        open={selected !== null}
        alert={selected}
        onClose={() => setSelected(null)}
      />
    </div>
  )
}

/* ----------------------------- Summary stats ----------------------------- */

function SummaryBar({ counts }: { counts?: AlertSummary }) {
  const tiles: { label: string; value: number; tone: 'crit' | 'warn' | 'info' | 'ok' }[] = [
    { label: 'critical · open', value: counts?.open_critical ?? 0, tone: 'crit' },
    { label: 'warning · open', value: counts?.open_warning ?? 0, tone: 'warn' },
    { label: 'info · open', value: counts?.open_info ?? 0, tone: 'info' },
    { label: 'closed · 24h', value: counts?.closed_last_24h ?? 0, tone: 'ok' },
  ]
  return (
    <div className="grid grid-cols-2 md:grid-cols-4 border-b border-line">
      {tiles.map((t, i) => {
        const wash =
          t.tone === 'crit'
            ? 'bg-crit-wash'
            : t.tone === 'warn'
              ? 'bg-warn-wash'
              : t.tone === 'info'
                ? ''
                : 'bg-ok-wash'
        const valueColor =
          t.tone === 'crit'
            ? 'text-crit'
            : t.tone === 'warn'
              ? 'text-warn'
              : t.tone === 'info'
                ? 'text-accent'
                : 'text-ok'
        const visible = t.tone === 'info' ? '' : t.value > 0 ? wash : ''
        return (
          <div
            key={i}
            className={`px-4 py-3.5 border-r border-line last:border-r-0 ${visible}`}
          >
            <div className="text-[10px] uppercase tracking-[0.1em] font-mono font-semibold text-faint mb-1.5">
              {t.label}
            </div>
            <div
              className={`font-mono text-[24px] tabular leading-[1.1] tracking-[-0.02em] ${
                t.value > 0 ? valueColor : 'text-text'
              }`}
            >
              {fmt.num(t.value)}
            </div>
          </div>
        )
      })}
    </div>
  )
}

/* ----------------------------- Filter tabs ----------------------------- */

function FilterTabs({
  active,
  onChange,
  openCount,
  ackCount,
  closedCount,
}: {
  active: StateFilter
  onChange: (s: StateFilter) => void
  openCount: number
  ackCount: number
  closedCount: number
}) {
  return (
    <div className="flex border-b border-line bg-ink">
      <Tab id="open" active={active} onChange={onChange} count={openCount}>
        Open
      </Tab>
      <Tab id="acknowledged" active={active} onChange={onChange} count={ackCount}>
        Acknowledged
      </Tab>
      <Tab id="closed" active={active} onChange={onChange} count={closedCount}>
        Closed · 24h
      </Tab>
    </div>
  )
}

function Tab({
  id,
  active,
  onChange,
  count,
  children,
}: {
  id: StateFilter
  active: StateFilter
  onChange: (s: StateFilter) => void
  count: number
  children: ReactNode
}) {
  const selected = id === active
  return (
    <button
      onClick={() => onChange(id)}
      className={`relative px-4 py-2.5 text-[13px] border-r border-line ${
        selected ? 'text-text' : 'text-dim hover:text-text hover:bg-surface'
      }`}
    >
      {children}
      <span className="ml-2 font-mono text-[11px] text-faint tabular">{count}</span>
      {selected && <span className="absolute left-0 right-0 -bottom-px h-0.5 bg-accent" />}
    </button>
  )
}

/* ----------------------------- Alert list ----------------------------- */

function List({
  alerts,
  loading,
  error,
  onSelect,
}: {
  alerts: Alert[]
  loading: boolean
  error?: Error
  onSelect: (a: Alert) => void
}) {
  if (loading) {
    return <div className="px-6 py-6 text-faint font-mono text-[12px]">loading…</div>
  }
  if (error) {
    return (
      <div className="px-6 py-6 text-crit font-mono text-[12px]">error · {error.message}</div>
    )
  }
  if (alerts.length === 0) {
    return (
      <div className="px-6 py-12 text-center text-[13px] font-mono text-dim">
        no alerts · drive synth to populate flows, then wait for rule conditions
      </div>
    )
  }
  return (
    <ul>
      {alerts.map((a) => (
        <Row key={a.id} alert={a} onSelect={onSelect} />
      ))}
    </ul>
  )
}

function Row({ alert, onSelect }: { alert: Alert; onSelect: (a: Alert) => void }) {
  const qc = useQueryClient()
  const ack = useMutation({
    mutationFn: () => api.ackAlert(alert.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alerts'] })
      qc.invalidateQueries({ queryKey: ['alerts-summary'] })
    },
  })
  const close = useMutation({
    mutationFn: () => api.closeAlert(alert.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['alerts'] })
      qc.invalidateQueries({ queryKey: ['alerts-summary'] })
    },
  })

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

  // Click on the row content opens the detail modal. Per the
  // render-on-state-change rule the parent flips state synchronously
  // here — the AlertDetail component shows skeleton placeholders
  // while the GET /api/alerts/{id} fetch resolves. Action buttons
  // (ack / close) are siblings, so clicks on them don't trigger the
  // modal because the listener is on the content button only.
  return (
    <li className="border-b border-line">
      <div className="grid grid-cols-[3px_1fr_auto] gap-4 px-6 py-4">
        <div className={`${sevBar} -mx-6`} />
        <button
          type="button"
          onClick={() => onSelect(alert)}
          aria-label={`Open detail for alert: ${alert.title}`}
          className="min-w-0 text-left hover:bg-ink/40 -mx-2 px-2 -my-1 py-1 rounded-sm cursor-pointer"
        >
          <div className="flex items-baseline gap-3 mb-1">
            <span className={`font-mono text-[10px] uppercase tracking-[0.18em] font-semibold ${sevColor}`}>
              {alert.severity}
            </span>
            <span className="font-mono text-[10.5px] text-faint">
              {alert.state === 'acknowledged' ? '· acked' : alert.state === 'closed' ? '· closed' : '· active'}
            </span>
            <span className="font-mono text-[10.5px] text-faint ml-auto tabular">
              opened {fmt.time(alert.opened_at).slice(11, 19)}Z · {since(alert.opened_at)}
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
            {alert.state === 'acknowledged' && (
              <span>
                <span className="text-faint">by ·</span>{' '}
                <span className="text-dim">{alert.actor}</span>
              </span>
            )}
          </div>
        </button>
        <div className="flex flex-col gap-1.5 shrink-0">
          {alert.state !== 'acknowledged' && alert.state !== 'closed' && (
            <Button onClick={() => ack.mutate()} disabled={ack.isPending}>
              ack
            </Button>
          )}
          {alert.state !== 'closed' && (
            <Button onClick={() => close.mutate()} disabled={close.isPending} ghost>
              close
            </Button>
          )}
        </div>
      </div>
    </li>
  )
}

function Button({
  children,
  onClick,
  disabled,
  ghost,
}: {
  children: ReactNode
  onClick: () => void
  disabled?: boolean
  ghost?: boolean
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className={`font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1 border ${
        ghost
          ? 'border-line text-dim hover:text-text hover:border-text'
          : 'border-accent text-accent hover:bg-accent-wash'
      } disabled:opacity-50 disabled:cursor-not-allowed`}
    >
      {children}
    </button>
  )
}

/* ----------------------------- helpers ----------------------------- */

function since(iso: string): string {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return ''
  const sec = Math.max(0, (Date.now() - t) / 1000)
  if (sec < 60) return `${Math.floor(sec)}s ago`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${Math.floor(sec % 60)}s ago`
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m ago`
  return `${Math.floor(sec / 86400)}d ago`
}
