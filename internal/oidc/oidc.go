// Package oidc wraps github.com/coreos/go-oidc and golang.org/x/oauth2
// into the small surface FlowScope's auth-code+PKCE login flow needs:
//
//   - New(...) — discover the issuer, set up the verifier and oauth2
//     config.
//   - LoginURL(state, codeVerifier) — build the authorize URL with
//     a PKCE S256 challenge derived from codeVerifier.
//   - Exchange(ctx, code, codeVerifier) — exchange the auth-code, verify
//     the ID token, return the extracted claims.
//
// The library handles JWKS rotation under the hood (refresh on cache
// miss). We deliberately do not roll our own JWT validation — see
// CLAUDE.md's "OIDC is famously easy to get wrong" note. coreos/go-oidc
// is the well-trodden path.
//
// Phase 2 scope: ID token only. Access / refresh tokens are received
// but not stored anywhere; the session cookie is the only thing that
// outlives the callback. Group-claim → RBAC scope mapping is deferred
// to Phase 2.5 and called out in the PR body.
package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Provider holds the discovered issuer metadata, the ID-token verifier,
// and the oauth2 config the callback handler reuses. Safe for
// concurrent use after New returns.
type Provider struct {
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	oauth    *oauth2.Config
}

// Claims is the small subset of ID-token claims FlowScope cares about.
// Groups is filled when the IdP returns a "groups" claim — Entra ID
// does when the app registration's manifest has groupMembershipClaims
// set. v1 of FlowScope reads it but does not yet map it to scope (see
// docs/oidc-setup.md).
type Claims struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

// New discovers issuer and builds a verifier + oauth2 config. The
// scopes slice MUST include "openid" — the verifier requires an ID
// token in the response. We append "openid" defensively if the caller
// forgot.
func New(ctx context.Context, issuer, clientID, clientSecret, redirectURL string, scopes []string) (*Provider, error) {
	if issuer == "" || clientID == "" || redirectURL == "" {
		return nil, errors.New("oidc: issuer, client_id, redirect_url required")
	}
	p, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", issuer, err)
	}
	scopes = ensureOpenID(scopes)
	verifier := p.Verifier(&gooidc.Config{ClientID: clientID})
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}
	return &Provider{provider: p, verifier: verifier, oauth: cfg}, nil
}

// ensureOpenID returns scopes with "openid" included exactly once.
func ensureOpenID(scopes []string) []string {
	for _, s := range scopes {
		if strings.EqualFold(s, "openid") {
			return scopes
		}
	}
	return append([]string{"openid"}, scopes...)
}

// LoginURL returns the IdP authorize URL with an S256 PKCE challenge
// derived from codeVerifier. state is echoed back on the callback so
// the api can match the cookie-bound CSRF token.
//
// PKCE encoding choice (called out in the PR body): we use S256 with
// base64.RawURLEncoding of SHA-256(codeVerifier). The verifier itself
// must be 43–128 chars from the unreserved set [A-Za-z0-9-._~]; the
// api generates 32 random bytes encoded as RawURLEncoding (43 chars).
func (p *Provider) LoginURL(state, codeVerifier string) string {
	chal := codeChallengeS256(codeVerifier)
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", chal),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// codeChallengeS256 returns base64.RawURLEncoding(SHA-256(verifier)).
// Exposed as a package func so tests can pin the encoding.
func codeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Exchange swaps an auth-code for tokens, validates the ID token, and
// returns extracted claims. The codeVerifier MUST be the same value
// LoginURL was given so the IdP's PKCE check passes.
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier string) (*Claims, error) {
	tok, err := p.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", codeVerifier),
	)
	if err != nil {
		return nil, fmt.Errorf("oidc: exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("oidc: response missing id_token")
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	var c Claims
	if err := idt.Claims(&c); err != nil {
		return nil, fmt.Errorf("oidc: parse id_token claims: %w", err)
	}
	if c.Sub == "" {
		return nil, errors.New("oidc: id_token has no sub claim")
	}
	return &c, nil
}
