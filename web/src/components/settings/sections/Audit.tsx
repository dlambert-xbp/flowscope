import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { api, type AuditEntry } from '../../../api'
import { SectionHeader } from '../Shell'
import { Empty, Field, Section, StyleScope, Tag } from '../shared'

const RESOURCES = [
  '',
  'custom_service',
  'api_token',
  'exporter_allowlist',
  'app_setting',
  'alert_rule_setting',
  'webhook',
  'oidc_config',
  'snmp_credential',
] as const

const ACTIONS = ['', 'create', 'update', 'delete'] as const

export function Audit() {
  const [resource, setResource] = useState<typeof RESOURCES[number]>('')
  const [action, setAction] = useState<typeof ACTIONS[number]>('')
  const [actor, setActor] = useState('')

  const data = useQuery({
    queryKey: ['audit', resource, action, actor],
    queryFn: () => api.listAudit({ resource, action, actor, limit: 200 }),
    refetchInterval: 15_000,
  })

  const [expanded, setExpanded] = useState<string | null>(null)

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · Audit"
        title="Settings change log"
        subtitle="Append-only ledger. Every settings mutation lands here with the before/after row, the actor, and the request id. TTL'd at 365 days."
      />
      <StyleScope />

      <Section eyebrow="Filters">
        <div className="flex flex-wrap items-end gap-3">
          <Field label="resource">
            <select
              value={resource}
              onChange={(e) => setResource(e.target.value as typeof RESOURCES[number])}
              className="s-input"
            >
              {RESOURCES.map((r) => (
                <option key={r} value={r}>{r || 'any'}</option>
              ))}
            </select>
          </Field>
          <Field label="action">
            <select
              value={action}
              onChange={(e) => setAction(e.target.value as typeof ACTIONS[number])}
              className="s-input"
            >
              {ACTIONS.map((a) => (
                <option key={a} value={a}>{a || 'any'}</option>
              ))}
            </select>
          </Field>
          <Field label="actor">
            <input
              value={actor}
              onChange={(e) => setActor(e.target.value)}
              placeholder="any"
              className="s-input"
            />
          </Field>
        </div>
      </Section>

      <Section eyebrow={`Events · ${data.data?.count ?? 0}`}>
        {data.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
        {data.data && data.data.rows.length === 0 && (
          <Empty>no audit events match · try clearing filters</Empty>
        )}
        {data.data && data.data.rows.length > 0 && (
          <div className="border border-line">
            <table className="w-full">
              <thead>
                <tr className="border-b border-line bg-raise">
                  <Th>when</Th>
                  <Th>action</Th>
                  <Th>resource</Th>
                  <Th>target</Th>
                  <Th>actor</Th>
                  <Th>source</Th>
                </tr>
              </thead>
              <tbody>
                {data.data.rows.map((e, i) => (
                  <Row
                    key={i}
                    e={e}
                    expanded={expanded === `${i}`}
                    onToggle={() => setExpanded(expanded === `${i}` ? null : `${i}`)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
    </div>
  )
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th className="text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2">
      {children}
    </th>
  )
}

function Row({ e, expanded, onToggle }: { e: AuditEntry; expanded: boolean; onToggle: () => void }) {
  const tone = e.action === 'delete' ? 'crit' : e.action === 'update' ? 'warn' : 'accent'
  const ts = e.ts.slice(0, 19).replace('T', ' ')
  return (
    <>
      <tr
        className="border-b border-line/60 hover:bg-surface cursor-pointer"
        onClick={onToggle}
      >
        <td className="px-3 py-1.5 text-[11.5px] font-mono text-faint">{ts}</td>
        <td className="px-3 py-1.5"><Tag tone={tone}>{e.action}</Tag></td>
        <td className="px-3 py-1.5 text-[12px] font-mono text-dim">{e.resource}</td>
        <td className="px-3 py-1.5 text-[12px] font-mono text-text truncate max-w-[34ch]" title={e.target}>{e.target}</td>
        <td className="px-3 py-1.5 text-[12px] text-text">{e.actor}</td>
        <td className="px-3 py-1.5 text-[11px] font-mono text-faint">{e.source_ip}</td>
      </tr>
      {expanded && (
        <tr>
          <td colSpan={6} className="bg-raise px-4 py-3 text-[11.5px] font-mono">
            {e.before_json && (
              <div className="mb-2">
                <div className="text-faint mb-1">before</div>
                <pre className="bg-ink border border-line px-2 py-1 overflow-x-auto whitespace-pre-wrap break-all">{prettyJSON(e.before_json)}</pre>
              </div>
            )}
            {e.after_json && (
              <div>
                <div className="text-faint mb-1">after</div>
                <pre className="bg-ink border border-line px-2 py-1 overflow-x-auto whitespace-pre-wrap break-all">{prettyJSON(e.after_json)}</pre>
              </div>
            )}
            {e.request_id && (
              <div className="mt-2 text-faint">request_id · {e.request_id}</div>
            )}
          </td>
        </tr>
      )}
    </>
  )
}

function prettyJSON(s: string): string {
  if (!s) return ''
  try {
    return JSON.stringify(JSON.parse(s), null, 2)
  } catch {
    return s
  }
}
