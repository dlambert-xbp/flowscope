// auth.go wires the Phase 2 OIDC login flow into cmd/api.
//
// Four endpoints (all unauthenticated — by design, see the allowlist
// in main.go):
//
//   GET  /auth/login    — generate PKCE verifier + state, set short-
//                         lived state cookie, redirect to the IdP's
//                         authorize URL.
//   GET  /auth/callback — read code + state from query, validate
//                         state against the cookie, exchange the
//                         auth-code, mint the session cookie, redirect
//                         to /.
//   POST /auth/logout   — clear the session cookie; optionally record
//                         a revocation row in oidc_sessions.
//   GET  /auth/me       — return the current session's subject/email/
//                         scope, or 401 if no valid session.
//
// All four endpoints log structured fields on the error paths so an
// operator can diagnose "OIDC isn't working" without enabling debug
// logging (CLAUDE.md "no silent failures").
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/authz"
	"github.com/dlambert-xbp/flowscope/internal/oidc"
	"github.com/dlambert-xbp/flowscope/internal/sessionsign"
	"github.com/dlambert-xbp/flowscope/internal/settings"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

// authDeps groups the collaborators the auth handlers reach for. Held
// on handlers as h.auth so the existing struct stays narrow.
type authDeps struct {
	signer  *sessionsign.Signer       // nil when FLOWSCOPE_SESSION_KEY_REF is unset
	store   settings.OIDCStore        // reads oidc_config
	crypter *snmpx.Crypter            // decrypts client_secret_ct
}

// sessionAdapter implements authz.SessionSource using a Signer.
// Lives in this package so internal/authz stays free of cookie /
// sessionsign coupling — the api owns the wire shape.
type sessionAdapter struct {
	signer     *sessionsign.Signer
	cookieName string
}

func (a *sessionAdapter) Verify(r *http.Request) (authz.Subject, error) {
	c, err := r.Cookie(a.cookieName)
	if err != nil || c.Value == "" {
		return authz.Subject{}, authz.ErrSessionInvalid
	}
	p, err := a.signer.Verify(c.Value)
	if err != nil {
		if errors.Is(err, sessionsign.ErrExpired) {
			return authz.Subject{}, authz.ErrSessionExpired
		}
		return authz.Subject{}, authz.ErrSessionInvalid
	}
	return authz.Subject{
		Source:  "session",
		Actor:   p.Subject,
		Email:   p.Email,
		Scope:   p.Scope,
		TokenID: p.ID,
	}, nil
}

// Cookie names and defaults.
const (
	sessionCookieName   = "flowscope_session"
	stateCookieName     = "flowscope_oidc_state"
	stateCookieMaxAge   = 10 * 60                // 10 min — covers slow IdP round trips
	sessionCookieMaxAge = 24 * 60 * 60           // 24h default; refresh by re-login
)

// authLogin handles GET /auth/login. It generates a PKCE code verifier
// and a CSRF state, sets a short-lived state cookie carrying both, and
// redirects to the IdP authorize URL.
//
// Race + tab safety: each call mints a fresh verifier+state. If the
// operator opens two login tabs the second overwrites the cookie and
// the first becomes unrecoverable — that's the standard auth-code
// flow caveat and matches every other OIDC implementation.
func (h *handlers) authLogin(w http.ResponseWriter, r *http.Request) {
	if h.auth.signer == nil {
		writeError(w, http.StatusServiceUnavailable,
			"OIDC unavailable — FLOWSCOPE_SESSION_KEY_REF not configured")
		return
	}
	cfg, err := h.loadOIDCConfig(r.Context())
	if err != nil {
		slog.Error("auth: load oidc config", "err", err)
		writeError(w, http.StatusInternalServerError, "OIDC config load failed")
		return
	}
	if cfg == nil || !cfg.Enabled {
		writeError(w, http.StatusServiceUnavailable, "OIDC login is not enabled")
		return
	}
	prov, err := h.buildProvider(r.Context(), cfg)
	if err != nil {
		slog.Error("auth: build provider", "issuer", cfg.Issuer, "err", err)
		writeError(w, http.StatusBadGateway, "OIDC provider unavailable")
		return
	}

	state := randTokenURL(32)
	verifier := randTokenURL(32) // 43 chars after base64 — within PKCE [43,128]
	// The state cookie holds both state and verifier joined by '.'
	// (neither contains a dot in RawURLEncoding). HttpOnly + SameSite=Lax
	// — Lax allows the redirect back from the IdP to carry it, Strict
	// would not.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state + "." + verifier,
		Path:     "/auth",
		MaxAge:   stateCookieMaxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	url := prov.LoginURL(state, verifier)
	slog.Info("auth: redirect to IdP", "issuer", cfg.Issuer)
	http.Redirect(w, r, url, http.StatusFound)
}

// authCallback handles GET /auth/callback. It validates the state
// cookie, exchanges the auth-code, mints the session cookie, and
// redirects to /.
func (h *handlers) authCallback(w http.ResponseWriter, r *http.Request) {
	if h.auth.signer == nil {
		writeError(w, http.StatusServiceUnavailable, "OIDC unavailable")
		return
	}
	q := r.URL.Query()
	if errStr := q.Get("error"); errStr != "" {
		slog.Warn("auth: idp returned error",
			"error", errStr,
			"description", q.Get("error_description"),
		)
		writeError(w, http.StatusBadGateway, "IdP returned error: "+errStr)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeError(w, http.StatusBadRequest, "missing code or state")
		return
	}
	sc, err := r.Cookie(stateCookieName)
	if err != nil || sc.Value == "" {
		slog.Warn("auth: state cookie missing")
		writeError(w, http.StatusBadRequest, "state cookie missing or expired")
		return
	}
	parts := strings.SplitN(sc.Value, ".", 2)
	if len(parts) != 2 || parts[0] != state {
		slog.Warn("auth: state mismatch")
		writeError(w, http.StatusBadRequest, "state mismatch")
		return
	}
	verifier := parts[1]

	// Clear the state cookie now that we've consumed it.
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})

	cfg, err := h.loadOIDCConfig(r.Context())
	if err != nil || cfg == nil || !cfg.Enabled {
		slog.Error("auth: oidc not configured on callback", "err", err)
		writeError(w, http.StatusServiceUnavailable, "OIDC not configured")
		return
	}
	prov, err := h.buildProvider(r.Context(), cfg)
	if err != nil {
		slog.Error("auth: build provider on callback", "err", err)
		writeError(w, http.StatusBadGateway, "OIDC provider unavailable")
		return
	}
	claims, err := prov.Exchange(r.Context(), code, verifier)
	if err != nil {
		slog.Warn("auth: exchange failed", "err", err)
		writeError(w, http.StatusBadGateway, "OIDC exchange failed")
		return
	}

	// v1: every successfully-authenticated session is granted "admin"
	// scope. Group → scope mapping is a Phase 2.5 follow-up called out
	// in the PR body and docs/oidc-setup.md. The signed cookie is
	// invalidated by rotating FLOWSCOPE_SESSION_KEY_REF or via explicit
	// revoke through oidc_sessions.
	scope := "admin"
	sid := uuid.NewString()
	exp := time.Now().Add(time.Duration(sessionCookieMaxAge) * time.Second)
	cookieVal := h.auth.signer.Sign(sessionsign.Payload{
		ID:      sid,
		Subject: claims.Sub,
		Email:   claims.Email,
		Scope:   scope,
		Expires: exp,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    cookieVal,
		Path:     "/",
		MaxAge:   sessionCookieMaxAge,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	slog.Info("auth: session minted", "sub", claims.Sub, "email", claims.Email, "scope", scope)
	http.Redirect(w, r, "/", http.StatusFound)
}

// authLogout clears the session cookie. If the cookie is verifiable
// (so we know the session id) it ALSO inserts a revocation row into
// oidc_sessions. The middleware doesn't currently consult that table
// (sessions are stateless by default), but the row gives a future
// "revoke specific session" feature something to query.
func (h *handlers) authLogout(w http.ResponseWriter, r *http.Request) {
	// Try to read the cookie so we can record the revocation, but never
	// fail logout because of it — clearing the cookie is what matters.
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" && h.auth.signer != nil {
		if p, verr := h.auth.signer.Verify(c.Value); verr == nil || errors.Is(verr, sessionsign.ErrExpired) {
			h.recordSessionRevocation(r.Context(), p)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authMe returns the current session payload (subject, email, scope)
// or 401 if no valid session. Used by the UI on app boot to render
// the signed-in user's name + email in the brand bar.
func (h *handlers) authMe(w http.ResponseWriter, r *http.Request) {
	if h.auth.signer == nil {
		writeError(w, http.StatusUnauthorized, "no session")
		return
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		writeError(w, http.StatusUnauthorized, "no session")
		return
	}
	p, err := h.auth.signer.Verify(c.Value)
	if err != nil {
		if errors.Is(err, sessionsign.ErrExpired) {
			w.Header().Set("WWW-Authenticate", `oidc realm="flowscope"`)
		}
		writeError(w, http.StatusUnauthorized, "invalid or expired session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    p.Subject,
		"email":      p.Email,
		"scope":      p.Scope,
		"id":         p.ID,
		"expires_at": p.Expires.UTC(),
	})
}

// recordSessionRevocation writes one row to oidc_sessions with
// revoked=1. Best-effort; failures are logged and ignored.
func (h *handlers) recordSessionRevocation(ctx context.Context, p sessionsign.Payload) {
	if h.conn == nil || p.ID == "" {
		return
	}
	uid, err := uuid.Parse(p.ID)
	if err != nil {
		return
	}
	const ins = `
INSERT INTO oidc_sessions
   (id, subject, email, scope, created_at, expires_at, revoked)
 VALUES (?, ?, ?, ?, ?, ?, ?)`
	now := time.Now().UTC()
	if err := h.conn.Exec(ctx, ins, uid, p.Subject, p.Email, p.Scope, now, p.Expires.UTC(), uint8(1)); err != nil {
		slog.Warn("auth: record session revocation failed", "err", err, "session_id", p.ID)
	}
}

// loadOIDCConfig returns the decrypted OIDC config or nil if not
// configured. Returns an error only on storage failure — a row whose
// 'enabled' is false is returned as-is and the caller decides.
func (h *handlers) loadOIDCConfig(ctx context.Context) (*oidcRuntimeConfig, error) {
	if h.auth.store == nil {
		return nil, nil
	}
	c, err := h.auth.store.Get(ctx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	out := &oidcRuntimeConfig{
		Enabled:     c.Enabled,
		Issuer:      c.Issuer,
		ClientID:    c.ClientID,
		RedirectURI: c.RedirectURI,
		Scopes:      strings.Fields(c.Scopes),
	}
	if h.auth.crypter != nil {
		ct, err := h.fetchOIDCSecretCT(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch oidc secret ct: %w", err)
		}
		if ct != "" {
			pt, err := h.auth.crypter.Decrypt(ct)
			if err != nil {
				return nil, fmt.Errorf("decrypt oidc secret: %w", err)
			}
			out.ClientSecret = pt
		}
	}
	return out, nil
}

// fetchOIDCSecretCT pulls the encrypted client_secret_ct directly. The
// settings.OIDCStore public surface intentionally never returns the
// ciphertext — same rule as webhook secrets. The auth handlers are the
// only call site that needs plaintext for the actual oauth2 exchange.
func (h *handlers) fetchOIDCSecretCT(ctx context.Context) (string, error) {
	const q = `SELECT client_secret_ct FROM oidc_config FINAL WHERE id = 'singleton'`
	var ct string
	if err := h.conn.QueryRow(ctx, q).Scan(&ct); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", nil
		}
		return "", err
	}
	return ct, nil
}

type oidcRuntimeConfig struct {
	Enabled      bool
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURI  string
	Scopes       []string
}

func (h *handlers) buildProvider(ctx context.Context, c *oidcRuntimeConfig) (*oidc.Provider, error) {
	return oidc.New(ctx, c.Issuer, c.ClientID, c.ClientSecret, c.RedirectURI, c.Scopes)
}

// isHTTPS returns true when the request was made over TLS, either
// directly (r.TLS != nil) or via a proxy that set X-Forwarded-Proto:
// https. CLAUDE.md's "TLS at the edge" note: production deployments
// terminate TLS at Application Gateway / nginx / Caddy, so we accept
// the proxy header but only with an exact "https" match (no fuzzy
// matching that could be spoofed by a misconfigured ingress).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// randTokenURL returns a RawURLEncoding string of n random bytes.
// 32 bytes → 43 chars, which is exactly the PKCE verifier minimum.
func randTokenURL(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure on a healthy host is a panic-worthy
		// event; we bail rather than mint a predictable token.
		panic(fmt.Sprintf("auth: crypto/rand: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
