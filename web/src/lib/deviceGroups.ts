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

/* ------------------------- Hierarchical paths -------------------------- */

// PATH_KEY_SEP joins a path's segments into a single string used as the
// localStorage entry and React key for a folder. U+203A "single
// right-pointing angle quotation mark" is chosen specifically because
// it cannot collide with any of the recognised user-facing delimiters
// (`/`, ` > `, ` : `, ` :: `, ` - `, `, `) — so even a location string
// that happens to contain " > " round-trips unambiguously through the
// persisted set.
export const PATH_KEY_SEP = ' › '

// DELIMITERS lists path separators to try, in priority order. The
// first delimiter that splits the trimmed location into ≥ 2 non-empty
// segments wins. Ordering matters:
//
//   - ` > ` and ` / ` (with spaces) are the most unambiguous "I meant
//     hierarchy" signals.
//   - `/` (no spaces) catches `Troy/DC/Row A/Rack 12` — the canonical
//     pollerless-shop convention — but we guard against leading slashes
//     so a path-like `/usr/local` doesn't shed its leading segment as
//     an empty.
//   - ` :: ` then ` : ` cover Cisco-style "Site :: Building :: Floor".
//   - ` - ` (with spaces) is intentionally space-padded so a hyphenated
//     name like `BPO-Edge-01` stays a single segment.
//   - `, ` (with trailing space) catches `Austin DC, Row B, Rack 5`.
const DELIMITERS: ReadonlyArray<string> = [
  ' > ',
  ' / ',
  '/',
  ' :: ',
  ' : ',
  ' - ',
  ', ',
]

// parseLocationPath splits a sys_location into an ordered hierarchy of
// path segments. Empty / placeholder values yield `[]` (the
// Uncategorized bucket); no recognised delimiter yields a single-element
// array, which preserves PR #53's flat-grouping behaviour bit-for-bit.
//
// Delimiter precedence is fixed (see DELIMITERS): the first delimiter
// that produces ≥ 2 non-empty segments wins. Leading/trailing empty
// segments (e.g. from `/Troy/DC`) are dropped, so `/Troy/DC` resolves
// to `['Troy', 'DC']` rather than `['', 'Troy', 'DC']`.
//
// Mixed-delimiter strings resolve by priority order: a string like
// `Troy/DC - Row A` produces `['Troy', 'DC - Row A']` (the bare `/`
// matches before ` - ` in the priority list). This is a deliberate
// tradeoff — the alternative ("longest delimiter wins" or "user's
// preferred separator") would require operator config, which we don't
// have. Most real-world strings use a single consistent delimiter, so
// priority-order works well; mixed-delimiter strings are edge cases.
export function parseLocationPath(loc: string | null | undefined): string[] {
  if (isPlaceholderLocation(loc)) return []
  const trimmed = (loc as string).trim()
  // First pass: any delimiter that yields ≥ 2 non-empty segments wins.
  for (const delim of DELIMITERS) {
    const parts = trimmed.split(delim).map((s) => s.trim()).filter((s) => s.length > 0)
    if (parts.length >= 2) return parts
  }
  // Second pass: a delimiter that yields EXACTLY 1 non-empty segment
  // is still treated as having "structurally matched" — e.g. `/Troy`
  // (single segment after dropping the leading empty) should resolve
  // to `['Troy']`, not `['/Troy']`. We only re-scan against `/`
  // because it's the only zero-padding delimiter where a leading
  // empty-segment artefact is common in real input. The padded
  // delimiters (` > `, ` / `, ` :: `, …) can't strand themselves at
  // the start of a non-empty string.
  const slashParts = trimmed.split('/').map((s) => s.trim()).filter((s) => s.length > 0)
  if (trimmed.includes('/') && slashParts.length === 1) {
    return slashParts
  }
  // No delimiter matched. Return as a single segment — same bucket
  // the flat groupDevices() would produce.
  return [trimmed]
}

// pathKey joins a path array into the stable string used as
// localStorage entry, React key, and Set membership token. Inverse of
// (path → key); the original delimiter is lost on the way in (that's
// fine — we only need stable equality, not round-trip).
export function pathKey(path: ReadonlyArray<string>): string {
  return path.join(PATH_KEY_SEP)
}

// pathSlug produces a data-testid-safe slug from a full path. Each
// segment is slugified independently and joined with `-` so the slug
// reads naturally (`troy-dc-row-a`) and stays stable even when a
// segment contains characters that would otherwise collapse to the
// same slug as another path.
export function pathSlug(path: ReadonlyArray<string>): string {
  if (path.length === 0) return UNCATEGORIZED_SLUG
  return path.map((s) => slugify(s)).join('-')
}

// FolderNode is one node in the location tree. Leaves carry the devices
// physically at that path; interior nodes carry only children but their
// totalCount aggregates the entire subtree.
export type FolderNode = {
  // name is the last segment of `path`, used as the display label.
  name: string
  // path is the full ancestor chain ending at this node. Top-level
  // folders have a single-element path; depth = path.length - 1.
  path: string[]
  // key is the stable PATH_KEY_SEP-joined string. Suitable as a React
  // key and as a localStorage entry.
  key: string
  // slug is the dash-joined per-segment slug. Used for data-testid.
  slug: string
  // depth is path.length - 1. Top-level folders are depth 0.
  depth: number
  // children are the sub-folders under this node, ordered alphabetically.
  children: FolderNode[]
  // devices are the exporters whose parsed path ends EXACTLY at this
  // node. A parent folder may have its own devices when the dataset
  // mixes `Troy` and `Troy/DC` — the former lands here, not as a child.
  devices: Device[]
  // totalCount is the aggregate device count for this entire subtree
  // (this.devices + sum(children.totalCount)).
  totalCount: number
  // isUncategorized flags the catch-all bucket so callers can sort it
  // to the bottom. Always false for nested folders.
  isUncategorized: boolean
}

// LocationTree is the result of grouping devices hierarchically: a
// list of top-level folder roots plus an optional Uncategorized bucket
// at the end (kept as a separate field so the renderer can pin it).
export type LocationTree = {
  // roots are the top-level (depth-0) folders, sorted alphabetically.
  roots: FolderNode[]
  // uncategorized is the flat catch-all bucket of placeholder-location
  // devices, or null if no such devices exist.
  uncategorized: FolderNode | null
}

// groupDevicesTree builds a hierarchical folder tree from the supplied
// devices. Devices with placeholder / blank sys_location land in the
// dedicated Uncategorized bucket; the rest are placed at the leaf of
// their parsed path. Intermediate folders are synthesised on demand.
//
// totalCount is computed in a second pass after the tree is built, so
// callers can read it without recursing.
export function groupDevicesTree(devices: Device[]): LocationTree {
  let uncategorizedDevices: Device[] | undefined

  // pathStr → mutable node. We reuse this map both to dedupe siblings
  // and to look up intermediate ancestors as we descend.
  const byKey = new Map<string, FolderNode>()
  const roots: FolderNode[] = []

  const ensureNode = (path: string[]): FolderNode => {
    const key = pathKey(path)
    const existing = byKey.get(key)
    if (existing) return existing
    const node: FolderNode = {
      name: path[path.length - 1] ?? '',
      path: path.slice(),
      key,
      slug: pathSlug(path),
      depth: path.length - 1,
      children: [],
      devices: [],
      totalCount: 0,
      isUncategorized: false,
    }
    byKey.set(key, node)
    if (path.length === 1) {
      roots.push(node)
    } else {
      const parent = ensureNode(path.slice(0, -1))
      parent.children.push(node)
    }
    return node
  }

  for (const d of devices) {
    const path = parseLocationPath(d.sys_location)
    if (path.length === 0) {
      if (!uncategorizedDevices) uncategorizedDevices = []
      uncategorizedDevices.push(d)
      continue
    }
    const leaf = ensureNode(path)
    leaf.devices.push(d)
  }

  // Sort siblings alphabetically (case-insensitive). Recurse to sort
  // every depth in one pass.
  const sortRec = (nodes: FolderNode[]) => {
    nodes.sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }))
    for (const n of nodes) sortRec(n.children)
  }
  sortRec(roots)

  // Compute totalCount bottom-up.
  const aggregate = (n: FolderNode): number => {
    let sum = n.devices.length
    for (const c of n.children) sum += aggregate(c)
    n.totalCount = sum
    return sum
  }
  for (const r of roots) aggregate(r)

  let uncategorized: FolderNode | null = null
  if (uncategorizedDevices && uncategorizedDevices.length > 0) {
    uncategorized = {
      name: UNCATEGORIZED_LABEL,
      path: [UNCATEGORIZED_LABEL],
      key: UNCATEGORIZED_LABEL,
      slug: UNCATEGORIZED_SLUG,
      depth: 0,
      children: [],
      devices: uncategorizedDevices,
      totalCount: uncategorizedDevices.length,
      isUncategorized: true,
    }
  }

  return { roots, uncategorized }
}

// findAncestorKeys returns the set of folder keys on the ancestor
// chain (inclusive of the leaf) of the folder that physically contains
// the supplied exporter. Used to auto-expand a path on mount when the
// operator has a selected device in a deeply nested folder.
//
// Returns an empty set when the exporter isn't found in the tree (e.g.
// pre-load, or a stale URL pointing at a deleted device).
export function findAncestorKeys(tree: LocationTree, exporter: string): Set<string> {
  const out = new Set<string>()
  const walk = (node: FolderNode, chain: string[]): boolean => {
    const nextChain = [...chain, node.key]
    if (node.devices.some((d) => d.exporter === exporter)) {
      for (const k of nextChain) out.add(k)
      return true
    }
    for (const c of node.children) {
      if (walk(c, nextChain)) return true
    }
    return false
  }
  for (const r of tree.roots) {
    if (walk(r, [])) return out
  }
  if (tree.uncategorized) {
    if (walk(tree.uncategorized, [])) return out
  }
  return out
}

// collectFolderKeys flattens every folder key in the tree into a set,
// optionally filtered to depth ≤ maxDepth (0-indexed; pass 0 to get
// only top-level keys). Useful for the "default: top-level expanded,
// deeper levels collapsed" policy — pass maxDepth = 0 to collapse
// everything below the roots.
export function collectFolderKeys(tree: LocationTree, minDepth = 0): Set<string> {
  const out = new Set<string>()
  const walk = (n: FolderNode) => {
    if (n.depth >= minDepth) out.add(n.key)
    for (const c of n.children) walk(c)
  }
  for (const r of tree.roots) walk(r)
  if (tree.uncategorized) walk(tree.uncategorized)
  return out
}

// findMatchingFolderKeys returns the set of every folder key on the
// ancestor chain of every device whose row matches the supplied
// case-insensitive needle. Used to force-expand matching paths when a
// filter query is active. An empty / whitespace-only needle yields an
// empty set (the caller should suppress force-expand in that case).
export function findMatchingFolderKeys(
  tree: LocationTree,
  needle: string,
): Set<string> {
  const out = new Set<string>()
  const n = needle.trim().toLowerCase()
  if (n === '') return out
  const matches = (d: Device): boolean => {
    if (d.exporter.toLowerCase().includes(n)) return true
    if (d.sys_name && d.sys_name.toLowerCase().includes(n)) return true
    if (d.sys_location && d.sys_location.toLowerCase().includes(n)) return true
    return false
  }
  const walk = (node: FolderNode, chain: string[]): void => {
    const nextChain = [...chain, node.key]
    if (node.devices.some(matches)) {
      for (const k of nextChain) out.add(k)
    }
    for (const c of node.children) walk(c, nextChain)
  }
  for (const r of tree.roots) walk(r, [])
  if (tree.uncategorized) walk(tree.uncategorized, [])
  return out
}
