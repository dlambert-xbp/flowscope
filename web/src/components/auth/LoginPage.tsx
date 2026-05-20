import { useState } from 'react'
import { getConfig } from '../../config'
import { setSettingsAuthToken } from '../../api'

// LoginPage is the cold-boot entry point for un-authed visitors. It's
// rendered by AuthGate when /api/summary returns 401. Two paths:
//
//   • SSO — primary when the backend has OIDC enabled (WWW-Authenti-
//     cate: oidc on the 401). The button hard-navigates to
//     /auth/login, which on the api side mints PKCE state + redirects
//     to the IdP. The IdP returns to /auth/callback, which sets the
//     session cookie and lands the operator back at /.
//
//   • API token — fallback for non-OIDC tenants and for CLI-leaning
//     humans. Paste into the field, click save; AuthGate's onTokenSaved
//     callback invalidates the gate query, which re-probes /api/summary
//     with the new token and flips the gate to authed on success.
//
// When OIDC is enabled the token field is collapsed behind a small
// disclosure to keep the SSO button visually dominant. When OIDC is
// off the token field is open by default — there is no SSO to choose.
//
// Branding follows the rest of the app: dark/light themed surface,
// the same 4×4 accent square mark and uppercase eyebrow / hint typo-
// graphy. No external assets, no images — the visual identity is the
// type system + the accent square.

export function LoginPage({
  oidcEnabled,
  onTokenSaved,
}: {
  oidcEnabled: boolean
  onTokenSaved: () => void
}) {
  const brand = getConfig().display_name
  return (
    <div className="grid place-items-center min-h-screen bg-ink text-text px-6 py-12">
      <div className="w-full max-w-[440px]">
        <Brand name={brand} />
        <div className="border border-line bg-surface mt-8">
          <div className="px-6 pt-6 pb-4 border-b border-line">
            <div className="text-[10.5px] uppercase tracking-[0.1em] text-faint font-mono font-semibold">
              sign in
            </div>
            <div className="text-[14px] text-text mt-1.5 leading-snug">
              {oidcEnabled
                ? 'Continue with your organization sign-in.'
                : 'Paste your API token to continue.'}
            </div>
          </div>
          <div className="px-6 py-6">
            {oidcEnabled ? (
              <SSOBlock />
            ) : (
              <NoSSOBanner />
            )}
            <Divider show={oidcEnabled} />
            <TokenBlock startCollapsed={oidcEnabled} onSaved={onTokenSaved} />
          </div>
          <FooterHint oidcEnabled={oidcEnabled} />
        </div>
      </div>
    </div>
  )
}

/* ----------------------------- Brand ----------------------------- */

function Brand({ name }: { name: string }) {
  return (
    <div className="flex items-center gap-3 justify-center">
      <span className="relative w-5 h-5 border-[1.5px] border-accent">
        <span className="absolute inset-[3px] bg-accent" />
      </span>
      <span className="font-semibold tracking-tight text-[20px]">
        {name}
        <span className="text-accent">.</span>
      </span>
    </div>
  )
}

/* ----------------------------- SSO ----------------------------- */

function SSOBlock() {
  // Stash the current URL so the post-callback bounce can restore it.
  // Today the api redirects to "/" after /auth/callback; AuthGate's
  // consumePostLoginReturn picks up this key and replaceState's back
  // to the original URL on the next paint. Safe to no-op if the
  // visitor came straight to "/".
  const stashReturn = () => {
    try {
      const here = window.location.pathname + window.location.search
      if (here !== '/' && here !== '') {
        sessionStorage.setItem('flowscope:post-login-return', here)
      }
    } catch {
      /* sessionStorage unavailable — best effort */
    }
  }
  return (
    <a
      href="/auth/login"
      onClick={stashReturn}
      data-testid="login-sso-button"
      className="block text-center px-4 py-3 border border-accent text-accent hover:bg-accent-wash font-mono text-[12px] uppercase tracking-[0.08em]"
    >
      sign in with SSO →
    </a>
  )
}

function NoSSOBanner() {
  return (
    <div className="border border-line bg-raise px-3 py-2 mb-4">
      <div className="text-[11px] text-faint font-mono leading-relaxed">
        <span className="text-dim">SSO not configured.</span> An admin can
        wire OIDC at <span className="text-text">Settings → Auth & tokens</span>{' '}
        once the first API token is in place.
      </div>
    </div>
  )
}

/* ----------------------------- Token ----------------------------- */

function TokenBlock({
  startCollapsed,
  onSaved,
}: {
  startCollapsed: boolean
  onSaved: () => void
}) {
  const [open, setOpen] = useState(!startCollapsed)
  const [tok, setTok] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        data-testid="login-token-disclosure"
        className="block w-full text-center px-3 py-2 text-[11px] uppercase tracking-[0.08em] font-mono text-dim hover:text-text border border-line hover:border-dim"
      >
        use API token instead
      </button>
    )
  }

  const submit = async () => {
    if (!tok || busy) return
    setError(null)
    // Render-on-state-change: flip busy synchronously so the button
    // reflects the click in the same paint, then persist + refetch.
    setBusy(true)
    setSettingsAuthToken(tok.trim())
    // Hand off to AuthGate to re-probe; on success the gate unmounts
    // this component and renders <App />.
    try {
      onSaved()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setBusy(false)
    }
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
      data-testid="login-token-form"
      className="space-y-3"
    >
      <label className="block">
        <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-mono font-semibold block mb-1">
          api token
        </span>
        <input
          autoFocus={!startCollapsed}
          type="password"
          value={tok}
          onChange={(e) => {
            setTok(e.target.value)
            setError(null)
          }}
          placeholder="fls_…  (or the shared FLOWSCOPE_AUTH_TOKEN)"
          data-testid="login-token-input"
          className="w-full bg-ink border border-line px-3 py-2 text-[13px] font-mono focus:border-accent focus:outline-none"
        />
        <span className="block text-[11px] text-faint mt-1 leading-[1.4]">
          stored in this browser's localStorage · cleared by signing out
        </span>
      </label>
      <button
        type="submit"
        disabled={!tok || busy}
        data-testid="login-token-submit"
        className="block w-full text-center px-4 py-2.5 border border-line text-text hover:border-accent hover:text-accent font-mono text-[12px] uppercase tracking-[0.08em] disabled:opacity-50 disabled:cursor-not-allowed"
      >
        {busy ? 'checking…' : 'continue'}
      </button>
      {error && (
        <div className="text-crit font-mono text-[11px]" data-testid="login-token-error">
          {error}
        </div>
      )}
    </form>
  )
}

/* ----------------------------- Divider ----------------------------- */

function Divider({ show }: { show: boolean }) {
  if (!show) return null
  return (
    <div className="my-5 flex items-center gap-3">
      <span className="h-px bg-line flex-1" />
      <span className="text-[10px] uppercase tracking-[0.1em] text-faint font-mono">or</span>
      <span className="h-px bg-line flex-1" />
    </div>
  )
}

/* ----------------------------- Footer ----------------------------- */

function FooterHint({ oidcEnabled }: { oidcEnabled: boolean }) {
  return (
    <div className="px-6 py-3 border-t border-line bg-raise/40 text-[11px] text-faint font-mono leading-relaxed">
      {oidcEnabled ? (
        <>
          Trouble signing in? Check the api logs for the OIDC handshake — see{' '}
          <span className="text-dim">docs/oidc-setup.md</span> for the
          troubleshooting matrix.
        </>
      ) : (
        <>
          API tokens are issued from <span className="text-dim">Settings →
          Auth &amp; tokens</span> once you're signed in.
        </>
      )}
    </div>
  )
}
