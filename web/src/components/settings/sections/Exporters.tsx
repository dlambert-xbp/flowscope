import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, type ExporterAllowlistEntry } from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Banner, Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'

// Exporters: opt-in allowlist. Empty (zero rows) = ingest accepts
// every source. Adding any row flips the service to deny-by-default
// — the UI surfaces that flip explicitly so an operator doesn't
// silently break a working setup.

export function Exporters() {
  const list = useQuery({
    queryKey: ['allowlist'],
    queryFn: () => api.listAllowlist(),
  })
  const [editing, setEditing] = useState<ExporterAllowlistEntry | 'new' | null>(null)

  const isFirst = (list.data?.count ?? 0) === 0
  const acceptAll = isFirst

  return (
    <div>
      <SectionHeader
        eyebrow="Settings · Exporters"
        title="Exporter allowlist"
        subtitle={
          acceptAll
            ? 'No allowlist configured. The ingest service accepts flows from every source. Adding the first entry switches to deny-by-default.'
            : 'Only sources on this list are ingested. Disable a row to mute a device during maintenance without losing its label.'
        }
        actions={
          <Btn tone="accent" size="md" onClick={() => setEditing('new')}>
            + add exporter
          </Btn>
        }
      />
      <StyleScope />

      {acceptAll && (
        <div className="px-6 pt-4">
          <Banner tone="warn">
            <strong className="text-warn">Permissive mode.</strong> Anyone who can
            send a NetFlow/sFlow/IPFIX packet to your collectors will be ingested.
            Production deployments should add at least one explicit entry.
          </Banner>
        </div>
      )}

      <Section eyebrow={`Allowlist · ${list.data?.count ?? 0}`} hint="enabled rows are ingested · disabled rows are dropped at the listener">
        {editing && (
          <AllowlistForm
            initial={editing === 'new' ? null : editing}
            onClose={() => setEditing(null)}
          />
        )}
        {list.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
        {list.data && list.data.rows.length === 0 && !editing && (
          <Empty>no entries · click "add exporter" to start</Empty>
        )}
        {list.data && list.data.rows.length > 0 && (
          <div className="border border-line">
            <table className="w-full">
              <thead>
                <tr className="border-b border-line bg-raise">
                  <Th>exporter</Th>
                  <Th>label</Th>
                  <Th>state</Th>
                  <Th>notes</Th>
                  <Th>updated</Th>
                  <Th />
                </tr>
              </thead>
              <tbody>
                {list.data.rows.map((e) => (
                  <Row key={e.exporter} e={e} onEdit={() => setEditing(e)} />
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

function Row({ e, onEdit }: { e: ExporterAllowlistEntry; onEdit: () => void }) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteAllowlist(e.exporter),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['allowlist'] }),
  })
  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] font-mono text-text">{e.exporter}</td>
      <td className="px-3 py-1.5 text-[12.5px] text-text">{e.label || <span className="text-faint">—</span>}</td>
      <td className="px-3 py-1.5">
        {e.enabled ? <Tag tone="ok">enabled</Tag> : <Tag tone="warn">disabled</Tag>}
      </td>
      <td className="px-3 py-1.5 text-[12px] text-faint truncate max-w-[40ch]">{e.notes || '—'}</td>
      <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
        {e.updated_at ? e.updated_at.slice(0, 19).replace('T', ' ') : '—'}
        {e.updated_by ? ` · ${e.updated_by}` : ''}
      </td>
      <td className="px-3 py-1.5">
        <div className="flex justify-end gap-2">
          <Btn onClick={onEdit}>edit</Btn>
          <Btn
            tone="crit"
            onClick={async () => {
              const ok = await confirm({
                title: `Remove ${e.exporter}?`,
                body: 'Future flows from this source will be dropped (or accepted, if this leaves the allowlist empty).',
                confirmLabel: 'Remove',
                tone: 'crit',
              })
              if (ok) del.mutate()
            }}
          >
            remove
          </Btn>
        </div>
      </td>
    </tr>
  )
}

function AllowlistForm({ initial, onClose }: { initial: ExporterAllowlistEntry | null; onClose: () => void }) {
  const isEdit = !!initial
  const [e, setE] = useState<ExporterAllowlistEntry>(
    initial ?? { exporter: '', label: '', enabled: true, notes: '' },
  )
  const [error, setError] = useState<string | null>(null)
  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: () => api.putAllowlist(e),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['allowlist'] })
      onClose()
    },
    onError: (err: Error) => setError(err.message),
  })
  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit exporter' : 'Add exporter'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">cancel</button>
      </div>
      <div className="grid grid-cols-1 md:grid-cols-4 gap-3">
        <Field label="exporter (IPv4 or IPv6)">
          <input
            disabled={isEdit}
            value={e.exporter}
            onChange={(ev) => setE({ ...e, exporter: ev.target.value })}
            placeholder="10.2.0.11"
            className="s-input"
          />
        </Field>
        <Field label="label">
          <input
            value={e.label ?? ''}
            onChange={(ev) => setE({ ...e, label: ev.target.value })}
            placeholder="core-rtr-1"
            className="s-input"
          />
        </Field>
        <Field label="state">
          <select
            value={e.enabled ? 'on' : 'off'}
            onChange={(ev) => setE({ ...e, enabled: ev.target.value === 'on' })}
            className="s-input"
          >
            <option value="on">enabled</option>
            <option value="off">disabled (mute)</option>
          </select>
        </Field>
        <Field label="notes">
          <input
            value={e.notes ?? ''}
            onChange={(ev) => setE({ ...e, notes: ev.target.value })}
            placeholder="DC-A · Cisco N9K"
            className="s-input"
          />
        </Field>
      </div>
      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}
      <div className="flex items-center gap-3 mt-4">
        <Btn tone="accent" size="md" disabled={save.isPending || !e.exporter} onClick={() => { setError(null); save.mutate() }}>
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
        <Btn size="md" onClick={onClose}>cancel</Btn>
      </div>
    </div>
  )
}
