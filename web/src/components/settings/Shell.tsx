import { useEffect, useState, type ReactElement, type ReactNode } from 'react'
import { General } from './sections/General'
import { Services } from './sections/Services'
import { Exporters } from './sections/Exporters'
import { SNMP } from './sections/SNMP'
import { AlertsTuning } from './sections/AlertsTuning'
import { AlertRules } from './sections/AlertRules'
import { Integrations } from './sections/Integrations'
import { AuthTokens } from './sections/AuthTokens'
import { Audit } from './sections/Audit'
import { Advanced } from './sections/Advanced'

// SectionId is the URL-addressable identifier for each panel under
// the Settings tab. The default section is 'general' — that's what
// most operators land here looking for.
export type SectionId =
  | 'general'
  | 'services'
  | 'exporters'
  | 'snmp'
  | 'rules'
  | 'alerts'
  | 'integrations'
  | 'auth'
  | 'audit'
  | 'advanced'

type SectionDef = {
  id: SectionId
  label: string
  group: 'core' | 'connectivity' | 'alerting' | 'admin'
  Component: () => ReactElement
}

const SECTIONS: SectionDef[] = [
  { id: 'general',      label: 'General',       group: 'core',         Component: General },
  { id: 'services',     label: 'Services',      group: 'core',         Component: Services },
  { id: 'exporters',    label: 'Exporters',     group: 'connectivity', Component: Exporters },
  { id: 'snmp',         label: 'SNMP',          group: 'connectivity', Component: SNMP },
  { id: 'rules',        label: 'Per-device rules', group: 'alerting',  Component: AlertRules },
  { id: 'alerts',       label: 'Defaults',      group: 'alerting',     Component: AlertsTuning },
  { id: 'integrations', label: 'Integrations',  group: 'alerting',     Component: Integrations },
  { id: 'auth',         label: 'Auth & tokens', group: 'admin',        Component: AuthTokens },
  { id: 'audit',        label: 'Audit log',     group: 'admin',        Component: Audit },
  { id: 'advanced',     label: 'Advanced',      group: 'admin',        Component: Advanced },
]

const VALID_IDS = new Set<SectionId>(SECTIONS.map((s) => s.id))

// useSection reads / writes the section id in the URL. Same pattern
// as useTimeRange — direct URLSearchParams manipulation, no router.
function useSection(): [SectionId, (id: SectionId) => void] {
  const read = (): SectionId => {
    const v = new URLSearchParams(window.location.search).get('s')
    return v && VALID_IDS.has(v as SectionId) ? (v as SectionId) : 'general'
  }
  const [section, setSection] = useState<SectionId>(read)
  useEffect(() => {
    const onPop = () => setSection(read())
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [])
  const set = (id: SectionId) => {
    setSection(id)
    const url = new URL(window.location.href)
    url.searchParams.set('s', id)
    // Drop any per-section item state when switching sections.
    url.searchParams.delete('item')
    window.history.replaceState({}, '', url.toString())
  }
  return [section, set]
}

export function SettingsShell() {
  const [section, setSection] = useSection()
  const Active = SECTIONS.find((s) => s.id === section)?.Component ?? General

  return (
    <div className="grid h-full" style={{ gridTemplateColumns: '240px 1fr' }}>
      <Rail current={section} onSelect={setSection} />
      <div className="overflow-auto">
        <Active />
      </div>
    </div>
  )
}

function Rail({
  current,
  onSelect,
}: {
  current: SectionId
  onSelect: (s: SectionId) => void
}) {
  const groups: Record<SectionDef['group'], { label: string; items: SectionDef[] }> = {
    core:         { label: 'Core',         items: [] },
    connectivity: { label: 'Connectivity', items: [] },
    alerting:     { label: 'Alerting',     items: [] },
    admin:        { label: 'Administration', items: [] },
  }
  for (const s of SECTIONS) groups[s.group].items.push(s)

  return (
    <nav
      aria-label="Settings sections"
      className="border-r border-line bg-surface overflow-auto"
    >
      <div className="px-4 pt-5 pb-4 border-b border-line/60">
        <div className="text-[10.5px] uppercase tracking-[0.1em] font-semibold text-faint">
          Settings
        </div>
        <div className="text-[16px] font-semibold text-text mt-0.5">
          Administration
        </div>
      </div>
      {Object.entries(groups).map(([key, g]) => (
        <div key={key} className="px-2 mt-3">
          <div className="px-2 pt-2 pb-1 text-[10px] uppercase tracking-[0.12em] text-faint font-mono">
            {g.label}
          </div>
          {g.items.map((s) => (
            <RailItem
              key={s.id}
              active={current === s.id}
              onClick={() => onSelect(s.id)}
              label={s.label}
              sectionId={s.id}
            />
          ))}
        </div>
      ))}
    </nav>
  )
}

function RailItem({
  active,
  onClick,
  label,
  sectionId,
}: {
  active: boolean
  onClick: () => void
  label: string
  sectionId: SectionId
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-current={active ? 'page' : undefined}
      data-testid={`settings-section-${sectionId}`}
      className={`w-full text-left px-3 py-1.5 border-l-2 transition-colors text-[13px] ${
        active
          ? 'border-accent bg-accent-wash text-text'
          : 'border-transparent text-dim hover:bg-raise hover:text-text'
      }`}
    >
      {label}
    </button>
  )
}

/* ----------------------------- Page header used by all sections ----------------------------- */

export function SectionHeader({
  eyebrow,
  title,
  subtitle,
  actions,
}: {
  eyebrow: string
  title: string
  subtitle?: ReactNode
  actions?: ReactNode
}) {
  return (
    <div className="px-6 pt-6 pb-4 border-b border-line bg-surface flex items-start gap-4">
      <div className="flex-1 min-w-0">
        <div className="text-[10.5px] uppercase tracking-[0.1em] font-semibold text-faint mb-1">
          {eyebrow}
        </div>
        <h1 className="text-[22px] font-semibold tracking-tight text-text leading-[1.2]">
          {title}
        </h1>
        {subtitle && (
          <p className="text-[13px] text-dim mt-1.5 max-w-[78ch] leading-[1.5]">
            {subtitle}
          </p>
        )}
      </div>
      {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
    </div>
  )
}
