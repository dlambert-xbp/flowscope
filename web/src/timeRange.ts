import { useCallback, useEffect, useMemo, useState } from 'react'

// PRESETS are the trailing-window options offered in the selector. The
// values are Go-duration strings the API understands directly.
export const PRESETS = ['5m', '15m', '1h', '6h', '24h'] as const
export type Preset = (typeof PRESETS)[number]

export const DEFAULT_PRESET: Preset = '5m'

// TimeRange is either a trailing-window preset or an absolute interval.
export type TimeRange =
  | { kind: 'preset'; window: Preset }
  | { kind: 'absolute'; from: Date; to: Date }

export const DEFAULT_TIME_RANGE: TimeRange = { kind: 'preset', window: DEFAULT_PRESET }

// presetSeconds maps a preset to its duration in seconds — used when the
// API needs `seconds=N` rather than `window=Xs`.
const PRESET_SECONDS: Record<Preset, number> = {
  '5m': 300,
  '15m': 900,
  '1h': 3600,
  '6h': 21600,
  '24h': 86400,
}

// rangeSeconds returns the span of a TimeRange in seconds. For absolute
// ranges this is (to - from); for presets it is the preset duration.
export function rangeSeconds(r: TimeRange): number {
  if (r.kind === 'preset') return PRESET_SECONDS[r.window]
  return Math.max(1, Math.floor((r.to.getTime() - r.from.getTime()) / 1000))
}

// rangeLabel renders a TimeRange as a short human label (e.g. "5 min",
// "1 hour", "May 7 14:00 → 15:00 UTC").
export function rangeLabel(r: TimeRange): string {
  if (r.kind === 'preset') return PRESET_LABELS[r.window]
  const f = (d: Date) =>
    d
      .toISOString()
      .replace('T', ' ')
      .slice(5, 16)
  return `${f(r.from)} → ${f(r.to)} UTC`
}

const PRESET_LABELS: Record<Preset, string> = {
  '5m': '5 min',
  '15m': '15 min',
  '1h': '1 hour',
  '6h': '6 hours',
  '24h': '24 hours',
}

// toApi converts a TimeRange into the shape api wrappers accept. Pass
// the result as the `range` argument to api.summary / api.devices /
// api.interfaces / api.top* / api.interfaceTimeseries.
export function toApi(r: TimeRange): string | { from: Date; to: Date } {
  if (r.kind === 'preset') return r.window
  return { from: r.from, to: r.to }
}

// rangeToParams returns the query params an api wrapper should append.
// Presets emit `window=Xs`; absolute ranges emit `from=...&to=...`.
export function rangeToParams(r: TimeRange): URLSearchParams {
  const sp = new URLSearchParams()
  if (r.kind === 'preset') {
    sp.set('window', r.window)
  } else {
    sp.set('from', r.from.toISOString())
    sp.set('to', r.to.toISOString())
  }
  return sp
}

// keyFor produces the namespaced URL params used by useTimeRange so
// each tab keeps an independent range. Key is the scope (e.g. 'ov').
const keyFor = (scope: string, suffix: 'window' | 'from' | 'to') => `${scope}_${suffix}`

function readFromURL(scope: string): TimeRange {
  if (typeof window === 'undefined') return DEFAULT_TIME_RANGE
  const sp = new URLSearchParams(window.location.search)
  const fromStr = sp.get(keyFor(scope, 'from'))
  const toStr = sp.get(keyFor(scope, 'to'))
  if (fromStr && toStr) {
    const from = new Date(fromStr)
    const to = new Date(toStr)
    if (!isNaN(from.getTime()) && !isNaN(to.getTime())) {
      return { kind: 'absolute', from, to }
    }
  }
  const win = sp.get(keyFor(scope, 'window'))
  if (win && (PRESETS as readonly string[]).includes(win)) {
    return { kind: 'preset', window: win as Preset }
  }
  return DEFAULT_TIME_RANGE
}

function writeToURL(scope: string, r: TimeRange) {
  if (typeof window === 'undefined') return
  const sp = new URLSearchParams(window.location.search)
  // Wipe any prior keys for this scope so we never end up with stale
  // window= alongside a fresh from/to.
  sp.delete(keyFor(scope, 'window'))
  sp.delete(keyFor(scope, 'from'))
  sp.delete(keyFor(scope, 'to'))
  if (r.kind === 'preset') {
    // Default preset is implicit — leave the URL clean.
    if (r.window !== DEFAULT_PRESET) sp.set(keyFor(scope, 'window'), r.window)
  } else {
    sp.set(keyFor(scope, 'from'), r.from.toISOString())
    sp.set(keyFor(scope, 'to'), r.to.toISOString())
  }
  const qs = sp.toString()
  const next = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  if (window.location.pathname + window.location.search !== next) {
    window.history.replaceState({}, '', next)
  }
}

// useTimeRange is a per-tab time range hook. The `scope` arg names the
// tab so each tab's range is encoded under its own URL params (e.g.
// ov_window, fl_window, dv_window). React Query callers should include
// the queryKey() helper in their queryKey so range changes refetch.
export function useTimeRange(scope: string): {
  range: TimeRange
  set: (r: TimeRange) => void
  setPreset: (p: Preset) => void
  setAbsolute: (from: Date, to: Date) => void
  reset: () => void
  queryKey: unknown
} {
  const [range, setRange] = useState<TimeRange>(() => readFromURL(scope))

  // React to back/forward navigation that may change the URL.
  useEffect(() => {
    const onPop = () => setRange(readFromURL(scope))
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [scope])

  // Mirror state to URL whenever it changes.
  useEffect(() => {
    writeToURL(scope, range)
  }, [scope, range])

  const set = useCallback((r: TimeRange) => setRange(r), [])
  const setPreset = useCallback(
    (p: Preset) => setRange({ kind: 'preset', window: p }),
    [],
  )
  const setAbsolute = useCallback(
    (from: Date, to: Date) => setRange({ kind: 'absolute', from, to }),
    [],
  )
  const reset = useCallback(() => setRange(DEFAULT_TIME_RANGE), [])

  // queryKey is a stable, serializable representation of the current
  // range. Components include this in their useQuery keys so range
  // changes trigger refetches without manual invalidation.
  const queryKey = useMemo<unknown>(() => {
    if (range.kind === 'preset') return ['preset', range.window]
    return ['absolute', range.from.toISOString(), range.to.toISOString()]
  }, [range])

  return { range, set, setPreset, setAbsolute, reset, queryKey }
}
