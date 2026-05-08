import type { ReactNode } from 'react'

// Field is the standard form-row primitive used across every Settings
// section: small uppercase mono label + child input. Keeps the visual
// language unified and lets one tweak (e.g. label tracking) ripple
// everywhere.
export function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <label className="block">
      <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-mono font-semibold block mb-1">
        {label}
      </span>
      {children}
      {hint && (
        <span className="block text-[11px] text-faint mt-1 leading-[1.4]">{hint}</span>
      )}
    </label>
  )
}

// Section is a card-like grouping under a SectionHeader. Sections
// stack vertically; the eyebrow on the left identifies the topic.
// Single horizontal rule under the title row — no outer section
// border, otherwise sections read as double-walled boxes.
export function Section({
  eyebrow,
  hint,
  actions,
  children,
}: {
  eyebrow: string
  hint?: string
  actions?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="px-6 py-5">
      <div className="flex items-baseline gap-3 pb-3 border-b border-line mb-4">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          {eyebrow}
        </span>
        {hint && (
          <span className="font-mono text-[11px] text-faint">{hint}</span>
        )}
        {actions && <span className="ml-auto flex items-center gap-2">{actions}</span>}
      </div>
      {children}
    </section>
  )
}

// Button is a small uppercase-mono action used everywhere settings
// need a button. tone='ghost' is the default low-key form; 'accent'
// is the affirmative action; 'crit' is destructive.
export function Btn({
  tone = 'ghost',
  size = 'sm',
  onClick,
  disabled,
  children,
  type = 'button',
  title,
}: {
  tone?: 'ghost' | 'accent' | 'crit' | 'warn'
  size?: 'sm' | 'md'
  onClick?: () => void
  disabled?: boolean
  children: ReactNode
  type?: 'button' | 'submit'
  title?: string
}) {
  const toneClass =
    tone === 'accent' ? 'border-accent text-accent hover:bg-accent-wash'
      : tone === 'crit' ? 'border-crit text-crit hover:bg-crit/10'
        : tone === 'warn' ? 'border-warn text-warn hover:bg-warn/10'
          : 'border-line text-dim hover:text-text hover:border-dim'
  const padding = size === 'md' ? 'px-3 py-1.5' : 'px-2.5 py-1'
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={`font-mono text-[11px] uppercase tracking-[0.06em] border ${toneClass} ${padding} disabled:opacity-50 disabled:cursor-not-allowed`}
    >
      {children}
    </button>
  )
}

// SECTIONS each include a single CSS injection of `.s-input` so the
// styling is co-located. Importing this once at the shell root would
// be cleaner long-term — for now the duplication is honest.
export const inputCSS = `
.s-input {
  background: var(--color-ink);
  border: 1px solid var(--color-line);
  padding: 6px 8px;
  font-family: var(--font-mono);
  font-size: 12.5px;
  color: var(--color-text);
  outline: none;
  width: 100%;
}
.s-input:focus { border-color: var(--color-accent); }
.s-input:disabled { color: var(--color-dim); cursor: not-allowed; }
.s-input--narrow { width: auto; min-width: 7ch; }
.s-tag {
  font-family: var(--font-mono);
  font-size: 10.5px;
  letter-spacing: 0.04em;
  padding: 1px 5px;
  border: 1px solid var(--color-line);
  color: var(--color-dim);
  text-transform: uppercase;
}
.s-tag--accent { color: var(--color-accent); border-color: var(--color-accent); background: var(--color-accent-wash); }
.s-tag--warn { color: var(--color-warn); border-color: var(--color-warn); background: var(--color-warn-wash); }
.s-tag--crit { color: var(--color-crit); border-color: var(--color-crit); background: var(--color-crit-wash); }
.s-tag--ok { color: var(--color-ok); border-color: var(--color-ok); }
`

export function StyleScope() {
  return <style>{inputCSS}</style>
}

// Tag is a small metadata pill used in lists.
export function Tag({
  tone,
  children,
  title,
}: {
  tone?: 'accent' | 'warn' | 'crit' | 'ok'
  children: ReactNode
  title?: string
}) {
  const cls = tone ? `s-tag s-tag--${tone}` : 's-tag'
  return (
    <span className={cls} title={title}>
      {children}
    </span>
  )
}

// Empty is the standard "nothing to show" treatment. Plain dim
// mono prose — matches the Devices/Overview empty states.
export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="py-6 text-center text-[12.5px] font-mono text-dim">
      {children}
    </div>
  )
}

// Banner is the high-visibility callout used for "auth disabled",
// "Phase 2 not active", "secrets disabled" notes. Body copy reads
// in text-dim so the wash + the toned <strong> lead carry the
// emphasis instead of competing with the prose.
export function Banner({
  tone = 'warn',
  children,
}: {
  tone?: 'warn' | 'crit' | 'accent'
  children: ReactNode
}) {
  const map = {
    warn:   'border-warn/40 bg-warn-wash',
    crit:   'border-crit/40 bg-crit-wash',
    accent: 'border-accent/40 bg-accent-wash',
  } as const
  return (
    <div className={`border ${map[tone]} px-4 py-3 mb-4 text-[12.5px] text-dim leading-[1.5]`}>
      {children}
    </div>
  )
}

// EditForm is the inline create/edit panel used by every list-with-
// CRUD section. Subtle bg-raise with an accent left rule, instead of
// the full accent-wash treatment that would compete with Banner.
export function EditForm({
  title,
  onCancel,
  children,
}: {
  title: string
  onCancel: () => void
  children: ReactNode
}) {
  return (
    <div
      className="bg-raise border border-line border-l-2 px-4 py-4 mb-4"
      style={{ borderLeftColor: 'var(--color-accent)' }}
    >
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {title}
        </span>
        <button
          onClick={onCancel}
          className="ml-auto font-mono text-[11px] text-dim hover:text-text"
        >
          cancel
        </button>
      </div>
      {children}
    </div>
  )
}
