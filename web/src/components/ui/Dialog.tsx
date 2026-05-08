import { useEffect, useRef, type ReactNode } from 'react'

// Dialog is the in-app modal primitive. Use this instead of
// window.alert / window.confirm / window.prompt anywhere in the app —
// the visual language of those native dialogs clashes with the
// dashboard, and they trap focus on the OS layer in ways that break
// keyboard tests.
export function Dialog({
  open,
  onClose,
  title,
  children,
  width = 480,
}: {
  open: boolean
  onClose: () => void
  title?: ReactNode
  children: ReactNode
  width?: number
}) {
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    // Move focus into the dialog so Tab cycles inside it.
    ref.current?.focus()
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={typeof title === 'string' ? title : undefined}
      className="fixed inset-0 z-40 flex items-center justify-center"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="absolute inset-0 bg-black/60" />
      <div
        ref={ref}
        tabIndex={-1}
        className="relative bg-surface border border-line shadow-xl outline-none"
        style={{ width, maxWidth: 'calc(100vw - 2rem)' }}
      >
        {title && (
          <div className="px-4 py-3 border-b border-line flex items-baseline gap-3">
            <span className="text-[10.5px] uppercase tracking-[0.18em] font-mono font-semibold text-faint">
              dialog
            </span>
            <span className="text-[14px] font-semibold text-text">{title}</span>
            <button
              type="button"
              onClick={onClose}
              className="ml-auto text-faint hover:text-text font-mono text-[12px]"
              aria-label="close"
            >
              ✕
            </button>
          </div>
        )}
        <div className="px-4 py-4">{children}</div>
      </div>
    </div>
  )
}
