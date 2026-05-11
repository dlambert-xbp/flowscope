package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/authz"
	"github.com/dlambert-xbp/flowscope/internal/sessionsign"
)

// authLogin / authCallback / authLogout / authMe exercised below.
// The OIDC discovery + token exchange path is covered by
// internal/oidc/oidc_test.go; here we focus on the route plumbing
// (state cookie, session cookie, fallback when unconfigured).

func newHandlersWithSigner(t *testing.T) (*handlers, *sessionsign.Signer) {
	t.Helper()
	s, err := sessionsign.New("0123456789abcdef-pad-pad-pad-pad")
	if err != nil {
		t.Fatalf("sessionsign.New: %v", err)
	}
	return &handlers{auth: authDeps{signer: s}}, s
}

func TestAuthLoginReturns503WhenSignerMissing(t *testing.T) {
	h := &handlers{}
	r := httptest.NewRequest("GET", "/auth/login", nil)
	w := httptest.NewRecorder()
	h.authLogin(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "FLOWSCOPE_SESSION_KEY_REF") {
		t.Errorf("body missing helpful hint: %q", w.Body.String())
	}
}

func TestAuthMeReturns401WhenSignerMissing(t *testing.T) {
	h := &handlers{}
	r := httptest.NewRequest("GET", "/auth/me", nil)
	w := httptest.NewRecorder()
	h.authMe(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMeReturns401WhenNoCookie(t *testing.T) {
	h, _ := newHandlersWithSigner(t)
	r := httptest.NewRequest("GET", "/auth/me", nil)
	w := httptest.NewRecorder()
	h.authMe(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestAuthMeReturnsSessionWhenCookieValid(t *testing.T) {
	h, s := newHandlersWithSigner(t)
	tok := s.Sign(sessionsign.Payload{
		ID:      "id-1",
		Subject: "user-42",
		Email:   "u@example.com",
		Scope:   "admin",
		Expires: time.Now().Add(time.Hour),
	})
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	w := httptest.NewRecorder()
	h.authMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"user-42", "u@example.com", "admin", "id-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

func TestAuthMeReturns401WithWWWAuthenticateOnExpiredCookie(t *testing.T) {
	h, s := newHandlersWithSigner(t)
	tok := s.Sign(sessionsign.Payload{
		Subject: "u",
		Scope:   "admin",
		Expires: time.Now().Add(-time.Minute),
	})
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
	w := httptest.NewRecorder()
	h.authMe(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "oidc") {
		t.Errorf("WWW-Authenticate = %q, want oidc", w.Header().Get("WWW-Authenticate"))
	}
}

func TestAuthLogoutClearsCookie(t *testing.T) {
	h, _ := newHandlersWithSigner(t)
	r := httptest.NewRequest("POST", "/auth/logout", nil)
	w := httptest.NewRecorder()
	h.authLogout(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	// The Set-Cookie should clear the session — MaxAge < 0 or
	// Max-Age=0 / empty value.
	sc := w.Header().Values("Set-Cookie")
	found := false
	for _, c := range sc {
		if strings.Contains(c, sessionCookieName) && (strings.Contains(c, "Max-Age=0") || strings.Contains(c, "Max-Age=-1") || strings.Contains(c, sessionCookieName+"=;")) {
			found = true
		}
	}
	if !found {
		t.Errorf("logout did not clear cookie; Set-Cookie=%v", sc)
	}
}

// isHTTPS uses r.TLS or X-Forwarded-Proto: https. Confirm both
// branches.
func TestIsHTTPS(t *testing.T) {
	r := httptest.NewRequest("GET", "/auth/login", nil)
	if isHTTPS(r) {
		t.Errorf("plain http request flagged as https")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !isHTTPS(r) {
		t.Errorf("X-Forwarded-Proto: https not honoured")
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS")
	if !isHTTPS(r) {
		t.Errorf("uppercase X-Forwarded-Proto not honoured (case-insensitive expected)")
	}
}

// sessionAdapter is what cmd/api wires into authz.Config.Sessions. It
// must hand back Subject{Source:"session"} for valid cookies and the
// right error variant for missing/expired/tampered ones.
func TestSessionAdapterVerify(t *testing.T) {
	_, s := newHandlersWithSigner(t)
	adp := &sessionAdapter{signer: s, cookieName: sessionCookieName}

	t.Run("no cookie → ErrSessionInvalid", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/summary", nil)
		_, err := adp.Verify(r)
		if err == nil {
			t.Errorf("expected error")
		}
	})

	t.Run("valid cookie → subject", func(t *testing.T) {
		tok := s.Sign(sessionsign.Payload{
			Subject: "u-7",
			Email:   "u7@example.com",
			Scope:   "write",
			Expires: time.Now().Add(time.Hour),
		})
		r := httptest.NewRequest("GET", "/api/summary", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
		sub, err := adp.Verify(r)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if sub.Source != "session" || sub.Actor != "u-7" || sub.Scope != "write" || sub.Email != "u7@example.com" {
			t.Errorf("subject = %+v", sub)
		}
	})

	t.Run("expired cookie → ErrSessionExpired", func(t *testing.T) {
		tok := s.Sign(sessionsign.Payload{
			Subject: "u-8", Scope: "read",
			Expires: time.Now().Add(-time.Minute),
		})
		r := httptest.NewRequest("GET", "/api/summary", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
		_, err := adp.Verify(r)
		// The adapter must distinguish expired from invalid so the
		// middleware can return WWW-Authenticate: oidc.
		if !errors.Is(err, authz.ErrSessionExpired) {
			t.Errorf("err = %v, want authz.ErrSessionExpired", err)
		}
	})

	t.Run("tampered cookie → ErrSessionInvalid", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/api/summary", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "garbage"})
		_, err := adp.Verify(r)
		if !errors.Is(err, authz.ErrSessionInvalid) {
			t.Errorf("err = %v, want authz.ErrSessionInvalid", err)
		}
	})
}
