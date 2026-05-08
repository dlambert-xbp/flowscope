import { createContext, useCallback, useContext, useState, type ReactNode } from 'react'
import { Dialog } from './Dialog'

// appConfirm replaces window.confirm() across the app. It returns a
// Promise<boolean> the caller can await; resolves to true on confirm,
// false on cancel or backdrop click.
//
// Usage:
//   const ok = await confirm({
//     title: 'Delete custom service?',
//     body: 'This removes the override; built-in name returns.',
//     confirmLabel: 'Delete',
//     tone: 'crit',
//   })
//   if (ok) doIt()

export type ConfirmOptions = {
  title: string
  body?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  tone?: 'crit' | 'warn' | 'accent'
}

type ConfirmFn = (opts: ConfirmOptions) => Promise<boolean>

const Ctx = createContext<ConfirmFn | null>(null)

// AppConfirmProvider mounts once at the app root. Children call
// useAppConfirm() to obtain the imperative confirm function.
export function AppConfirmProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<{
    opts: ConfirmOptions
    resolve: (v: boolean) => void
  } | null>(null)

  const confirm: ConfirmFn = useCallback((opts) => {
    return new Promise<boolean>((resolve) => {
      setState({ opts, resolve })
    })
  }, [])

  const close = useCallback(
    (v: boolean) => {
      if (!state) return
      state.resolve(v)
      setState(null)
    },
    [state],
  )

  const tone = state?.opts.tone ?? 'accent'
  const toneClass =
    tone === 'crit' ? 'border-crit text-crit hover:bg-crit/10'
      : tone === 'warn' ? 'border-warn text-warn hover:bg-warn/10'
        : 'border-accent text-accent hover:bg-accent-wash'

  return (
    <Ctx.Provider value={confirm}>
      {children}
      <Dialog open={!!state} onClose={() => close(false)} title={state?.opts.title} width={460}>
        {state?.opts.body && (
          <div className="text-[13px] text-text leading-[1.55] mb-4">
            {state.opts.body}
          </div>
        )}
        <div className="flex items-center justify-end gap-2 mt-2">
          <button
            type="button"
            onClick={() => close(false)}
            className="font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1.5 border border-line text-dim hover:text-text"
          >
            {state?.opts.cancelLabel ?? 'cancel'}
          </button>
          <button
            type="button"
            onClick={() => close(true)}
            className={`font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1.5 border ${toneClass}`}
          >
            {state?.opts.confirmLabel ?? 'confirm'}
          </button>
        </div>
      </Dialog>
    </Ctx.Provider>
  )
}

// useAppConfirm returns the imperative confirm function. If called
// outside the provider it falls back to window.confirm so partial
// integrations don't crash, but in normal use the provider should
// always be present.
export function useAppConfirm(): ConfirmFn {
  const ctx = useContext(Ctx)
  if (ctx) return ctx
  return ({ title, body }) =>
    Promise.resolve(window.confirm(`${title}${body ? `\n\n${body}` : ''}`))
}
