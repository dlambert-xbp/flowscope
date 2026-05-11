// Package authz provides the X-Auth-Token middleware that gates
// settings write endpoints. Two parallel auth modes are accepted:
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
// Either mode counts as authenticated. When neither is configured the
// middleware permits all requests but stamps subject="unauth-bypass"
// so audit rows make the gap visible. Phase 2 makes the gate strict.
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
// the per-token path, and "unauth-bypass" when no auth is configured.
type Subject struct {
	Source    string
	Actor     string
	Scope     string
	TokenID   string
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

// Config carries the credentials the middleware checks against.
// Either or both fields may be set; Tokens may be nil (e.g. when the
// crypter / DB is unavailable on api boot).
type Config struct {
	SharedToken string
	Tokens      settings.APITokensStore
}

// RequireRead returns middleware that gates a handler behind any valid
// token — read, write, or admin scope all pass. It is the right wrap
// for /api/* GET endpoints (and the alert ack/close POSTs, which the
// product treats as read-tier mutations because they don't change
// auth or configuration state). The unauth-bypass behaviour is
// identical to RequireWrite / RequireAdmin: when neither SharedToken
// nor Tokens is configured the middleware lets requests through and
// stamps Subject.Source = "unauth-bypass" so audit rows make the gap
// visible. Phase 2 makes the gate strict for every scope.
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
			plain := readToken(r)

			// No auth configured at all — let it through with a
			// stamped Subject so audit rows record the gap.
			//
			// "No auth" means: no shared token AND no active rows
			// in api_tokens. c.Tokens is always non-nil in
			// production (it's a store handle assigned at boot
			// from settings.New(...)), so a nil-only check was the
			// original bug — the bypass never fired on a fresh
			// install. We probe the store directly so a freshly
			// minted token flips the gate to strict on the next
			// request.
			//
			// Fail-closed semantics: if CountActive errors we log
			// and treat the store as non-empty. That keeps the
			// gate strict (callers will get a 401 missing/invalid
			// header) rather than opening the door on a transient
			// DB blip. Per CLAUDE.md §"No silent failures".
			if c.SharedToken == "" && c.bypassActive(r.Context()) {
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

// bypassActive reports whether the "no auth configured" fallback
// should fire. It is only consulted on the SharedToken==""  branch,
// so the DB hit is paid only on dev / bootstrap stacks. A nil Tokens
// store is treated as "no per-token mode wired" and counts as empty.
// A non-nil store is queried for its active-row count; an error from
// the store is logged and treated as "non-empty" so the gate fails
// closed rather than opening on a transient outage.
func (c Config) bypassActive(ctx context.Context) bool {
	if c.Tokens == nil {
		return true
	}
	n, err := c.Tokens.CountActive(ctx)
	if err != nil {
		slog.Warn("authz: api_tokens CountActive failed; treating store as non-empty (fail-closed)",
			"err", err,
		)
		return false
	}
	return n == 0
}
