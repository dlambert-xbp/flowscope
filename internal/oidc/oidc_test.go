package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Mock OpenID Provider: serves /.well-known/openid-configuration, a
// JWKS, a token endpoint, and verifies PKCE on exchange. Enough to
// exercise New(), LoginURL(), and Exchange() end-to-end with a real
// RS256 signature path.
type fakeIdP struct {
	server   *httptest.Server
	clientID string
	priv     *rsa.PrivateKey
	kid      string
	// State recorded by handlers so tests can assert on it.
	lastCodeChallenge string
	lastCode          string
	// codeVerifier the test expects to see on /token.
	expectedVerifier string
	subject          string
	email            string
}

func newFakeIdP(t *testing.T, clientID string) *fakeIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	f := &fakeIdP{
		clientID: clientID,
		priv:     priv,
		kid:      "test-key-1",
		lastCode: "test-auth-code-xyz",
		subject:  "user-42",
		email:    "user@example.com",
	}
	mux := http.NewServeMux()
	f.server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 f.server.URL,
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"jwks_uri":               f.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": f.kid, "n": n, "e": e},
			},
		})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		// PKCE check
		if got := r.Form.Get("code_verifier"); got != f.expectedVerifier {
			http.Error(w, "code_verifier mismatch", http.StatusBadRequest)
			return
		}
		if r.Form.Get("code") != f.lastCode {
			http.Error(w, "code mismatch", http.StatusBadRequest)
			return
		}
		idToken := f.signIDToken(t, map[string]any{
			"iss":   f.server.URL,
			"sub":   f.subject,
			"aud":   f.clientID,
			"exp":   time.Now().Add(time.Hour).Unix(),
			"iat":   time.Now().Unix(),
			"email": f.email,
			"name":  "Test User",
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access",
			"id_token":     idToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})

	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIdP) signIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": f.kid}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.priv, crypto.SHA256, h[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestNewDiscoversIssuer(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"openid", "email"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.oauth.RedirectURL != "https://app/callback" {
		t.Errorf("redirect URL = %q", p.oauth.RedirectURL)
	}
}

func TestNewAppendsOpenIDScope(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"email"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := strings.Join(p.oauth.Scopes, " ")
	if !strings.Contains(got, "openid") {
		t.Errorf("scopes %q missing openid", got)
	}
}

func TestLoginURLEncodesPKCEChallenge(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"openid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	verifier := "the-quick-brown-fox-jumps-over-the-lazy-dog-1"
	wantChal := codeChallengeS256(verifier)
	u := p.LoginURL("state-abc", verifier)
	if !strings.Contains(u, "code_challenge="+wantChal) {
		t.Errorf("login URL %q missing code_challenge=%s", u, wantChal)
	}
	if !strings.Contains(u, "code_challenge_method=S256") {
		t.Errorf("login URL %q missing S256", u)
	}
	if !strings.Contains(u, "state=state-abc") {
		t.Errorf("login URL %q missing state", u)
	}
}

func TestExchangeReturnsClaims(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	f.expectedVerifier = "test-verifier-abc-1234567890abcdefghijk"
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"openid", "email"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claims, err := p.Exchange(context.Background(), f.lastCode, f.expectedVerifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Sub != f.subject {
		t.Errorf("sub = %q want %q", claims.Sub, f.subject)
	}
	if claims.Email != f.email {
		t.Errorf("email = %q want %q", claims.Email, f.email)
	}
	if claims.Name == "" {
		t.Errorf("name empty")
	}
}

func TestExchangeRejectsWrongVerifier(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	f.expectedVerifier = "right-verifier"
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"openid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Exchange(context.Background(), f.lastCode, "wrong-verifier")
	if err == nil {
		t.Fatalf("expected exchange error with wrong verifier")
	}
}

// Reachability test: ensure codeChallengeS256 matches a known RFC 7636
// test vector. From the appendix:
//
//	code_verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
//	code_challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
func TestPKCEMatchesRFC7636Vector(t *testing.T) {
	const v = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := codeChallengeS256(v); got != want {
		t.Errorf("PKCE S256: got %q want %q", got, want)
	}
}

// Ensure errors surface a useful message — the api logs them and we
// want operators to be able to grep for the issuer URL on failure.
func TestNewMissingFieldsErrors(t *testing.T) {
	cases := []struct{ issuer, cid, redir string }{
		{"", "x", "https://x"},
		{"https://x", "", "https://x"},
		{"https://x", "x", ""},
	}
	for _, c := range cases {
		_, err := New(context.Background(), c.issuer, c.cid, "secret", c.redir, []string{"openid"})
		if err == nil {
			t.Errorf("expected error for %+v", c)
		}
	}
}

// Sanity: ensure we can sign + verify a fresh ID token via the verifier
// path (no callback round trip). This catches issues if the verifier
// were wired incorrectly.
func TestVerifierAcceptsFreshIDToken(t *testing.T) {
	f := newFakeIdP(t, "client-1")
	p, err := New(context.Background(), f.server.URL, "client-1", "secret", "https://app/callback", []string{"openid"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	idt := f.signIDToken(t, map[string]any{
		"iss": f.server.URL,
		"sub": "u",
		"aud": "client-1",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	if _, err := p.verifier.Verify(context.Background(), idt); err != nil {
		t.Errorf("verifier rejected fresh token: %v", err)
	}
}
