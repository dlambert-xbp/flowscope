package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// fakeTokens is a tiny in-memory APITokensStore for middleware
// tests. Only Verify is exercised; the rest of the methods return
// "not implemented" errors so a slip in the middleware that calls
// into them gets caught.
type fakeTokens struct {
	plain string
	scope string
	id    uuid.UUID
	used  int
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
	tk := &fakeTokens{plain: "fls_real", scope: "write", id: uuid.New()}
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
	tk := &fakeTokens{plain: "fls_readonly", scope: "read", id: uuid.New()}
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
	tk := &fakeTokens{plain: "fls_writer", scope: "write", id: uuid.New()}
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
	tk := &fakeTokens{plain: "fls_admin", scope: "admin", id: uuid.New()}
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
			tk := &fakeTokens{plain: "fls_" + scope, scope: scope, id: uuid.New()}
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
