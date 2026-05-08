import { useEffect, useRef, useState, type RefObject } from 'react'
import {
  PRESETS,
  rangeLabel,
  type Preset,
  type TimeRange,
} from '../timeRange'

// TimeRangeSelector is the per-tab time-window picker. Shows a preset
// button strip (5m / 15m / 1h / 6h / 24h) plus an absolute toggle that
// opens a popover with two datetime-local inputs. Aesthetic matches the
// rest of FlowScope: 1px lines, monospace labels, accent on selection.
export function TimeRangeSelector({
  range,
  onChange,
}: {
  range: TimeRange
  onChange: (r: TimeRange) => void
}) {
  const [open, setOpen] = useState(false)
  const popRef = useRef<HTMLDivElement | null>(null)

  // Close popover on outside click.
  useEffect(() => {
    if (!open) return
    const onClick = (e: MouseEvent) => {
      if (popRef.current && !popRef.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    window.addEventListener('mousedown', onClick)
    return () => window.removeEventListener('mousedown', onClick)
  }, [open])

  return (
    <div className="relative inline-flex items-center gap-2 font-mono text-[11px]">
      <span className="text-faint text-[10px] uppercase tracking-[0.1em]">range</span>
      <div className="inline-flex border border-line">
        {PRESETS.map((p) => (
          <PresetBtn
            key={p}
            preset={p}
            active={range.kind === 'preset' && range.window === p}
            onClick={() => onChange({ kind: 'preset', window: p })}
          />
        ))}
        <button
          onClick={() => setOpen((v) => !v)}
          className={`px-2 py-1 border-l border-line whitespace-nowrap ${
            range.kind === 'absolute'
              ? 'bg-accent-wash text-text'
              : 'text-dim hover:text-text hover:bg-surface'
          }`}
          aria-pressed={range.kind === 'absolute'}
          aria-expanded={open}
          title={range.kind === 'absolute' ? rangeLabel(range) : 'pick an absolute range'}
        >
          {range.kind === 'absolute' ? rangeLabel(range) : 'custom'}
        </button>
      </div>
      {open && (
        <AbsolutePopover
          ref={popRef}
          initial={range.kind === 'absolute' ? range : undefined}
          onApply={(from, to) => {
            onChange({ kind: 'absolute', from, to })
            setOpen(false)
          }}
          onCancel={() => setOpen(false)}
        />
      )}
    </div>
  )
}

function PresetBtn({
  preset,
  active,
  onClick,
}: {
  preset: Preset
  active: boolean
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={`px-2 py-1 border-r border-line last:border-r-0 ${
        active
          ? 'bg-accent-wash text-text'
          : 'text-dim hover:text-text hover:bg-surface'
      }`}
    >
      {preset}
    </button>
  )
}

const AbsolutePopover = ({
  ref,
  initial,
  onApply,
  onCancel,
}: {
  ref: RefObject<HTMLDivElement | null>
  initial?: { from: Date; to: Date }
  onApply: (from: Date, to: Date) => void
  onCancel: () => void
}) => {
  const now = new Date()
  const oneHourAgo = new Date(now.getTime() - 60 * 60 * 1000)
  const [fromStr, setFromStr] = useState(toLocal(initial?.from ?? oneHourAgo))
  const [toStr, setToStr] = useState(toLocal(initial?.to ?? now))
  const [err, setErr] = useState<string | null>(null)

  const apply = () => {
    const from = new Date(fromStr)
    const to = new Date(toStr)
    if (isNaN(from.getTime()) || isNaN(to.getTime())) {
      setErr('invalid timestamp')
      return
    }
    if (to <= from) {
      setErr('end must be after start')
      return
    }
    const span = (to.getTime() - from.getTime()) / 1000
    if (span > 168 * 60 * 60) {
      setErr('range cannot exceed 7 days')
      return
    }
    onApply(from, to)
  }

  return (
    <div
      ref={ref}
      className="absolute top-full right-0 mt-1 z-50 bg-surface border border-line shadow-lg p-3 min-w-[280px]"
    >
      <div className="text-[10px] uppercase tracking-[0.1em] text-faint font-semibold mb-2">
        absolute range · local
      </div>
      <Field label="from" value={fromStr} onChange={setFromStr} />
      <Field label="to" value={toStr} onChange={setToStr} />
      {err && <div className="text-crit text-[10.5px] mt-1.5">{err}</div>}
      <div className="flex gap-2 mt-3 justify-end">
        <button
          onClick={onCancel}
          className="px-3 py-1 border border-line text-dim hover:text-text"
        >
          cancel
        </button>
        <button
          onClick={apply}
          className="px-3 py-1 border border-accent text-accent hover:bg-accent-wash"
        >
          apply
        </button>
      </div>
    </div>
  )
}

function Field({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <label className="flex items-center gap-2 mb-1.5">
      <span className="w-10 text-faint text-[10.5px] uppercase tracking-[0.08em]">
        {label}
      </span>
      <input
        type="datetime-local"
        value={value}
        step={1}
        onChange={(e) => onChange(e.target.value)}
        className="flex-1 bg-ink border border-line px-2 py-1 text-text outline-none focus:border-accent font-mono text-[11px]"
      />
    </label>
  )
}

// toLocal formats a Date for an HTML datetime-local input.
function toLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  )
}
