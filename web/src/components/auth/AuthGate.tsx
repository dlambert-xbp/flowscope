import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, type ReactNode } from 'react'
import { LoginPage } from './LoginPage'

// AuthGate decides what to render on cold boot:
//
//   • probe /api/summary?seconds=1 (a real authed endpoint — not
//     /healthz, which is open and tells us nothing about authn).
//   • 200  → the user is already signed in (via OIDC cookie, X-Auth-
//     Token, or unauth-bypass when no auth is configured). Render the
//     app, restoring any stashed return URL.
//   • 401  → the user is NOT signed in. The server's
//     WWW-Authenticate header tells us which login path to surface:
//     `oidc` → SSO button is the primary action; absent → API token
//     paste is the primary action.
//   • anything else (network error, 5xx, …) → render the app anyway.
//     The downstream fetches will surface the real failure mode
//     instead of leaving the operator stuck on the boot splash.
//
// Session-expiry mid-session is NOT this component's job — that path
// is still handled by maybeRedirectToLogin in api.ts, which kicks the
// browser straight to /auth/login for an IdP round-trip. AuthGate is
// strictly for the cold-boot "is this person signed in" question.

type GateState =
  | { kind: 'authed' }
  | { kind: 'unauthed'; oidc: boolean }

const POST_LOGIN_RETURN_KEY = 'flowscope:post-login-return'

async function probeAuth(): Promise<GateState> {
  let token = ''
  try {
    token = localStorage.getItem('flowscope:auth-token') ?? ''
  } catch {
    /* localStorage unavailable */
  }
  const headers: Record<string, string> = {}
  if (token) headers['X-Auth-Token'] = token
  const r = await fetch('/api/summary?seconds=1', {
    cache: 'no-store',
    headers,
    credentials: 'same-origin',
  })
  if (r.ok) return { kind: 'authed' }
  if (r.status === 401) {
    const wa = r.headers.get('WWW-Authenticate') ?? ''
    return { kind: 'unauthed', oidc: wa.toLowerCase().includes('oidc') }
  }
  // 5xx / 403 / unexpected — let the app boot and surface the real
  // error in-line. The CLAUDE.md "no silent failures" rule is honored
  // by the downstream fetches' own error UI.
  return { kind: 'authed' }
}

// consumePostLoginReturn reads + clears the stash that maybeRedirect-
// ToLogin (api.ts) sets on session expiry. We restore the URL via
// replaceState so the back stack doesn't grow a phantom /auth/login
// entry.
function consumePostLoginReturn() {
  try {
    const stash = sessionStorage.getItem(POST_LOGIN_RETURN_KEY)
    if (!stash) return
    sessionStorage.removeItem(POST_LOGIN_RETURN_KEY)
    if (stash === window.location.pathname + window.location.search) return
    window.history.replaceState({}, '', stash)
  } catch {
    /* sessionStorage unavailable or stash malformed — give up silently */
  }
}

export function AuthGate({ children }: { children: ReactNode }) {
  const qc = useQueryClient()
  const probe = useQuery({
    queryKey: ['auth-gate'],
    queryFn: probeAuth,
    // Identity probe — no auto-refresh. The token-save path on
    // LoginPage explicitly invalidates this query, and OIDC sign-in
    // hard-navigates via /auth/login, so a periodic refetch would
    // only cost an extra round-trip per refetchInterval tick for no
    // benefit.
    refetchInterval: false,
    refetchOnWindowFocus: false,
    retry: false,
    staleTime: Infinity,
  })

  useEffect(() => {
    if (probe.data?.kind === 'authed') consumePostLoginReturn()
  }, [probe.data])

  if (probe.isLoading || !probe.data) return <Splash />
  if (probe.data.kind === 'unauthed') {
    return (
      <LoginPage
        oidcEnabled={probe.data.oidc}
        onTokenSaved={() => {
          // Invalidate so the next render re-probes /api/summary with
          // the freshly-saved token. Synchronous state flip first
          // (handled inside LoginPage) then refetch — per the project
          // "render on state change" rule.
          qc.invalidateQueries({ queryKey: ['auth-gate'] })
        }}
      />
    )
  }
  return <>{children}</>
}

function Splash() {
  return (
    <div className="grid place-items-center h-screen bg-ink text-text">
      <div className="flex items-center gap-3 text-faint font-mono text-[11px] uppercase tracking-[0.1em]">
        <span className="w-2 h-2 rounded-full bg-accent animate-pulse" />
        <span>checking session…</span>
      </div>
    </div>
  )
}
