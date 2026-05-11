# FlowScope OIDC setup (Phase 2)

This guide walks an operator through wiring an OIDC-compliant identity
provider (Entra ID / Azure AD, Auth0, Okta, Google Workspace, Keycloak,
etc.) so users can sign in to FlowScope without pasting an X-Auth-Token.
OIDC is **additive**: shared-token and per-token auth keep working
exactly as they did before; OIDC is a new, higher-priority path.

## TL;DR

1. Register the app with your IdP. Redirect URI: `https://<host>/auth/callback`.
2. Set `FLOWSCOPE_SESSION_KEY_REF` on the `api` deployment (see below).
3. In **Settings → Auth & tokens → OIDC SSO**, paste issuer / client id /
   client secret / redirect URI, then flip **enabled** on.
4. Click **sign in with SSO** to verify the round-trip works.

If anything goes wrong, flip the flag off and you're back to the
shared/per-token path — no data is lost.

## 1. Register an Entra ID app

In the Azure portal:

1. **Microsoft Entra ID → App registrations → New registration**
2. Name: `FlowScope`
3. Supported account types: usually *Accounts in this organizational
   directory only* (single tenant).
4. **Redirect URI**: platform = *Web*, value = `https://<your-flowscope-host>/auth/callback`.
5. Register. Copy the **Application (client) ID** and the **Directory (tenant) ID**.
6. **Certificates & secrets → New client secret**. Copy the generated
   value immediately — Entra ID shows it only once.
7. **API permissions** are already correct: Microsoft Graph →
   `User.Read` (delegated) is enough for the `email`, `profile`, and
   `openid` scopes FlowScope requests.

For other providers (Auth0, Okta, Keycloak, …) the equivalent steps
are: create an OIDC application, set the redirect URI, copy the
issuer URL + client ID + client secret. The flow is the same.

## 2. Wire `FLOWSCOPE_SESSION_KEY_REF` on the api

The api binary signs session cookies with HMAC-SHA256. The key is read
once at startup. Same loader as `FLOWSCOPE_SNMP_KEY_REF` —
`env:`/`file:`/`kv:` URIs supported.

```yaml
# Helm values snippet
env:
  - name: FLOWSCOPE_SESSION_KEY_REF
    value: "kv:https://my-vault.vault.azure.net/secrets/flowscope-session-key"
```

For dev / docker-compose:

```bash
FLOWSCOPE_SESSION_KEY=$(openssl rand -base64 32)
export FLOWSCOPE_SESSION_KEY_REF="env:FLOWSCOPE_SESSION_KEY"
```

**Distinct from `FLOWSCOPE_SNMP_KEY`.** The two roots are independent
by design: rotating the session key invalidates outstanding cookies
(operators sign back in) but does not affect stored SNMP credentials.
Rotating the SNMP key invalidates every stored v3 credential but does
not log anyone out. Pick the right one for the right blast radius.

If `FLOWSCOPE_SESSION_KEY_REF` is unset, the OIDC endpoints return 503
and the legacy shared/per-token paths handle every request — same as
before this PR.

## 3. Paste the IdP config in the FlowScope UI

**Settings → Auth & tokens → OIDC SSO**. Fields:

| Field           | Entra ID value |
|-----------------|----------------|
| issuer          | `https://login.microsoftonline.com/<tenant>/v2.0` |
| client id       | the Application (client) ID from step 1 |
| client secret   | the value from "Certificates & secrets" |
| redirect URI    | `https://<your-host>/auth/callback` (must match exactly) |
| scopes          | `openid email profile` |

For Auth0 / Okta / Keycloak / Google, the **issuer** is whatever the
provider's `.well-known/openid-configuration` is served under (drop the
`/.well-known/openid-configuration` suffix). FlowScope discovers the
endpoints automatically via go-oidc.

The client secret is encrypted at rest under `FLOWSCOPE_SNMP_KEY` (same
key the webhook secrets use). Saved blanks preserve the existing
ciphertext — you only need to re-paste on rotation.

## 4. Flip the flag on and test

1. Set **enabled** to `enabled` and save.
2. Click **sign in with SSO**. You'll be redirected to your IdP, sign
   in there, and land back on the FlowScope dashboard with your email
   shown in the top-right brand bar.
3. To verify: open `GET /auth/me`. A 200 with `{ "subject": "...", "email": "..." }`
   confirms the session cookie is valid.

## 5. Rollback

Turn **enabled** off in the same form, or `PUT /api/settings/oidc`
with `enabled=false`. Users with active sessions stay signed in until
their cookie expires (24h default) or they explicitly log out — but
**new** logins go back to the shared/per-token path. There is no data
migration required.

To invalidate every outstanding session immediately, rotate
`FLOWSCOPE_SESSION_KEY_REF` (assign a new value and restart the api).
Every signed cookie minted under the old key fails verification and
users hit the login redirect.

## 6. What this PR does NOT do (deferred)

- **Group → scope mapping.** Every successful OIDC sign-in is granted
  `admin` scope in v1. A follow-up PR will read the `groups` claim
  (Entra ID's group GUIDs, Okta's group names, etc.) and map them to
  `read`/`write`/`admin` via a small config table. The scope field is
  already plumbed end to end through `internal/sessionsign` and
  `internal/authz`; only the mapping logic is missing.
- **OIDC logout federated with the IdP.** `POST /auth/logout` clears
  the FlowScope cookie only. Following the IdP's end_session_endpoint
  is a small follow-up.
- **Multi-tenant tenant detection.** Entra ID multi-tenant apps need
  per-request tenant resolution. FlowScope's single-tenant setup is the
  common case; multi-tenant support can land on top of the existing
  provider abstraction in `internal/oidc`.

## 7. Troubleshooting

| Symptom | Where to look |
|---------|---------------|
| `/auth/login` returns 503 | `FLOWSCOPE_SESSION_KEY_REF` not set, or OIDC `enabled=false`. Check api logs for "oidc login disabled". |
| `/auth/callback` returns "state mismatch" | The state cookie expired (>10 min between login and callback) or got dropped by an aggressive cookie policy. Try again. |
| `/auth/callback` returns "OIDC exchange failed" | Redirect URI in FlowScope doesn't match the IdP registration, or the client secret is wrong. Check api logs for the structured `exchange failed` line. |
| Users hit `/auth/login` but never get redirected back | Application Gateway or another proxy is stripping the session cookie. Confirm `Secure`/`SameSite=Lax`/`HttpOnly` are preserved end-to-end. |
| Session works but immediately expires | Clock skew between api replicas — the cookie carries a Unix timestamp; ±1 minute skew is tolerable, anything larger and tokens "expire" instantly. NTP your nodes. |

Each of these failure modes increments a structured log line that
operators can grep for in production. There is no silent failure mode
in the auth path — that's the project rule (CLAUDE.md "no silent
failures").
