import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import {
  api,
  fmt,
  type SNMPBindingKind,
  type SNMPCredential,
  type SNMPGlobalDefault,
  type SNMPTestResult,
} from '../../../api'
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
          eyebrow="Global defaults"
          hint="fleet-wide v2c / v3 fallback · per-exporter bindings can defer to these"
        >
          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            <GlobalDefaultCard role="v2c" />
            <GlobalDefaultCard role="v3" />
          </div>
        </Section>
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
  const [walkState, setWalkState] = useState<'idle' | 'queued' | 'error'>('idle')
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
  const runWalk = async () => {
    setWalkState('queued')
    try {
      await api.requestSnmpWalk(c.exporter)
    } catch {
      setWalkState('error')
      return
    }
    setTimeout(() => setWalkState('idle'), 4000)
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
            <Btn onClick={runWalk} disabled={walkState === 'queued'}>
              {walkState === 'queued'
                ? 'queued · walks ≤30s'
                : walkState === 'error'
                  ? 'walk failed'
                  : 'walk now'}
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
      binding_kind: 'custom',
      has_community: false,
      has_auth_pass: false,
      has_priv_pass: false,
    },
  )
  // Older rows may not carry binding_kind on first fetch — fall back to
  // 'custom' so the radio defaults sensibly.
  const kind: SNMPBindingKind = c.binding_kind || 'custom'
  const usesGlobal = kind === 'global_v2c' || kind === 'global_v3'

  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putCredential({ ...c, binding_kind: kind }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-credentials'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const set = <K extends keyof SNMPCredential>(k: K, v: SNMPCredential[K]) => setC({ ...c, [k]: v })

  // Switching to a global kind also forces the version so the row
  // matches the global it points at; switching back to custom leaves
  // the operator's selection alone.
  const setKind = (next: SNMPBindingKind) => {
    setC((prev) => {
      if (next === 'global_v2c') return { ...prev, binding_kind: next, version: 'v2c' }
      if (next === 'global_v3') return { ...prev, binding_kind: next, version: 'v3' }
      return { ...prev, binding_kind: next }
    })
  }

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit credential' : 'New credential'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">cancel</button>
      </div>

      <Field label="type">
        <div className="flex gap-1 text-[12px]">
          <KindRadio kind="custom" active={kind} onChange={setKind}>Custom (inline)</KindRadio>
          <KindRadio kind="global_v2c" active={kind} onChange={setKind}>Use global v2c</KindRadio>
          <KindRadio kind="global_v3" active={kind} onChange={setKind}>Use global v3</KindRadio>
        </div>
      </Field>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-3 mt-3">
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
            disabled={usesGlobal}
            title={usesGlobal ? 'Locked by binding type' : undefined}
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

      {usesGlobal && (
        <div className="mb-3 px-3 py-2 border border-line bg-raise font-mono text-[11.5px] text-dim">
          this binding will use the fleet-wide <span className="text-accent">{kind === 'global_v2c' ? 'v2c' : 'v3'}</span> default — credentials live in the Global defaults section above
        </div>
      )}

      {!usesGlobal && c.version === 'v2c' && (
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

      {!usesGlobal && c.version === 'v3' && (
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

/* -------------------------- Binding-kind radio -------------------------- */

function KindRadio({
  kind,
  active,
  onChange,
  children,
}: {
  kind: SNMPBindingKind
  active: SNMPBindingKind
  onChange: (k: SNMPBindingKind) => void
  children: React.ReactNode
}) {
  const on = kind === active
  return (
    <button
      type="button"
      onClick={() => onChange(kind)}
      className={`px-2.5 py-1 border font-mono text-[11.5px] uppercase tracking-[0.06em] ${
        on
          ? 'border-accent bg-accent-wash text-accent'
          : 'border-line text-dim hover:border-accent hover:text-text'
      }`}
    >
      {children}
    </button>
  )
}

/* --------------------------- Global defaults --------------------------- */

// GlobalDefaultCard renders one fleet-wide role (v2c or v3) at the top
// of the SNMP section. Shows current configured state and opens a
// per-role edit form inline when the operator clicks Edit. Same
// "blank passphrase preserves existing secret" convention as the
// per-exporter form.
function GlobalDefaultCard({ role }: { role: 'v2c' | 'v3' }) {
  const q = useQuery({
    queryKey: ['snmp-global', role],
    queryFn: () => api.getGlobalCredential(role),
    refetchInterval: 30_000,
    retry: false,
  })
  const [editing, setEditing] = useState(false)
  const g = q.data
  const title = role === 'v2c' ? 'Global v2c default' : 'Global v3 default'
  return (
    <div className="border border-line bg-raise px-3 py-3">
      <div className="flex items-baseline gap-2 mb-2 flex-wrap">
        <span className="text-[11px] uppercase tracking-[0.1em] font-mono text-dim font-semibold">
          {title}
        </span>
        <Tag tone={g?.configured ? 'ok' : undefined}>
          {g?.configured ? 'configured' : 'unconfigured'}
        </Tag>
        {g?.default_for_dynamic && (
          <Tag tone="accent">default for dynamic</Tag>
        )}
        <button
          onClick={() => setEditing((b) => !b)}
          className="ml-auto font-mono text-[11px] text-accent hover:underline"
        >
          {editing ? 'cancel' : g?.configured ? 'edit' : 'configure'}
        </button>
      </div>
      {!editing && g && (
        <div className="font-mono text-[11.5px] text-dim grid grid-cols-2 gap-x-3 gap-y-0.5">
          <span className="text-faint">port</span>
          <span className="text-text tabular">{g.port}</span>
          <span className="text-faint">interval</span>
          <span className="text-text tabular">{g.interval_sec}s</span>
          {role === 'v2c' && (
            <>
              <span className="text-faint">community</span>
              <span className={g.has_community ? 'text-ok' : 'text-faint'}>
                {g.has_community ? 'set' : 'not set'}
              </span>
            </>
          )}
          {role === 'v3' && (
            <>
              <span className="text-faint">username</span>
              <span className="text-text">{g.v3_username || <span className="text-faint">—</span>}</span>
              <span className="text-faint">auth</span>
              <span className={g.has_auth_pass ? 'text-ok' : 'text-faint'}>
                {g.v3_auth_proto ? `${g.v3_auth_proto} ${g.has_auth_pass ? '·set' : ''}` : 'none'}
              </span>
              <span className="text-faint">priv</span>
              <span className={g.has_priv_pass ? 'text-ok' : 'text-faint'}>
                {g.v3_priv_proto ? `${g.v3_priv_proto} ${g.has_priv_pass ? '·set' : ''}` : 'none'}
              </span>
            </>
          )}
        </div>
      )}
      {editing && g && (
        <GlobalDefaultForm initial={g} onClose={() => setEditing(false)} />
      )}
    </div>
  )
}

function GlobalDefaultForm({
  initial,
  onClose,
}: {
  initial: SNMPGlobalDefault
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [g, setG] = useState<SNMPGlobalDefault>(initial)
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putGlobalCredential(g),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-global', g.role] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })
  const set = <K extends keyof SNMPGlobalDefault>(k: K, v: SNMPGlobalDefault[K]) =>
    setG({ ...g, [k]: v })
  return (
    <div className="mt-2 pt-3 border-t border-line">
      <div className="grid grid-cols-2 gap-3 mb-3">
        <Field label="port">
          <input
            type="number"
            value={g.port}
            onChange={(e) => set('port', Number(e.target.value) || 161)}
            className="s-input"
          />
        </Field>
        <Field label="interval (sec)">
          <input
            type="number"
            value={g.interval_sec}
            onChange={(e) => set('interval_sec', Number(e.target.value) || 60)}
            className="s-input"
          />
        </Field>
      </div>
      <label className="flex items-center gap-2 mb-3 font-mono text-[12px] text-dim cursor-pointer select-none">
        <input
          type="checkbox"
          checked={!!g.default_for_dynamic}
          onChange={(e) => set('default_for_dynamic', e.target.checked)}
        />
        <span>
          use as default for dynamically-discovered exporters
          <span className="text-faint">
            {' '}· walks unbound exporters with this credential before falling back to FLOWSCOPE_SNMP_COMMUNITY
          </span>
        </span>
      </label>
      {g.role === 'v2c' && (
        <Field label={`community ${g.has_community ? '· (already set; leave blank to keep)' : ''}`}>
          <input
            type="password"
            value={g.community || ''}
            onChange={(e) => set('community', e.target.value)}
            placeholder={g.has_community ? '••••••••' : 'public'}
            className="s-input"
          />
        </Field>
      )}
      {g.role === 'v3' && (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
          <Field label="username">
            <input
              value={g.v3_username || ''}
              onChange={(e) => set('v3_username', e.target.value)}
              placeholder="noc-readonly"
              className="s-input"
            />
          </Field>
          <Field label="auth protocol">
            <select
              value={g.v3_auth_proto || ''}
              onChange={(e) => set('v3_auth_proto', e.target.value)}
              className="s-input"
            >
              {AUTH_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>{p || 'none'}</option>
              ))}
            </select>
          </Field>
          <Field label={`auth passphrase ${g.has_auth_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={g.v3_auth_pass || ''}
              onChange={(e) => set('v3_auth_pass', e.target.value)}
              placeholder={g.has_auth_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="priv protocol">
            <select
              value={g.v3_priv_proto || ''}
              onChange={(e) => set('v3_priv_proto', e.target.value)}
              className="s-input"
            >
              {PRIV_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>{p || 'none'}</option>
              ))}
            </select>
          </Field>
          <Field label={`priv passphrase ${g.has_priv_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={g.v3_priv_pass || ''}
              onChange={(e) => set('v3_priv_pass', e.target.value)}
              placeholder={g.has_priv_pass ? '••••••••' : ''}
              className="s-input"
            />
          </Field>
          <Field label="context (optional)">
            <input
              value={g.v3_context || ''}
              onChange={(e) => set('v3_context', e.target.value)}
              className="s-input"
            />
          </Field>
        </div>
      )}
      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}
      <div className="flex items-center gap-3 mt-3">
        <Btn tone="accent" size="md" disabled={save.isPending} onClick={() => { setError(null); save.mutate() }}>
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
        <Btn size="md" onClick={onClose}>cancel</Btn>
      </div>
    </div>
  )
}
