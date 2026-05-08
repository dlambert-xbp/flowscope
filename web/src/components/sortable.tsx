import { useMemo, useState, type ReactNode } from 'react'

export type SortDir = 'asc' | 'desc'
export type SortGetter<T> = (r: T) => number | string | null | undefined
export type SortColumns<T> = Record<string, SortGetter<T>>

export function useTableSort<T>(
  rows: T[],
  columns: SortColumns<T>,
  initial?: { key: string; dir?: SortDir },
) {
  const [sortKey, setSortKey] = useState<string | null>(initial?.key ?? null)
  const [sortDir, setSortDir] = useState<SortDir>(initial?.dir ?? 'desc')

  const toggle = (key: string) => {
    if (key === sortKey) {
      setSortDir((d) => (d === 'asc' ? 'desc' : 'asc'))
      return
    }
    setSortKey(key)
    const sample = rows.length ? columns[key]?.(rows[0]) : null
    setSortDir(typeof sample === 'number' ? 'desc' : 'asc')
  }

  const sortedRows = useMemo(() => {
    if (!sortKey) return rows
    const get = columns[sortKey]
    if (!get) return rows
    const sign = sortDir === 'asc' ? 1 : -1
    return [...rows].sort((a, b) => {
      const av = get(a)
      const bv = get(b)
      if (av == null && bv == null) return 0
      if (av == null) return 1
      if (bv == null) return -1
      if (typeof av === 'number' && typeof bv === 'number') return (av - bv) * sign
      return String(av).localeCompare(String(bv), undefined, { numeric: true }) * sign
    })
  }, [rows, sortKey, sortDir, columns])

  return { sortedRows, sortKey, sortDir, toggle }
}

export function Th({
  sortKey,
  active,
  dir,
  onToggle,
  align,
  children,
}: {
  sortKey?: string
  active?: boolean
  dir?: SortDir
  onToggle?: (k: string) => void
  align?: 'r'
  children?: ReactNode
}) {
  const sortable = sortKey != null && onToggle != null
  const className = align === 'r' ? 'r' : undefined
  if (!sortable) return <th className={className}>{children}</th>
  return (
    <th className={className}>
      <button
        type="button"
        onClick={() => onToggle!(sortKey!)}
        className={`th-sort ${align === 'r' ? 'th-sort-r' : ''} ${active ? 'is-active' : ''}`}
      >
        <span>{children}</span>
        <span className="th-arrow" aria-hidden>
          {active ? (dir === 'asc' ? '▲' : '▼') : '↕'}
        </span>
      </button>
    </th>
  )
}
