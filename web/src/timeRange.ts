import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { getConfig } from './config'

// PRESETS are the trailing-window options offered in the selector. The
// values are Go-duration strings the API understands directly.
export const PRESETS = ['5m', '15m', '1h', '6h', '24h'] as const
export type Preset = (typeof PRESETS)[number]

// 1h gives ~60 resource samples at the default 60s SNMP cadence, so
// sparklines + charts have something to draw on first load. Operators
// can still pin to 5m or stretch to 24h via the TimeRangeSelector.
export const DEFAULT_PRESET: Preset = '1h'

// configuredPreset returns the operator-configured tenant default
// (Settings -> General -> default_time_range) when it's a known
// preset, otherwise the hard-coded DEFAULT_PRESET. Read at hook-mount
// time so the value is always the freshly hydrated config.
function configuredPreset(): Preset {
  const cfg = getConfig().default_time_range
  if ((PRESETS as readonly string[]).includes(cfg)) return cfg as Preset
  return DEFAULT_PRESET
}

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

function readFromURL(): TimeRange {
  if (typeof window === 'undefined') return DEFAULT_TIME_RANGE
  const sp = new URLSearchParams(window.location.search)
  const fromStr = sp.get('from')
  const toStr = sp.get('to')
  if (fromStr && toStr) {
    const from = new Date(fromStr)
    const to = new Date(toStr)
    if (!isNaN(from.getTime()) && !isNaN(to.getTime())) {
      return { kind: 'absolute', from, to }
    }
  }
  const win = sp.get('window')
  if (win && (PRESETS as readonly string[]).includes(win)) {
    return { kind: 'preset', window: win as Preset }
  }
  return { kind: 'preset', window: configuredPreset() }
}

function writeToURL(r: TimeRange) {
  if (typeof window === 'undefined') return
  const sp = new URLSearchParams(window.location.search)
  // Wipe any prior time keys so we never end up with stale window=
  // alongside a fresh from/to.
  sp.delete('window')
  sp.delete('from')
  sp.delete('to')
  if (r.kind === 'preset') {
    // The currently-effective default preset (operator-configured or
    // hard-coded) is implicit — leave the URL clean. Anything else is
    // an explicit override and gets serialized.
    if (r.window !== configuredPreset()) sp.set('window', r.window)
  } else {
    sp.set('from', r.from.toISOString())
    sp.set('to', r.to.toISOString())
  }
  const qs = sp.toString()
  const next = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  if (window.location.pathname + window.location.search !== next) {
    window.history.replaceState({}, '', next)
  }
}

// TimeRangeAPI is the shape the rest of the app consumes — read the
// current range, set a new one, get a queryKey for React Query deps.
export type TimeRangeAPI = {
  range: TimeRange
  set: (r: TimeRange) => void
  setPreset: (p: Preset) => void
  setAbsolute: (from: Date, to: Date) => void
  reset: () => void
  queryKey: unknown
}

// Internal hook that builds the state. Used by the provider; not
// exported, since calling it twice would create independent state
// instances that don't share via React (URL sync would partially
// paper over the divergence, but updates wouldn't be reactive).
function useTimeRangeState(): TimeRangeAPI {
  const [range, setRange] = useState<TimeRange>(() => readFromURL())

  // React to back/forward navigation that may change the URL.
  useEffect(() => {
    const onPop = () => setRange(readFromURL())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  // Mirror state to URL whenever it changes.
  useEffect(() => {
    writeToURL(range)
  }, [range])

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

// TimeRangeContext lets every chart, panel, and selector share the
// same range state. Brushing a chart calls setAbsolute on this
// context; the URL is updated, every consumer re-renders, and
// dependent queries refetch with the narrowed window.
const TimeRangeContext = createContext<TimeRangeAPI | null>(null)

// Module-level live-mode flag. Mirrors `range.kind === 'preset'` so
// the QueryClient's defaultOptions (set once at app boot, before any
// React tree exists) can consult the current mode without taking a
// dependency on the React context. TimeRangeProvider keeps this in
// sync via the effect below.
//
// "live" = trailing-window preset → auto-refresh on each query's
//          configured cadence
// "fixed" = absolute (from, to) range → freeze all auto-refreshes;
//           only manual invalidations (refresh button, Walk now,
//           etc.) trigger a refetch
let liveModeFlag = true

export function isLive(): boolean {
  return liveModeFlag
}

// useLiveInterval is the helper every useQuery should wrap its
// refetchInterval in. Returns the supplied cadence when in live
// mode, false when frozen. Pure function of the range; safe to call
// in any component that's inside a TimeRangeProvider.
export function useLiveInterval(ms: number): number | false {
  const { range } = useTimeRange()
  return range.kind === 'preset' ? ms : false
}

export function TimeRangeProvider({ children }: { children: ReactNode }) {
  const value = useTimeRangeState()
  // Keep the module-level mirror in sync so the QueryClient default
  // refetchInterval function picks up mode changes.
  useEffect(() => {
    liveModeFlag = value.range.kind === 'preset'
  }, [value.range.kind])
  return createElement(TimeRangeContext.Provider, { value }, children)
}

// useTimeRange returns the shared API. Must be called inside a
// <TimeRangeProvider>; the provider lives at the app root so anything
// inside the React tree (App's TimeRangeSelector, TimeseriesChart's
// brush handler, Devices' per-tab consumers) sees the same state.
export function useTimeRange(): TimeRangeAPI {
  const ctx = useContext(TimeRangeContext)
  if (!ctx) {
    throw new Error('useTimeRange must be used inside <TimeRangeProvider>')
  }
  return ctx
}
