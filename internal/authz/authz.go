// Package authz provides the X-Auth-Token middleware that gates
// settings write endpoints. Three parallel auth modes are accepted:
//
//   - Session cookie (Phase 2): HMAC-SHA256 signed cookie minted by
//     the OIDC callback handler (see cmd/api/auth.go and
//     internal/sessionsign). Highest priority — checked first when a
//     SessionSource is wired in Config. Expired cookies return 401
//     with WWW-Authenticate: oidc so the UI can auto-redirect.
//
//   - Shared token (legacy / Phase 1): a single value sourced from
//     FLOWSCOPE_AUTH_TOKEN, applied uniformly. Useful for bootstrap
//     and for environments that don't have a per-user identity yet.
//
//   - Per-token API tokens: minted via /api/settings/tokens, hashed
//     in ClickHouse, with scope (read | write | admin). Authorised
//     handlers receive token metadata via Subject(ctx) so the audit
//     log can record who acted.
//
// Any mode counts as authenticated. When none of the three is
// configured the middleware permits all requests but stamps
// subject="unauth-bypass" so audit rows make the gap visible. Phase 2
// makes the gate strict.
package authz

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// Subject carries the authenticated identity into request handlers.
// Source is "shared" for the legacy single-token path, "token" for
// the per-token path, "session" for the Phase 2 OIDC signed-cookie
// path, and "unauth-bypass" when no auth is configured.
type Subject struct {
	Source  string
	Actor   string
	Scope   string
	TokenID string
	// Email is populated for session subjects so audit rows can
	// record both the IdP sub claim (Actor) and the human-readable
	// email when present. Per-token / shared paths leave it empty.
	Email string
}

type ctxKey struct{}

// WithSubject attaches s to ctx — primarily used by tests.
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// SubjectFrom returns the Subject attached by the middleware (or the
// zero value if no middleware ran). The zero value is treated as
// "anonymous" by audit-recording code.
func SubjectFrom(ctx context.Context) Subject {
	v, _ := ctx.Value(ctxKey{}).(Subject)
	return v
}

// SessionSource verifies a signed session cookie on a request. nil
// disables session auth entirely — the legacy shared/per-token paths
// still work. Returning ErrSessionExpired (vs. ErrSessionInvalid)
// lets the middleware respond 401 with WWW-Authenticate: oidc so the
// UI can auto-redirect to /auth/login.
//
// The interface stays in this package (not internal/oidc) so authz
// doesn't take a dependency on the OIDC implementation — cmd/api
// wires a small adapter that implements this interface using the
// sessionsign.Signer.
type SessionSource interface {
	Verify(r *http.Request) (Subject, error)
}

// ErrSessionExpired is returned by SessionSource.Verify when the
// cookie is well-formed and well-signed but past its expiry. The
// middleware translates this to 401 + WWW-Authenticate: oidc.
var ErrSessionExpired = errors.New("authz: session expired")

// ErrSessionInvalid is returned by SessionSource.Verify when the
// cookie is absent, malformed, or fails the signature check. The
// middleware falls through to shared/per-token/bypass on this error,
// preserving backward compatibility when OIDC is configured but a
// given request authenticates via a different mechanism (curl with
// X-Auth-Token, ansible scripts, etc.).
var ErrSessionInvalid = errors.New("authz: session invalid")

// Config carries the credentials the middleware checks against.
// Any field may be unset; Tokens may be nil (e.g. when the crypter /
// DB is unavailable on api boot); Sessions may be nil (OIDC not
// configured).
type Config struct {
	SharedToken string
	Tokens      settings.APITokensStore
	// Sessions is the optional OIDC session-cookie verifier. When set
	// it is checked FIRST on every gated request. nil disables session
	// auth — shared/per-token paths still work.
	Sessions SessionSource
}

// RequireRead returns middleware that gates a handler behind any valid
// token — read, write, or admin scope all pass. It is the right wrap
// for /api/* GET endpoints (and the alert ack/close POSTs, which the
// product treats as read-tier mutations because they don't change
// auth or configuration state). The unauth-bypass behaviour is
// identical to RequireWrite / RequireAdmin: when neither SharedToken
// nor Tokens nor Sessions is configured the middleware lets requests
// through and stamps Subject.Source = "unauth-bypass" so audit rows
// make the gap visible. Phase 2 makes the gate strict for every scope.
func (c Config) RequireRead() func(http.Handler) http.Handler {
	return c.requireScope("read")
}

// RequireWrite returns middleware that allows the request through
// when at least one credential matches a write-or-admin grant. It is
// suitable for PUT / POST / DELETE on /api/settings/*. GET handlers
// should wrap with RequireRead, not this.
func (c Config) RequireWrite() func(http.Handler) http.Handler {
	return c.requireScope("write")
}

// RequireAdmin gates handlers that mutate auth state itself
// (creating / revoking API tokens, OIDC config). Same as RequireWrite
// but with the higher scope.
func (c Config) RequireAdmin() func(http.Handler) http.Handler {
	return c.requireScope("admin")
}

func (c Config) requireScope(min string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Session cookie first — highest priority when OIDC is
			// configured. Three outcomes:
			//
			//   1. valid session → stamp Subject, proceed (after scope
			//      check)
			//   2. expired       → 401 + WWW-Authenticate: oidc so the
			//      UI can auto-redirect
			//   3. invalid/absent → fall through to the legacy paths;
			//      lets curl users keep working with X-Auth-Token
			//      while OIDC is the primary path for humans
			if c.Sessions != nil {
				sub, err := c.Sessions.Verify(r)
				if err == nil {
					if !scopeAtLeast(sub.Scope, min) {
						slog.Info("session scope insufficient",
							"subject", sub.Actor,
							"have", sub.Scope,
							"need", min,
							"path", r.URL.Path,
						)
						http.Error(w, "session scope insufficient", http.StatusForbidden)
						return
					}
					next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
					return
				}
				if errors.Is(err, ErrSessionExpired) {
					slog.Info("session expired", "path", r.URL.Path)
					w.Header().Set("WWW-Authenticate", `oidc realm="flowscope"`)
					http.Error(w, "session expired", http.StatusUnauthorized)
					return
				}
				// ErrSessionInvalid (and anything else) → fall through.
			}

			plain := readToken(r)

			// No auth configured at all — let it through with a
			// stamped Subject so audit rows record the gap.
			if c.SharedToken == "" && c.Tokens == nil {
				ctx := WithSubject(r.Context(), Subject{
					Source: "unauth-bypass",
					Actor:  "anonymous",
					Scope:  "admin", // bypass scope is permissive by design
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if plain == "" {
				http.Error(w, "missing X-Auth-Token", http.StatusUnauthorized)
				return
			}

			// Shared token first — fastest path, no DB hit.
			if c.SharedToken != "" && plain == c.SharedToken {
				ctx := WithSubject(r.Context(), Subject{
					Source: "shared",
					Actor:  "shared",
					Scope:  "admin",
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if c.Tokens != nil {
				tok, err := c.Tokens.Verify(r.Context(), plain)
				if err == nil && tok != nil {
					if !scopeAtLeast(tok.Scope, min) {
						http.Error(w, "token scope insufficient", http.StatusForbidden)
						return
					}
					_ = c.Tokens.MarkUsed(r.Context(), tok.ID.String())
					ctx := WithSubject(r.Context(), Subject{
						Source:  "token",
						Actor:   tok.Name,
						Scope:   tok.Scope,
						TokenID: tok.ID.String(),
					})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				if err != nil && !errors.Is(err, settings.ErrNotFound) {
					http.Error(w, "auth error", http.StatusInternalServerError)
					return
				}
			}

			http.Error(w, "invalid X-Auth-Token", http.StatusUnauthorized)
		})
	}
}

// readToken extracts a token from the canonical X-Auth-Token header,
// the Authorization: Bearer header, or a token query parameter (last
// resort, useful for browser-only tools that can't set headers).
func readToken(r *http.Request) string {
	if t := r.Header.Get("X-Auth-Token"); t != "" {
		return strings.TrimSpace(t)
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return strings.TrimSpace(r.URL.Query().Get("auth_token"))
}

// scopeAtLeast returns true if have grants what min requires.
// Order: admin > write > read.
func scopeAtLeast(have, min string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return rank[have] >= rank[min]
}
