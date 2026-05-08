import { useEffect, useState, useCallback } from 'react'

// FilterKey enumerates the dimensions the operator can pin in the
// Flows tab. The same set is what the api accepts on the /api/top/*
// endpoints — keep this list and store.FlowFilter in lockstep.
export type FilterKey =
  | 'exporter'
  | 'src_addr'
  | 'dst_addr'
  | 'src_port'
  | 'dst_port'
  | 'proto'
  | 'input_ifindex'
  | 'output_ifindex'

export const FILTER_KEYS: readonly FilterKey[] = [
  'exporter',
  'src_addr',
  'dst_addr',
  'src_port',
  'dst_port',
  'proto',
  'input_ifindex',
  'output_ifindex',
] as const

export type Filter = {
  key: FilterKey
  value: string
  // Human form of the value, for chip rendering (e.g. "https" for "443").
  label?: string
  // Human form of the key, for chip rendering. Most callers can rely on
  // keyLabelFor() defaults; this override is for cases where the same
  // underlying key carries a different concept in context — clicking a
  // dst_port in the Services panel produces "service · https", not
  // "dst port · https".
  keyLabel?: string
}

// keyLabelFor is the default human form of a FilterKey for chip
// rendering. Triggers can override via Filter.keyLabel.
const KEY_LABELS: Record<FilterKey, string> = {
  exporter: 'exporter',
  src_addr: 'src',
  dst_addr: 'dst',
  src_port: 'src port',
  dst_port: 'dst port',
  proto: 'proto',
  input_ifindex: 'in iface',
  output_ifindex: 'out iface',
}

export function keyLabelFor(key: FilterKey): string {
  return KEY_LABELS[key]
}

const ALLOWED: ReadonlySet<string> = new Set<FilterKey>(FILTER_KEYS)

// LABEL_PARAM_PREFIX is the URL convention for round-tripping a
// chip's human-readable label. Adding ?_l_exporter=troy-leaf-01
// alongside ?exporter=10.110.0.182 lets a freshly mounted page
// render the chip with the SNMP-resolved name instead of the bare
// IP. Labels are optional — readFromURL falls back to the value if
// the matching _l_<key> isn't present.
const LABEL_PARAM_PREFIX = '_l_'

// Read the current URL search params into an array of Filter values.
// Unknown keys are dropped silently so a malformed URL never breaks
// the dashboard. Optional ?_l_<key>=label round-trips the human form
// of the chip so reloads don't lose the SNMP-resolved name.
function readFromURL(): Filter[] {
  if (typeof window === 'undefined') return []
  const sp = new URLSearchParams(window.location.search)
  const out: Filter[] = []
  for (const [k, v] of sp.entries()) {
    if (ALLOWED.has(k) && v !== '') {
      const label = sp.get(LABEL_PARAM_PREFIX + k) ?? undefined
      out.push({ key: k as FilterKey, value: v, label })
    }
  }
  return out
}

// Push the current filter list back into the URL without reloading.
// Uses replaceState so chips coming and going don't pollute the back
// stack — a hard refresh still reads the current set. Preserves all
// non-filter params (time range, etc.) so other URL-backed state isn't
// trampled. Labels are persisted under the _l_<key> prefix.
function writeToURL(filters: Filter[]) {
  if (typeof window === 'undefined') return
  const sp = new URLSearchParams(window.location.search)
  for (const k of ALLOWED) {
    sp.delete(k)
    sp.delete(LABEL_PARAM_PREFIX + k)
  }
  for (const f of filters) {
    sp.set(f.key, f.value)
    if (f.label && f.label !== f.value) {
      sp.set(LABEL_PARAM_PREFIX + f.key, f.label)
    }
  }
  const qs = sp.toString()
  const next = qs ? `${window.location.pathname}?${qs}` : window.location.pathname
  if (window.location.pathname + window.location.search !== next) {
    window.history.replaceState({}, '', next)
  }
}

// useFilters is the single source of truth for Flows-tab filter chips.
// Components consuming filters subscribe to {filters, add, remove,
// clear} and pass the same array to the api wrappers.
export function useFilters(): {
  filters: Filter[]
  add: (f: Filter) => void
  remove: (key: FilterKey, value?: string) => void
  clear: () => void
} {
  const [filters, setFilters] = useState<Filter[]>(() => readFromURL())

  // React to back/forward navigation that may change the URL.
  useEffect(() => {
    const onPop = () => setFilters(readFromURL())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])

  // Sync URL whenever filters change.
  useEffect(() => {
    writeToURL(filters)
  }, [filters])

  const add = useCallback((f: Filter) => {
    setFilters((curr) => {
      // One value per key — adding a new value for the same key
      // replaces the old one. (E.g. clicking another exporter swaps
      // the exporter filter, doesn't accumulate.)
      const without = curr.filter((x) => x.key !== f.key)
      return [...without, f]
    })
  }, [])

  const remove = useCallback((key: FilterKey, value?: string) => {
    setFilters((curr) =>
      curr.filter((f) => f.key !== key || (value !== undefined && f.value !== value)),
    )
  }, [])

  const clear = useCallback(() => setFilters([]), [])

  return { filters, add, remove, clear }
}

// toQuery serializes a filter array for the api wrappers.
export function toQuery(filters: Filter[]): URLSearchParams {
  const sp = new URLSearchParams()
  for (const f of filters) {
    if (f.value !== '') sp.set(f.key, f.value)
  }
  return sp
}
