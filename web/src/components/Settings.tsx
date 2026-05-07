import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type ReactNode } from 'react'
import { api, fmt } from '../api'
import type { SNMPCredential, SNMPTestResult } from '../api'

// Settings tab — for v0 the only sub-page is SNMP credential
// management. Future settings sub-pages (alert rule editor, OIDC,
// API tokens, RBAC) will share this shell once they ship.
export function Settings() {
  return (
    <div>
      <SettingsHeader />
      <SnmpCredentials />
    </div>
  )
}

function SettingsHeader() {
  return (
    <div className="px-6 pt-6 pb-3 border-b border-line bg-surface">
      <div className="text-[10.5px] uppercase tracking-[0.18em] font-mono font-semibold text-faint mb-1">
        Settings
      </div>
      <h1 className="text-[20px] font-semibold tracking-tight text-text leading-[1.2]">
        Administration
      </h1>
      <p className="text-[13px] text-dim mt-1.5 max-w-[78ch] leading-[1.5]">
        Configure FlowScope itself. SNMP credential management today; alert
        rules, OIDC SSO, API tokens, and RBAC arrive in follow-up slices.
        Endpoints here are{' '}
        <span className="text-warn">not yet auth-gated</span> — restrict
        access at the reverse proxy until OIDC ships.
      </p>
    </div>
  )
}

/* ----------------------------- SNMP credentials ----------------------------- */

function SnmpCredentials() {
  const list = useQuery({
    queryKey: ['snmp-credentials'],
    queryFn: () => api.listCredentials().catch((e: Error) => Promise.reject(e)),
    refetchInterval: 10_000,
    retry: false,
  })
  const [editing, setEditing] = useState<SNMPCredential | null>(null)
  const [adding, setAdding] = useState(false)

  const isUnavailable =
    list.error?.message?.includes('503') ||
    list.error?.message?.includes('disabled')

  return (
    <section className="px-6 py-5">
      <div className="flex items-baseline gap-3 pb-3 border-b border-line mb-4">
        <span className="text-[11px] uppercase tracking-[0.1em] text-dim font-semibold">
          SNMP credentials
        </span>
        <span className="font-mono text-[11px] text-faint">
          per-exporter v2c / v3 bindings · encrypted at rest
        </span>
        {!isUnavailable && (
          <button
            onClick={() => {
              setAdding(true)
              setEditing(null)
            }}
            className="ml-auto font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1 border border-accent text-accent hover:bg-accent-wash"
          >
            + add binding
          </button>
        )}
      </div>

      {isUnavailable && (
        <div className="border border-warn/40 bg-warn-wash px-4 py-3 mb-4 text-[13px] text-text">
          <span className="font-semibold text-warn">Disabled.</span>{' '}
          The api service is running without{' '}
          <code className="bg-raise px-1 font-mono">FLOWSCOPE_SNMP_KEY</code>.
          Set the master-key env var on both the api and snmp services
          (same value) and restart to enable per-exporter credential
          management.
        </div>
      )}

      {!isUnavailable && (adding || editing) && (
        <CredentialForm
          initial={editing}
          onClose={() => {
            setAdding(false)
            setEditing(null)
          }}
        />
      )}

      {!isUnavailable && (
        <CredentialList
          rows={list.data?.credentials ?? []}
          loading={list.isLoading}
          onEdit={(c) => {
            setEditing(c)
            setAdding(false)
          }}
        />
      )}
    </section>
  )
}

function CredentialList({
  rows,
  loading,
  onEdit,
}: {
  rows: SNMPCredential[]
  loading: boolean
  onEdit: (c: SNMPCredential) => void
}) {
  if (loading) return <p className="text-dim font-mono text-[12px]">loading…</p>
  if (rows.length === 0) {
    return (
      <div className="border border-dashed border-line py-6 text-center text-[12px] font-mono text-dim">
        no per-exporter credentials configured · the snmp service falls back to
        the cluster-wide v2c community / mock
      </div>
    )
  }
  return (
    <table className="w-full">
      <thead>
        <tr>
          <th>exporter</th>
          <th>version</th>
          <th>identity</th>
          <th>secrets</th>
          <th className="r">port</th>
          <th className="r">interval</th>
          <th>updated</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <CredentialRow key={r.exporter} c={r} onEdit={() => onEdit(r)} />
        ))}
      </tbody>
    </table>
  )
}

function CredentialRow({
  c,
  onEdit,
}: {
  c: SNMPCredential
  onEdit: () => void
}) {
  const qc = useQueryClient()
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
      ? [
          c.has_auth_pass ? 'auth' : null,
          c.has_priv_pass ? 'priv' : null,
        ]
          .filter(Boolean)
          .join(' + ') || 'noAuthNoPriv'
      : c.has_community
        ? '✓'
        : '—'

  return (
    <>
      <tr className="hover:bg-surface">
        <td className="n">{c.exporter}</td>
        <td className={c.version === 'v3' ? 'text-accent font-mono' : 'font-mono'}>
          {c.version}
        </td>
        <td className="n text-dim">{identity}</td>
        <td className="font-mono text-faint">{secrets}</td>
        <td className="r n">{c.port}</td>
        <td className="r n">
          {Math.round(c.interval_sec / 60)}m
        </td>
        <td className="n text-faint">
          {c.updated_at ? fmt.time(c.updated_at).slice(0, 19) : '—'}
          {c.updated_by ? ` · ${c.updated_by}` : ''}
        </td>
        <td className="r">
          <div className="flex justify-end gap-2">
            <button
              onClick={runTest}
              disabled={testing}
              className="font-mono text-[11px] text-dim hover:text-accent disabled:opacity-50"
            >
              {testing ? 'testing…' : 'test'}
            </button>
            <button
              onClick={onEdit}
              className="font-mono text-[11px] text-dim hover:text-accent"
            >
              edit
            </button>
            <button
              onClick={() => {
                if (confirm(`Delete SNMP credential for ${c.exporter}?`)) del.mutate()
              }}
              className="font-mono text-[11px] text-dim hover:text-crit"
            >
              delete
            </button>
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

/* ----------------------------- Credential form ----------------------------- */

const AUTH_PROTOS = ['', 'MD5', 'SHA', 'SHA-224', 'SHA-256', 'SHA-384', 'SHA-512']
const PRIV_PROTOS = ['', 'DES', 'AES', 'AES-192', 'AES-256']

function CredentialForm({
  initial,
  onClose,
}: {
  initial: SNMPCredential | null
  onClose: () => void
}) {
  const qc = useQueryClient()
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
  const isEdit = !!initial
  const [error, setError] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: () => api.putCredential(c),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['snmp-credentials'] })
      onClose()
    },
    onError: (e: Error) => setError(e.message),
  })

  const set = <K extends keyof SNMPCredential>(k: K, v: SNMPCredential[K]) =>
    setC({ ...c, [k]: v })

  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          {isEdit ? 'Edit credential' : 'New credential'}
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">
          cancel
        </button>
      </div>

      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-3">
        <Field label="exporter">
          <input
            disabled={isEdit}
            value={c.exporter}
            onChange={(e) => set('exporter', e.target.value)}
            placeholder="10.2.0.11"
            className="input"
          />
        </Field>
        <Field label="version">
          <select
            value={c.version}
            onChange={(e) => set('version', e.target.value as 'v2c' | 'v3')}
            className="input"
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
            className="input"
          />
        </Field>
        <Field label="interval (sec)">
          <input
            type="number"
            value={c.interval_sec}
            onChange={(e) => set('interval_sec', Number(e.target.value) || 900)}
            className="input"
          />
        </Field>
      </div>

      {c.version === 'v2c' && (
        <div className="grid grid-cols-1 gap-3">
          <Field label={`community ${c.has_community ? '· (already set; leave blank to keep)' : ''}`}>
            <input
              type="password"
              value={c.community || ''}
              onChange={(e) => set('community', e.target.value)}
              placeholder={c.has_community ? '••••••••' : 'public'}
              className="input"
            />
          </Field>
        </div>
      )}

      {c.version === 'v3' && (
        <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
          <Field label="username">
            <input
              value={c.v3_username || ''}
              onChange={(e) => set('v3_username', e.target.value)}
              placeholder="noc-readonly"
              className="input"
            />
          </Field>
          <Field label="auth protocol">
            <select
              value={c.v3_auth_proto || ''}
              onChange={(e) => set('v3_auth_proto', e.target.value)}
              className="input"
            >
              {AUTH_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>
                  {p || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`auth passphrase ${c.has_auth_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_auth_pass || ''}
              onChange={(e) => set('v3_auth_pass', e.target.value)}
              placeholder={c.has_auth_pass ? '••••••••' : ''}
              className="input"
            />
          </Field>
          <Field label="priv protocol">
            <select
              value={c.v3_priv_proto || ''}
              onChange={(e) => set('v3_priv_proto', e.target.value)}
              className="input"
            >
              {PRIV_PROTOS.map((p) => (
                <option key={p || 'none'} value={p}>
                  {p || 'none'}
                </option>
              ))}
            </select>
          </Field>
          <Field label={`priv passphrase ${c.has_priv_pass ? '· (set)' : ''}`}>
            <input
              type="password"
              value={c.v3_priv_pass || ''}
              onChange={(e) => set('v3_priv_pass', e.target.value)}
              placeholder={c.has_priv_pass ? '••••••••' : ''}
              className="input"
            />
          </Field>
          <Field label="context (optional)">
            <input
              value={c.v3_context || ''}
              onChange={(e) => set('v3_context', e.target.value)}
              className="input"
            />
          </Field>
        </div>
      )}

      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}

      <div className="flex items-center gap-3 mt-4">
        <button
          onClick={() => {
            setError(null)
            save.mutate()
          }}
          disabled={save.isPending || !c.exporter}
          className="font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1.5 border border-accent text-accent hover:bg-accent-wash disabled:opacity-50"
        >
          {save.isPending ? 'saving…' : 'save'}
        </button>
        <button
          onClick={onClose}
          className="font-mono text-[11px] uppercase tracking-[0.06em] px-3 py-1.5 border border-line text-dim hover:text-text"
        >
          cancel
        </button>
        <span className="ml-auto font-mono text-[10.5px] text-faint">
          empty passphrase fields preserve the existing secret
        </span>
      </div>

      <style>{`
        .input {
          background: var(--color-ink);
          border: 1px solid var(--color-line);
          padding: 6px 8px;
          font-family: var(--font-mono);
          font-size: 12.5px;
          color: var(--color-text);
          outline: none;
          width: 100%;
        }
        .input:focus { border-color: var(--color-accent); }
        .input:disabled { color: var(--color-dim); cursor: not-allowed; }
      `}</style>
    </div>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-mono font-semibold block mb-1">
        {label}
      </span>
      {children}
    </label>
  )
}
