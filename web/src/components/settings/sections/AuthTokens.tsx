import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api, setSettingsAuthToken, type APIToken, type OIDCConfig } from '../../../api'
import { useAppConfirm } from '../../ui/appConfirm'
import { SectionHeader } from '../Shell'
import { Banner, Btn, Empty, Field, Section, StyleScope, Tag } from '../shared'

// AuthTokens: OIDC SSO config (primary human path), per-token API
// tokens (for CLI / automation), and a collapsed advanced disclosure
// for the legacy browser-session shared-token paste. Order matters —
// SSO is the headline for enterprise tenants; the shared-token form
// is deliberately demoted now that LoginPage handles first-run paste.

export function AuthTokens() {
  return (
    <div data-testid="auth-tokens-section">
      <SectionHeader
        eyebrow="Auth & tokens"
        title="SSO & API access"
        subtitle="OIDC for humans, API tokens for scripts and automation."
      />
      <StyleScope />
      <OIDC />
      <Tokens />
      <SessionToken />
    </div>
  )
}

/* ----------------------------- Session token (local, advanced) ----------------------------- */

// The browser-side X-Auth-Token paste survives as an escape hatch for
// operators who can't sign in via SSO yet (e.g. mid-rollout, IdP
// outage, scripted browser automation). LoginPage owns the first-run
// paste experience now, so this form is collapsed by default — it's
// the "I know what I'm doing" advanced control, not the front door.
function SessionToken() {
  const initial = (() => {
    try {
      return localStorage.getItem('flowscope:auth-token') ?? ''
    } catch {
      return ''
    }
  })()
  // open defaults to true ONLY when a token is already saved — so an
  // operator who set one previously can find it again without
  // hunting; first-time visitors see the disclosure collapsed.
  const [open, setOpen] = useState(!!initial)
  const [tok, setTok] = useState(initial)
  const [saved, setSaved] = useState(false)
  return (
    <Section
      eyebrow="Browser session token · advanced"
      hint="legacy fallback · prefer SSO above or mint an API token"
    >
      {!open ? (
        <Btn
          size="md"
          data-testid="auth-session-token-show"
          onClick={() => setOpen(true)}
        >
          show advanced
        </Btn>
      ) : (
        <>
          <Banner tone="warn">
            <strong className="text-warn">Advanced.</strong> Pastes a token into
            this browser's localStorage and attaches it as{' '}
            <code className="font-mono text-text">X-Auth-Token</code> on every
            write. Use SSO above for humans; mint a scoped API token for scripts.
          </Banner>
          <div className="flex items-end gap-3 max-w-[800px]" data-testid="auth-session-token-form">
            <div className="flex-1">
              <Field label="token">
                <input
                  type="password"
                  value={tok}
                  onChange={(e) => {
                    setTok(e.target.value)
                    setSaved(false)
                  }}
                  placeholder="fls_…  (or the shared FLOWSCOPE_AUTH_TOKEN)"
                  data-testid="auth-session-token-input"
                  className="s-input"
                />
              </Field>
            </div>
            <Btn
              tone="accent"
              size="md"
              data-testid="auth-session-token-save"
              onClick={() => {
                setSettingsAuthToken(tok)
                setSaved(true)
              }}
            >
              {saved ? 'saved' : 'save'}
            </Btn>
            <Btn
              onClick={() => {
                setTok('')
                setSettingsAuthToken('')
                setSaved(false)
              }}
            >
              clear
            </Btn>
          </div>
        </>
      )}
    </Section>
  )
}

/* ----------------------------- API tokens (server) ----------------------------- */

function Tokens() {
  const list = useQuery({
    queryKey: ['api-tokens'],
    queryFn: () => api.listTokens(),
  })
  const [creating, setCreating] = useState(false)
  const [minted, setMinted] = useState<APIToken | null>(null)

  return (
    <Section
      eyebrow={`API tokens · ${list.data?.count ?? 0}`}
      hint="bcrypt-hashed in ClickHouse · plaintext shown once"
      actions={
        <Btn tone="accent" size="md" onClick={() => setCreating(true)}>
          + token
        </Btn>
      }
    >
      {creating && (
        <CreateForm
          onCancel={() => setCreating(false)}
          onCreated={(t) => {
            setMinted(t)
            setCreating(false)
          }}
        />
      )}
      {minted && <MintedReveal t={minted} onClose={() => setMinted(null)} />}

      {list.isLoading && <div className="text-dim text-[12px] font-mono">loading…</div>}
      {list.data && list.data.rows.length === 0 && !creating && !minted && (
        <Empty>no tokens · the shared FLOWSCOPE_AUTH_TOKEN env var still works</Empty>
      )}
      {list.data && list.data.rows.length > 0 && (
        <div className="border border-line">
          <table className="w-full">
            <thead>
              <tr className="border-b border-line bg-raise">
                <Th>name</Th>
                <Th>scope</Th>
                <Th>prefix</Th>
                <Th>created</Th>
                <Th>last used</Th>
                <Th>state</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {list.data.rows.map((t) => (
                <TokenRow key={t.id} t={t} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  )
}

function Th({ children }: { children?: React.ReactNode }) {
  return (
    <th className="text-left text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold px-3 py-2">
      {children}
    </th>
  )
}

function TokenRow({ t }: { t: APIToken }) {
  const qc = useQueryClient()
  const confirm = useAppConfirm()
  const revoke = useMutation({
    mutationFn: () => api.revokeToken(t.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['api-tokens'] }),
  })
  const revoked = !!t.revoked_at && t.revoked_at !== '0001-01-01T00:00:00Z' && !t.revoked_at.startsWith('1970')
  return (
    <tr className="border-b border-line/60 hover:bg-surface">
      <td className="px-3 py-1.5 text-[12.5px] text-text">{t.name}</td>
      <td className="px-3 py-1.5">
        <Tag tone={t.scope === 'admin' ? 'crit' : t.scope === 'write' ? 'accent' : undefined}>
          {t.scope}
        </Tag>
      </td>
      <td className="px-3 py-1.5 text-[12px] font-mono text-faint">fls_{t.prefix}</td>
      <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
        {t.created_at?.slice(0, 19).replace('T', ' ')} {t.created_by ? `· ${t.created_by}` : ''}
      </td>
      <td className="px-3 py-1.5 text-[11px] font-mono text-faint">
        {t.last_used_at && !t.last_used_at.startsWith('0001') && !t.last_used_at.startsWith('1970')
          ? t.last_used_at.slice(0, 19).replace('T', ' ')
          : 'never'}
      </td>
      <td className="px-3 py-1.5">
        {revoked ? <Tag tone="crit">revoked</Tag> : <Tag tone="ok">active</Tag>}
      </td>
      <td className="px-3 py-1.5 text-right">
        {!revoked && (
          <Btn
            tone="crit"
            onClick={async () => {
              const ok = await confirm({
                title: `Revoke "${t.name}"?`,
                body: 'Active integrations using this token will start failing.',
                confirmLabel: 'Revoke',
                tone: 'crit',
              })
              if (ok) revoke.mutate()
            }}
          >
            revoke
          </Btn>
        )}
      </td>
    </tr>
  )
}

function CreateForm({ onCancel, onCreated }: { onCancel: () => void; onCreated: (t: APIToken) => void }) {
  const [name, setName] = useState('')
  const [scope, setScope] = useState<APIToken['scope']>('write')
  const [error, setError] = useState<string | null>(null)
  const create = useMutation({
    mutationFn: () => api.createToken(name, scope),
    onSuccess: (t) => onCreated(t as APIToken),
    onError: (e: Error) => setError(e.message),
  })
  return (
    <div className="border border-accent/40 bg-accent-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-accent font-semibold">
          New token
        </span>
        <button onClick={onCancel} className="ml-auto font-mono text-[11px] text-dim hover:text-text">cancel</button>
      </div>
      <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
        <Field label="name">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="ci-bot · ansible · grafana-flow-import"
            className="s-input"
          />
        </Field>
        <Field label="scope">
          <select
            value={scope}
            onChange={(e) => setScope(e.target.value as APIToken['scope'])}
            className="s-input"
          >
            <option value="read">read</option>
            <option value="write">write</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <div />
      </div>
      {error && <div className="mt-3 text-crit font-mono text-[12px]">{error}</div>}
      <div className="flex items-center gap-3 mt-4">
        <Btn
          tone="accent"
          size="md"
          disabled={!name || create.isPending}
          onClick={() => {
            setError(null)
            create.mutate()
          }}
        >
          {create.isPending ? 'creating…' : 'mint'}
        </Btn>
        <Btn size="md" onClick={onCancel}>cancel</Btn>
        <span className="ml-auto font-mono text-[10.5px] text-faint">
          plaintext shown once · keep it safe
        </span>
      </div>
    </div>
  )
}

function MintedReveal({ t, onClose }: { t: APIToken; onClose: () => void }) {
  return (
    <div className="border border-warn/40 bg-warn-wash px-4 py-4 mb-4">
      <div className="flex items-baseline gap-3 mb-3">
        <span className="text-[11px] uppercase tracking-[0.1em] text-warn font-semibold">
          copy this now — it won't be shown again
        </span>
        <button onClick={onClose} className="ml-auto font-mono text-[11px] text-dim hover:text-text">dismiss</button>
      </div>
      <code className="block font-mono text-[12.5px] bg-ink border border-line px-3 py-2 select-all break-all">
        {t.plaintext}
      </code>
      <div className="text-[12px] text-faint mt-2 font-mono">
        scope · {t.scope} · prefix · fls_{t.prefix}
      </div>
    </div>
  )
}

/* ----------------------------- OIDC ----------------------------- */

function OIDC() {
  const data = useQuery({
    queryKey: ['oidc'],
    queryFn: () => api.getOIDC(),
  })
  const [c, setC] = useState<OIDCConfig | null>(null)
  useEffect(() => {
    if (data.data) setC(data.data)
  }, [data.dataUpdatedAt])

  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: () => api.putOIDC(c as OIDCConfig),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['oidc'] }),
  })

  if (!c) return null

  // Phase 2 ships in this PR — show the operator the active sign-in
  // entry point when the flag is on, and the rollback hint when it's
  // off. The Banner tone tracks state so a glance is enough.
  const enabled = !!c.enabled
  return (
    <Section eyebrow="OIDC SSO" hint={enabled ? 'login flow active' : 'login flow staged · enable to activate'}>
      {enabled ? (
        <Banner tone="accent">
          <strong className="text-accent">Active.</strong> Users can sign in via
          your IdP at <a href="/auth/login" className="underline">/auth/login</a>.
          The shared / per-token paths still work — OIDC is additive.
        </Banner>
      ) : (
        <Banner tone="warn">
          <strong className="text-warn">Staged.</strong> Fill in the fields and
          flip <em>enabled</em> on to activate the login flow. Shared and per-
          token auth keep working either way.
        </Banner>
      )}
      <div className="flex items-center gap-3 mb-3">
        {enabled && (
          <a
            href="/auth/login"
            className="px-3 py-1.5 border border-accent text-accent text-[12px] hover:bg-accent-wash font-mono"
          >
            sign in with SSO →
          </a>
        )}
      </div>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-3 max-w-[820px]">
        <Field label="enabled" hint="flipping this on activates /auth/login for users">
          <select
            value={c.enabled ? 'on' : 'off'}
            onChange={(e) => setC({ ...c, enabled: e.target.value === 'on' })}
            className="s-input"
          >
            <option value="off">disabled</option>
            <option value="on">enabled</option>
          </select>
        </Field>
        <Field label="issuer" hint="https://login.microsoftonline.com/<tenant>/v2.0">
          <input
            value={c.issuer ?? ''}
            onChange={(e) => setC({ ...c, issuer: e.target.value })}
            className="s-input"
          />
        </Field>
        <Field label="client id">
          <input
            value={c.client_id ?? ''}
            onChange={(e) => setC({ ...c, client_id: e.target.value })}
            className="s-input"
          />
        </Field>
        <Field label={`client secret ${c.has_secret ? '· (set; blank = preserve)' : ''}`}>
          <input
            type="password"
            value={c.client_secret ?? ''}
            onChange={(e) => setC({ ...c, client_secret: e.target.value })}
            placeholder={c.has_secret ? '••••••••' : ''}
            className="s-input"
          />
        </Field>
        <Field label="redirect URI" hint="must be registered with the IdP">
          <input
            value={c.redirect_uri ?? ''}
            onChange={(e) => setC({ ...c, redirect_uri: e.target.value })}
            placeholder="https://flowscope.example.com/auth/callback"
            className="s-input"
          />
        </Field>
        <Field label="scopes">
          <input
            value={c.scopes ?? ''}
            onChange={(e) => setC({ ...c, scopes: e.target.value })}
            placeholder="openid email profile"
            className="s-input"
          />
        </Field>
      </div>
      <div className="flex items-center gap-3 mt-4">
        <Btn tone="accent" size="md" disabled={save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? 'saving…' : 'save'}
        </Btn>
      </div>
    </Section>
  )
}
