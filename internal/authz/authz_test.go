package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// fakeTokens is a tiny in-memory APITokensStore for middleware
// tests. Only Verify and CountActive are exercised; the rest of the
// methods return "not implemented" errors so a slip in the middleware
// that calls into them gets caught.
//
// activeCount controls what CountActive returns. countErr forces
// CountActive to surface an error so the fail-closed branch can be
// tested. When the test wants the historical "store provisioned but
// empty" shape, leave activeCount at zero and countErr nil.
type fakeTokens struct {
	plain       string
	scope       string
	id          uuid.UUID
	used        int
	activeCount int
	countErr    error
	countCalls  int
}

func (f *fakeTokens) List(context.Context) ([]settings.APIToken, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTokens) Get(context.Context, string) (*settings.APIToken, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTokens) Create(context.Context, string, string, string) (*settings.APIToken, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeTokens) Revoke(context.Context, string, string) error {
	return errors.New("not implemented")
}
func (f *fakeTokens) Verify(_ context.Context, plaintext string) (*settings.APIToken, error) {
	if plaintext != f.plain {
		return nil, settings.ErrNotFound
	}
	return &settings.APIToken{
		ID:    f.id,
		Name:  "test",
		Scope: f.scope,
	}, nil
}
func (f *fakeTokens) MarkUsed(context.Context, string) error {
	f.used++
	return nil
}
func (f *fakeTokens) CountActive(context.Context) (int, error) {
	f.countCalls++
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.activeCount, nil
}

func okHandler(t *testing.T, wantSubject string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := SubjectFrom(r.Context())
		if s.Source != wantSubject {
			t.Errorf("subject source = %q, want %q", s.Source, wantSubject)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func req(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPut, "/api/settings/general/foo", nil)
	if token != "" {
		r.Header.Set("X-Auth-Token", token)
	}
	return r
}

func TestNoAuthConfiguredFallsThrough(t *testing.T) {
	cfg := Config{}
	mw := cfg.RequireWrite()(okHandler(t, "unauth-bypass"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req(""))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestSharedTokenAccepts(t *testing.T) {
	cfg := Config{SharedToken: "shared-secret"}
	mw := cfg.RequireWrite()(okHandler(t, "shared"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req("shared-secret"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestSharedTokenWrongValueRejects(t *testing.T) {
	cfg := Config{SharedToken: "shared-secret"}
	mw := cfg.RequireWrite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req("nope"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestPerTokenWriteAccepts(t *testing.T) {
	tk := &fakeTokens{plain: "fls_real", scope: "write", id: uuid.New(), activeCount: 1}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireWrite()(okHandler(t, "token"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req("fls_real"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if tk.used != 1 {
		t.Errorf("MarkUsed not invoked")
	}
}

func TestReadScopeRejectedFromWriteRoute(t *testing.T) {
	tk := &fakeTokens{plain: "fls_readonly", scope: "read", id: uuid.New(), activeCount: 1}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireWrite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for read scope on write route")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req("fls_readonly"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestWriteScopeRejectedFromAdminRoute(t *testing.T) {
	tk := &fakeTokens{plain: "fls_writer", scope: "write", id: uuid.New(), activeCount: 1}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireAdmin()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for write scope on admin route")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req("fls_writer"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestAdminTokenSatisfiesWriteAndAdmin(t *testing.T) {
	tk := &fakeTokens{plain: "fls_admin", scope: "admin", id: uuid.New(), activeCount: 1}
	cfg := Config{Tokens: tk}
	for _, label := range []struct {
		name string
		mw   func(http.Handler) http.Handler
	}{
		{"write", cfg.RequireWrite()},
		{"admin", cfg.RequireAdmin()},
	} {
		w := httptest.NewRecorder()
		label.mw(okHandler(t, "token")).ServeHTTP(w, req("fls_admin"))
		if w.Code != http.StatusNoContent {
			t.Errorf("%s route: status = %d, want 204", label.name, w.Code)
		}
	}
}

func TestBearerHeaderAlsoAccepted(t *testing.T) {
	cfg := Config{SharedToken: "shared"}
	mw := cfg.RequireWrite()(okHandler(t, "shared"))
	r := httptest.NewRequest(http.MethodPut, "/api/settings/x", nil)
	r.Header.Set("Authorization", "Bearer shared")
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestMissingTokenRejectedWhenAuthConfigured(t *testing.T) {
	cfg := Config{SharedToken: "x"}
	mw := cfg.RequireWrite()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, req(""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// readReq builds a GET request the way a /api/* read handler would
// see it. The read-route tests below mirror the write-route tests but
// against RequireRead.
func readReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	if token != "" {
		r.Header.Set("X-Auth-Token", token)
	}
	return r
}

func TestRequireReadNoAuthFallsThrough(t *testing.T) {
	cfg := Config{}
	mw := cfg.RequireRead()(okHandler(t, "unauth-bypass"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestRequireReadSharedTokenAccepts(t *testing.T) {
	cfg := Config{SharedToken: "shared-secret"}
	mw := cfg.RequireRead()(okHandler(t, "shared"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq("shared-secret"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestRequireReadMissingTokenRejects401(t *testing.T) {
	cfg := Config{SharedToken: "shared-secret"}
	mw := cfg.RequireRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestRequireReadWrongTokenRejects401(t *testing.T) {
	cfg := Config{SharedToken: "shared-secret"}
	mw := cfg.RequireRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq("nope"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// All three scopes (read / write / admin) must satisfy a read route —
// the scope ladder is admin > write > read.
func TestRequireReadAcceptsAllScopes(t *testing.T) {
	for _, scope := range []string{"read", "write", "admin"} {
		t.Run(scope, func(t *testing.T) {
			tk := &fakeTokens{plain: "fls_" + scope, scope: scope, id: uuid.New(), activeCount: 1}
			cfg := Config{Tokens: tk}
			mw := cfg.RequireRead()(okHandler(t, "token"))
			w := httptest.NewRecorder()
			mw.ServeHTTP(w, readReq("fls_"+scope))
			if w.Code != http.StatusNoContent {
				t.Errorf("scope=%s: status = %d, want 204", scope, w.Code)
			}
		})
	}
}

/* ------------------ zero-rows bypass behaviour (the fix) ------------------ */

// When SharedToken is empty AND the api_tokens store has zero active
// rows, the middleware MUST let requests through with the bypass
// subject — that's the zero-config dev / bootstrap UX. Pre-fix, this
// failed: c.Tokens != nil was sufficient to disable bypass, so a
// fresh install with no shared token and no minted tokens returned
// 401 on every request.
func TestBypassFiresWhenTokensStoreIsEmpty(t *testing.T) {
	tk := &fakeTokens{activeCount: 0}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireRead()(okHandler(t, "unauth-bypass"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if tk.countCalls == 0 {
		t.Errorf("CountActive should have been consulted on the bypass branch")
	}
}

// Once an operator mints a token (activeCount > 0), the gate becomes
// strict: a request with no header must get 401, not the bypass.
func TestBypassDoesNotFireWhenTokensExist(t *testing.T) {
	tk := &fakeTokens{activeCount: 1}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when tokens exist and no header was sent")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "missing X-Auth-Token") {
		t.Errorf("body = %q, want it to mention missing X-Auth-Token", got)
	}
}

// Wrong header with a non-empty store: still 401, but the "invalid"
// branch — distinct from "missing" — so the operator can tell the
// difference.
func TestWrongTokenWithPopulatedStoreReturnsInvalid(t *testing.T) {
	tk := &fakeTokens{plain: "fls_real", scope: "read", id: uuid.New(), activeCount: 1}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for a bad token")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq("fls_wrong"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, "invalid X-Auth-Token") {
		t.Errorf("body = %q, want it to mention invalid X-Auth-Token", got)
	}
}

// SharedToken set + store empty + correct shared header → accepted as
// "shared". The bypass branch must NOT short-circuit ahead of the
// shared-token check (it's predicated on SharedToken == "").
func TestSharedTokenStillUsedWhenStoreEmpty(t *testing.T) {
	tk := &fakeTokens{activeCount: 0}
	cfg := Config{SharedToken: "sh-secret", Tokens: tk}
	mw := cfg.RequireRead()(okHandler(t, "shared"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq("sh-secret"))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if tk.countCalls != 0 {
		t.Errorf("CountActive must not be called when SharedToken is set; called %d times", tk.countCalls)
	}
}

// Fail-closed: if CountActive errors, the bypass MUST NOT fire. A
// missing X-Auth-Token is rejected, the audit log is left to record
// the failure (we don't open the door on a transient DB outage).
func TestBypassFailsClosedOnCountError(t *testing.T) {
	tk := &fakeTokens{countErr: errors.New("clickhouse: connection refused")}
	cfg := Config{Tokens: tk}
	mw := cfg.RequireRead()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when CountActive errors")
	}))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (fail-closed)", w.Code)
	}
}

// Tokens==nil (e.g. api booted in a mode where the store isn't wired)
// must continue to fire the bypass alongside SharedToken=="". This
// preserves the historical behaviour and the existing
// TestNoAuthConfiguredFallsThrough case explicitly with the new code
// path going through bypassActive.
func TestBypassFiresWhenTokensStoreIsNil(t *testing.T) {
	cfg := Config{} // both fields zero
	mw := cfg.RequireRead()(okHandler(t, "unauth-bypass"))
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, readReq(""))
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}
