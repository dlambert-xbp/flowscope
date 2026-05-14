import type { ReactNode } from 'react'
import type { Filter, FilterKey } from '../filters'

// FilterTrigger wraps any clickable value (an IP, port, ifindex,
// exporter, etc.) so a click pushes the value into the filter chip
// set. Used across Top-N panels, Live tail rows, and the Devices
// interfaces list — every surface that surfaces a filterable value.
//
// keyLabel overrides the default chip key text — e.g. a dst_port
// click from the Top services panel renders as "service · https"
// instead of "dst port · https".
export function FilterTrigger({
  k,
  value,
  label,
  keyLabel,
  onAdd,
  block,
  children,
}: {
  k: FilterKey
  value: string
  label?: string
  keyLabel?: string
  onAdd: (f: Filter) => void
  block?: boolean
  children: ReactNode
}) {
  return (
    <button
      onClick={() => onAdd({ key: k, value, label, keyLabel })}
      className={
        block
          ? 'block w-full max-w-full min-w-0 text-left truncate hover:text-accent hover:underline decoration-dotted underline-offset-2'
          : 'hover:text-accent hover:underline decoration-dotted underline-offset-2'
      }
    >
      {children}
    </button>
  )
}
