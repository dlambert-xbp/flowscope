import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, type Webhook } from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'

const KINDS: Webhook['kind'][] = ['slack', 'teams', 'pagerduty', 'http']
const SEVERITIES = ['critical', 'warning', 'info']

export function Integrations() {
  const list = useQuery({
    queryKey: ['webhooks'],
    queryFn: () => api.listWebhooks(),
  })
  const [editing, setEditing] = useState<Webhook | 'new' | null>(null)

  return (
    <div>
      <SectionHeader
        eyebrow="Integrations"
        title="Outbound webhooks"
        subtitle="Where the alert engine posts state transitions. One row per channel; severity filter chooses which alerts reach it."
        actions={
          <Btn tone="accent" size="md" onClick={() => setEditing('new')}>
            + webhook
          </Btn>
        }
      />
      <StyleScope />
      <Section eyebrow={`Webhooks · ${list.data?.count ?? 0}`} hint="secrets sealed under FLOWSCOPE_SNMP_KEY">
        {editing && (
          <WebhookForm
            initial={editing === 'new' ? null : editing}
            onClose={() => setEditing(null)}
          />
        )}
        {list.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
        {list.data && list.data.rows.length === 0 && !editing && (
          <Empty>no integrations · click "new webhook" to wire one</Empty>
        )}
        {list.data && list.data.rows.length > 0 && (
          <div className="border border-line">
            <table className="w-full">
              <thead>
                <tr className="border-b border-line bg-raise">
                  <Th>name</Th>
                  <Th>kind</Th>
                  <Th>url</Th>
                  <Th>filter</Th>
                  <Th>state</Th>
                  <Th />
                </tr>
              </thead>
              <tbody>
                {list.data.rows.map((w) => (
                  <Row key={w.id} w={w} onEdit={() => setEditing(w)} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Section>
    </div>
  )
}

function Th({ children }: { children?: React.ReactNode }) {
  return (
    <th className="text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2">
      {children}
    </th>
  )
}

function Row({ w, onEdit }: { w: Webhook; onEdit: () => void }) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteWebhook(w.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['webhooks'] }),
  })
  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] text-text">{w.name}</td>
      <td className="px-3 py-1.5"><Tag tone="accent">{w.kind}</Tag></td>
      <td className="px-3 py-1.5 text-[12px] font-mono text-dim truncate max-w-[40ch]" title={w.url}>{w.url}</td>
      <td className="px-3 py-1.5 text-[12px] font-mono text-faint">
        {(w.severity_filter ?? []).join(' · ') || 'all'}
      </td>
      <td className="px-3 py-1.5">
        {w.enabled ? <Tag tone="ok">enabled</Tag> : <Tag tone="warn">disabled</Tag>}
      </td>
      <td className="px-3 py-1.5">
        <div className="flex justify-end gap-2">
          <Btn onClick={onEdit}>edit</Btn>
          <Btn
            tone="crit"
            onClick={async () => {
              const ok = await confirm({
                title: `Delete webhook "${w.name}"?`,
                confirmLabel: 'Delete',
                tone: 'crit',
              })
              if (ok) del.mutate()
            }}
          >
            delete
          </Btn>
        </div>
      </td>
    </tr>
  )
}

function WebhookForm({ initial, onClose }: { initial: Webhook | null; onClose: () => void }) {
  const isEdit = !!initial
  const [w, setW] = useState<Partial<Webhook>>(
    initial ?? {
      name: '',
      kind: 'slack',
      url: '',
      enabled: true,
      severity_filter: ['critical', 'warning'],
      header_template: {},
    },
  )
  const [error, setError] = useState<string | null>(null)
  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: () => api.putWebhook(w),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['webhooks'] })
      onClose()
    },
    onError: (err: Error) => setError(err.message),
  })

  const toggleSev = (s: string) => {
    const set = new Set(w.severity_filter ?? [])
    set.has(s) ? set.delete(s) : set.add(s)
    setW({ ...w, severity_filter: Array.from(set) })
  }

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit webhook' : 'New webhook'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">cancel</button>
      </div>
      <div className="grid grid-cols-1 md:grid-cols-4 gap-3">
        <Field label="name">
          <input
            value={w.name ?? ''}
            onChange={(e) => setW({ ...w, name: e.target.value })}
            placeholder="netops-slack"
            className="s-input"
          />
        </Field>
        <Field label="kind">
          <select
            value={w.kind ?? 'slack'}
            onChange={(e) => setW({ ...w, kind: e.target.value as Webhook['kind'] })}
            className="s-input"
          >
            {KINDS.map((k) => <option key={k} value={k}>{k}</option>)}
          </select>
        </Field>
        <div className="md:col-span-2">
          <Field label="url" hint={kindHint(w.kind ?? 'slack')}>
            <input
              value={w.url ?? ''}
              onChange={(e) => setW({ ...w, url: e.target.value })}
              placeholder="https://hooks.slack.com/services/T…/B…/…"
              className="s-input"
            />
          </Field>
        </div>
        <Field label={`secret ${w.has_secret ? '· (set; blank = preserve)' : ''}`}>
          <input
            type="password"
            value={w.secret ?? ''}
            onChange={(e) => setW({ ...w, secret: e.target.value })}
            placeholder={w.has_secret ? '••••••••' : '(optional)'}
            className="s-input"
          />
        </Field>
        <Field label="enabled">
          <select
            value={w.enabled ? 'on' : 'off'}
            onChange={(e) => setW({ ...w, enabled: e.target.value === 'on' })}
            className="s-input"
          >
            <option value="on">enabled</option>
            <option value="off">disabled</option>
          </select>
        </Field>
        <div className="md:col-span-2">
          <Field label="severity filter" hint="alerts at any selected severity reach this channel">
            <div className="flex items-center gap-2">
              {SEVERITIES.map((s) => {
                const active = (w.severity_filter ?? []).includes(s)
                return (
                  <button
                    key={s}
                    type="button"
                    onClick={() => toggleSev(s)}
                    className={`px-2.5 py-1 font-mono text-[11px] uppercase border ${
                      active
                        ? `${s === 'critical' ? 's-tag s-tag--crit' : s === 'warning' ? 's-tag s-tag--warn' : 's-tag s-tag--accent'}`
                        : 'border-line text-dim hover:text-text'
                    }`}
                  >
                    {s}
                  </button>
                )
              })}
            </div>
          </Field>
        </div>
      </div>

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <Btn tone="accent" size="md" disabled={save.isPending || !w.name || !w.url} onClick={() => { setError(null); save.mutate() }}>
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
        <Btn size="md" onClick={onClose}>cancel</Btn>
        {(w.kind === 'slack' || w.kind === 'teams') && (
          <span className="ml-auto font-mono text-[10.5px] text-faint">
            slack / teams just need the incoming-webhook URL · no secret
          </span>
        )}
      </div>
    </div>
  )
}

function kindHint(kind: Webhook['kind']): string {
  switch (kind) {
    case 'slack': return 'incoming-webhook URL from your Slack app'
    case 'teams': return 'connector URL from the Microsoft Teams channel'
    case 'pagerduty': return 'PagerDuty Events API v2 endpoint · routing key in secret'
    case 'http': return 'arbitrary HTTPS endpoint · use header_template for auth'
    default: return ''
  }
}
