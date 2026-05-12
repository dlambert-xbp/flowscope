// deviceGroups groups exporters by their SNMP sys_location for the
// Devices left-rail folder view. Pure functions, no React — so the
// logic stays trivially unit-testable once vitest lands.
//
// Real-world sys_location strings are messy. Operators leave the
// default ("set me!"), type "unknown" when they don't know, or just
// leave it blank. We collapse those into a single "Uncategorized"
// bucket so the rail doesn't sprout three near-identical folders.

import type { Device } from '../api'

// UNCATEGORIZED_LABEL is the display name for the catch-all bucket
// holding every device whose sys_location is blank or matches one of
// the default placeholder strings.
export const UNCATEGORIZED_LABEL = 'Uncategorized'

// UNCATEGORIZED_SLUG is the stable slug used in data-testid attrs and
// localStorage keys for the catch-all bucket.
export const UNCATEGORIZED_SLUG = 'uncategorized'

// PLACEHOLDER_LOCATIONS are the literal default strings shipped by
// network vendors / left behind by lazy operators. Compared
// case-insensitively against the trimmed sys_location. Real
// sysLocation values get grouped case-sensitively (see groupDevices)
// so "Floor 3" and "FLOOR 3" stay distinct buckets — only the
// well-known defaults collapse to Uncategorized.
const PLACEHOLDER_LOCATIONS: ReadonlySet<string> = new Set([
  'set me!',
  'unknown',
  'set this',
])

// isPlaceholderLocation returns true when the supplied sys_location
// (or undefined) should land in the Uncategorized bucket.
export function isPlaceholderLocation(loc: string | null | undefined): boolean {
  if (loc == null) return true
  const trimmed = loc.trim()
  if (trimmed === '') return true
  return PLACEHOLDER_LOCATIONS.has(trimmed.toLowerCase())
}

// normalizeLocation returns either the trimmed sys_location string or
// null when the value falls into the Uncategorized bucket. Group keys
// are case-sensitive on the trimmed value so messy real-world data
// stays as distinct as the operator typed it — that's a feature; if
// they want sites merged they can fix the SNMP config.
export function normalizeLocation(loc: string | null | undefined): string | null {
  if (isPlaceholderLocation(loc)) return null
  return (loc as string).trim()
}

// DeviceGroup is one folder in the left-rail directory.
export type DeviceGroup = {
  // name is the display name shown in the folder header.
  name: string
  // slug is the stable, URL-safe identifier used for data-testid and
  // localStorage state. Always lowercase, ASCII-only.
  slug: string
  // isUncategorized flags the catch-all bucket so callers can sort it
  // to the bottom independent of its display name.
  isUncategorized: boolean
  // devices are the exporters in this group, preserving the input
  // order from /api/devices (which already orders by trailing bytes).
  devices: Device[]
}

// slugify produces a stable, lowercase, ASCII-only identifier from
// arbitrary sys_location text. Spaces / punctuation become single
// hyphens; non-alphanumerics drop. Used for data-testid and as the
// localStorage key suffix. Empty results fall back to "group" to
// guarantee a non-empty slug for the attribute.
export function slugify(name: string): string {
  const ascii = name
    .normalize('NFKD')
    .replace(/[̀-ͯ]/g, '') // strip combining diacritics from NFKD output
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
  return ascii || 'group'
}

// groupDevices bins the supplied device list by sys_location. Returns
// groups sorted alphabetically by name, with Uncategorized last
// regardless of name. Within a group, devices keep their input order
// — callers rely on the upstream sort (bytes desc).
export function groupDevices(devices: Device[]): DeviceGroup[] {
  const buckets = new Map<string, Device[]>()
  let uncategorized: Device[] | undefined
  for (const d of devices) {
    const norm = normalizeLocation(d.sys_location)
    if (norm === null) {
      if (!uncategorized) uncategorized = []
      uncategorized.push(d)
      continue
    }
    const list = buckets.get(norm)
    if (list) {
      list.push(d)
    } else {
      buckets.set(norm, [d])
    }
  }
  const named: DeviceGroup[] = []
  for (const [name, devs] of buckets) {
    named.push({
      name,
      slug: slugify(name),
      isUncategorized: false,
      devices: devs,
    })
  }
  named.sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }))
  if (uncategorized && uncategorized.length > 0) {
    named.push({
      name: UNCATEGORIZED_LABEL,
      slug: UNCATEGORIZED_SLUG,
      isUncategorized: true,
      devices: uncategorized,
    })
  }
  return named
}

// COLLAPSED_GROUPS_KEY is the localStorage key holding the set of
// group names the operator collapsed. Default is "all expanded" — we
// only persist the collapsed set so a brand-new exporter at a new
// location starts visible.
export const COLLAPSED_GROUPS_KEY = 'flowscope.devices.collapsedGroups'

// loadCollapsedGroups reads the persisted collapsed-group set from
// localStorage. Returns an empty Set on any error (private mode,
// malformed JSON, etc.) — "all expanded" is the safe default.
export function loadCollapsedGroups(): Set<string> {
  try {
    const raw = localStorage.getItem(COLLAPSED_GROUPS_KEY)
    if (!raw) return new Set()
    const arr = JSON.parse(raw)
    if (!Array.isArray(arr)) return new Set()
    return new Set(arr.filter((v): v is string => typeof v === 'string'))
  } catch {
    return new Set()
  }
}

// saveCollapsedGroups persists the collapsed-group set. Best-effort —
// silently swallows storage errors (quota / private mode) since the
// state is purely UI ergonomics.
export function saveCollapsedGroups(set: Set<string>): void {
  try {
    localStorage.setItem(COLLAPSED_GROUPS_KEY, JSON.stringify(Array.from(set)))
  } catch {
    // ignore
  }
}
