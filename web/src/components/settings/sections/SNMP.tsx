import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, fmt, type SNMPCredential, type SNMPTestResult } from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Banner, Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'
import { useTableSort, type SortColumns, type SortDir } from '../../sortable'

const CRED_COLS: SortColumns<SNMPCredential> = {
  exporter: (r) => r.exporter,
  version: (r) => r.version,
  identity: (r) => (r.version === 'v3' ? r.v3_username || '' : 'community'),
  secrets: (r) => {
    if (r.version === 'v3') {
      const parts = [
        r.has_auth_pass ? 'auth' : null,
        r.has_priv_pass ? 'priv' : null,
      ].filter(Boolean)
      return parts.length ? parts.join(' + ') : 'noAuthNoPriv'
    }
    return r.has_community ? 1 : 0
  },
  port: (r) => r.port,
  interval: (r) => r.interval_sec,
  updated: (r) => r.updated_at || '',
}

// SNMP: per-exporter v2c / v3 credential bindings. Lifted from the
// previous single-page Settings.tsx; same backend endpoints, new
// shell, primitives swapped in (confirm → appConfirm, native input
// → .s-input).

const AUTH_PROTOS = ['', 'MD5', 'SHA', 'SHA-224', 'SHA-256', 'SHA-384', 'SHA-512']
const PRIV_PROTOS = ['', 'DES', 'AES', 'AES-192', 'AES-256']

export function SNMP() {
  const list = useQuery({
    queryKey: ['snmp-credentials'],
    queryFn: () => api.listCredentials().catch((e: Error) => Promise.reject(e)),
    refetchInterval: 10_000,
    retry: false,
  })
  const [editing, setEditing] = useState<SNMPCredential | 'new' | null>(null)

  const isUnavailable =
    list.error?.message?.includes('503') ||
    list.error?.message?.includes('disabled')

  return (
    <div>
      <SectionHeader
        eyebrow="SNMP"
        title="Per-exporter credentials"
        subtitle="v2c communities and v3 user/auth/priv passphrases. Stored in ClickHouse, AES-256-GCM-sealed under FLOWSCOPE_SNMP_KEY."
        actions={
          !isUnavailable && (
            <Btn tone="accent" size="md" onClick={() => setEditing('new')}>
              + binding
            </Btn>
          )
        }
      />
      <StyleScope />

      {isUnavailable && (
        <div className="px-6 pt-4">
          <Banner tone="crit">
            <strong className="text-crit">Disabled.</strong> The api service is
            running without <code className="bg-raise px-1 font-mono">FLOWSCOPE_SNMP_KEY</code>.
            Set the master-key env var on both the api and snmp services (same
            value) and restart to enable per-exporter credential management.
          </Banner>
        </div>
      )}

      {!isUnavailable && (
        <Section
          eyebrow={`Bindings · ${list.data?.count ?? 0}`}
          hint="encrypted at rest · master key MUST stay constant"
        >
          {editing && (
            <CredentialForm
              initial={editing === 'new' ? null : editing}
              onClose={() => setEditing(null)}
            />
          )}
          {list.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
          {list.data && list.data.credentials.length === 0 && !editing && (
            <Empty>
              no per-exporter credentials configured · the snmp service falls back to
              the cluster-wide v2c community / mock
            </Empty>
          )}
          {list.data && list.data.credentials.length > 0 && (
            <CredentialList
              rows={list.data.credentials}
              onEdit={(c) => setEditing(c)}
            />
          )}
        </Section>
      )}
    </div>
  )
}

function Th({ children, className }: { children?: React.ReactNode; className?: string }) {
  return (
    <th
      className={`text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2 ${className === 'r' ? 'text-right' : ''}`}
    >
      {children}
    </th>
  )
}

function SortTh({
  sortKey,
  active,
  dir,
  onToggle,
  align,
  children,
}: {
  sortKey: string
  active: string | null
  dir: SortDir
  onToggle: (k: string) => void
  align?: 'r'
  children?: React.ReactNode
}) {
  const isActive = active === sortKey
  const arrow = isActive ? (dir === 'asc' ? '▲' : '▼') : '↕'
  return (
    <th
      className={`text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2 ${align === 'r' ? 'text-right' : ''}`}
    >
      <button
        type="button"
        onClick={() => onToggle(sortKey)}
        className={`inline-flex items-center gap-1.5 ${align === 'r' ? 'flex-row-reverse' : ''} hover:text-text ${isActive ? 'text-text' : ''}`}
      >
        <span>{children}</span>
        <span
          aria-hidden
          className={`text-[9px] ${isActive ? 'text-accent' : 'text-line'}`}
        >
          {arrow}
        </span>
      </button>
    </th>
  )
}

function CredentialList({
  rows,
  onEdit,
}: {
  rows: SNMPCredential[]
  onEdit: (c: SNMPCredential) => void
}) {
  const { sortedRows, sortKey, sortDir, toggle } = useTableSort(rows, CRED_COLS, {
    key: 'exporter',
    dir: 'asc',
  })
  const props = (k: string) => ({
    sortKey: k,
    active: sortKey,
    dir: sortDir,
    onToggle: toggle,
  })
  return (
    <div className="border border-line">
      <table className="w-full">
        <thead>
          <tr className="border-b border-line bg-raise">
            <SortTh {...props('exporter')}>exporter</SortTh>
            <SortTh {...props('version')}>version</SortTh>
            <SortTh {...props('identity')}>identity</SortTh>
            <SortTh {...props('secrets')}>secrets</SortTh>
            <SortTh {...props('port')} align="r">port</SortTh>
            <SortTh {...props('interval')} align="r">interval</SortTh>
            <SortTh {...props('updated')}>updated</SortTh>
            <Th />
          </tr>
        </thead>
        <tbody>
          {sortedRows.map((c) => (
            <CredentialRow key={c.exporter} c={c} onEdit={() => onEdit(c)} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function CredentialRow({ c, onEdit }: { c: SNMPCredential; onEdit: () => void }) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const del = useMutation({
    mutationFn: () => api.deleteCredential(c.exporter),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['snmp-credentials'] }),
  })
  const [test, setTest] = useState<SNMPTestResult | null>(null)
  const [testing, setTesting] = useState(false)
  const runTest = async () => {
    setTesting(true)
    try {
      const r = await api.testCredential(c.exporter)
      setTest(r)
    } catch (e) {
      setTest({ ok: false, error: (e as Error).message })
    } finally {
      setTesting(false)
    }
  }
  const identity = c.version === 'v3' ? c.v3_username || '—' : 'community'
  const secrets =
    c.version === 'v3'
      ? [c.has_auth_pass ? 'auth' : null, c.has_priv_pass ? 'priv' : null]
          .filter(Boolean)
          .join(' + ') || 'noAuthNoPriv'
      : c.has_community
        ? '✓'
        : '—'

  return (
    <>
      <tr className="border-b border-line/60 hover:bg-surface">
        <td className="px-3 py-1.5 text-[12.5px] font-mono text-text">{c.exporter}</td>
        <td className="px-3 py-1.5 text-[12.5px] font-mono uppercase">
          <Tag tone={c.version === 'v3' ? 'accent' : undefined}>{c.version}</Tag>
        </td>
        <td className="px-3 py-1.5 text-[12.5px] text-dim">{identity}</td>
        <td className="px-3 py-1.5 text-[12px] font-mono text-faint">{secrets}</td>
        <td className="px-3 py-1.5 text-right text-[12.5px] font-mono">{c.port}</td>
        <td className="px-3 py-1.5 text-right text-[12.5px] font-mono">{Math.round(c.interval_sec / 60)}m</td>
        <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
          {c.updated_at ? fmt.time(c.updated_at).slice(0, 19) : '—'}
          {c.updated_by ? ` · ${c.updated_by}` : ''}
        </td>
        <td className="px-3 py-1.5">
          <div className="flex justify-end gap-2">
            <Btn onClick={runTest} disabled={testing}>
              {testing ? 'testing…' : 'test'}
            </Btn>
            <Btn onClick={onEdit}>edit</Btn>
            <Btn
              tone="crit"
              onClick={async () => {
                const ok = await confirm({
                  title: `Delete SNMP credential for ${c.exporter}?`,
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
      {test && (
        <tr>
          <td colSpan={8} className="bg-raise px-4 py-2.5 font-mono text-[11.5px]">
            {test.ok ? (
              <span className="text-ok">
                ✓ ok · sys_name={test.sys_name || '—'} · interfaces=
                {test.interfaces} · {test.poll_duration_ms}ms
              </span>
            ) : (
              <span className="text-crit">✗ {test.error || 'failed'}</span>
            )}
            <button
              onClick={() => setTest(null)}
              className="ml-3 text-faint hover:text-text"
            >
              dismiss
            </button>
          </td>
        </tr>
      )}
    </>
  )
}

function CredentialForm({ initial, onClose }: { initial: SNMPCredential | null; onClose: () => void }) {
  const qc = useQueryClient()
  const isEdit = !!initial
  const [c, setC] = useState<SNMPCredential>(
    initial ?? {
      exporter: '',
      version: 'v2c',
      port: 161,
      interval_sec: 900,
      has_community: false,
      has_auth_pass: false,
      has_priv_pass: false,
    },
  )
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putCredential(c),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-credentials'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const set = <K extends keyof SNMPCredential>(k: K, v: SNMPCredential[K]) => setC({ ...c, [k]: v })

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit credential' : 'New credential'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">cancel</button>
      </div>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-3">
        <Field label="exporter">
          <input
            disabled={isEdit}
            value={c.exporter}
            onChange={(e) => set('exporter', e.target.value)}
            placeholder="10.2.0.11"
            className="s-input"
          />
        </Field>
        <Field label="version">
          <select
            value={c.version}
            onChange={(e) => set('version', e.target.value as 'v2c' | 'v3')}
            className="s-input"
          >
            <option value="v2c">v2c</option>
            <option value="v3">v3</option>
          </select>
        </Field>
        <Field label="port">
          <input
            type="number"
            value={c.port}
            onChange={(e) => set('port', Number(e.target.value) || 161)}
            className="s-input"
          />
        </Field>
        <Field label="interval (sec)">
          <input
            type="number"
            value={c.interval_sec}
            onChange={(e) => set('interval_sec', Number(e.target.value) || 900)}
            className="s-input"
          />
        </Field>
      </div>

      {c.version === 'v2c' && (
        <Field label={`community ${c.has_community ? '· (already set; leave blank to keep)' : ''}`}>
          <input
            type="password"
            value={c.community || ''}
            onChange={(e) => set('community', e.target.value)}
            placeholder={c.has_community ? '••••••••' : 'public'}
            className="s-input"
          />
        </Field>
      )}

      {c.version === 'v3' && (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
          <Field label="username">
            <input
              value={c.v3_username || ''}
              onChange={(e) => set('v3_username', e.target.value)}
              placeholder="noc-readonly"
              className="s-input"
            />
          </Field>
          <Field label="auth protocol">
            <select
              value={c.v3_auth_proto || ''}
              onChange={(e) => set('v3_auth_proto', e.target.value)}
              className="s-input"
            >
              {AUTH_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>{p || 'none'}</option>
              ))}
            </select>
          </Field>
          <Field label={`auth passphrase ${c.has_auth_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_auth_pass || ''}
              onChange={(e) => set('v3_auth_pass', e.target.value)}
              placeholder={c.has_auth_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="priv protocol">
            <select
              value={c.v3_priv_proto || ''}
              onChange={(e) => set('v3_priv_proto', e.target.value)}
              className="s-input"
            >
              {PRIV_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>{p || 'none'}</option>
              ))}
            </select>
          </Field>
          <Field label={`priv passphrase ${c.has_priv_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_priv_pass || ''}
              onChange={(e) => set('v3_priv_pass', e.target.value)}
              placeholder={c.has_priv_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="context (optional)">
            <input
              value={c.v3_context || ''}
              onChange={(e) => set('v3_context', e.target.value)}
              className="s-input"
            />
          </Field>
        </div>
      )}

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <Btn tone="accent" size="md" disabled={save.isPending || !c.exporter} onClick={() => { setError(null); save.mutate() }}>
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
        <Btn size="md" onClick={onClose}>cancel</Btn>
        <span className="ml-auto font-mono text-[10.5px] text-faint">
          empty passphrase fields preserve the existing secret
        </span>
      </div>
    </div>
  )
}
