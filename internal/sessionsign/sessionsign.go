// Package sessionsign provides HMAC-SHA256 signed session cookies for
// FlowScope's OIDC login flow (TASKS.md P3 #23 / VISION.md §4).
//
// The cookie payload is stateless — every request reads only the
// cookie itself, no DB hit on the hot path. Revocation, when needed,
// is performed via the oidc_sessions table (see migration 000011 and
// internal/oidc/revoke.go), but verifying a cookie does not consult
// the table by default.
//
// Wire layout (URL-safe base64 of):
//
//	id|subject|email|scope|expires_unix|sig
//
// where sig = HMAC-SHA256(secret, "id|subject|email|scope|expires_unix").
//
//   - id is a UUID (string form) the api mints at login time and stamps
//     into oidc_sessions only when a revocation is later required.
//   - email may be empty if the IdP did not return one.
//   - scope is the same vocabulary as internal/authz ("read" / "write" /
//     "admin"). Phase 2.5 maps OIDC group claims to scopes; v1 defaults
//     to "admin" if the IdP login is configured, on the assumption that
//     the customer wires the IdP only for trusted operators. Documented
//     in docs/oidc-setup.md as the rollback-by-flag follow-up.
//
// Distinct from FLOWSCOPE_SNMP_KEY per Session E spec: rotating the
// session key invalidates outstanding cookies (operators log back in)
// but does NOT corrupt SNMP credential storage. The two roots are
// independent on purpose.
package sessionsign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Payload is the decoded session-cookie body.
type Payload struct {
	ID       string
	Subject  string
	Email    string
	Scope    string
	Expires  time.Time
}

// Signer wraps an HMAC key. Construct via New; safe for concurrent use.
type Signer struct {
	key []byte
}

// New returns a Signer that uses key as the HMAC secret. The key must
// be at least 16 bytes (128 bits) — same minimum as snmpx.Crypter for
// consistency. Operators get this from FLOWSCOPE_SESSION_KEY_REF.
func New(key string) (*Signer, error) {
	if len(key) < 16 {
		return nil, fmt.Errorf("sessionsign: key must be ≥ 16 bytes (got %d)", len(key))
	}
	return &Signer{key: []byte(key)}, nil
}

// Sign encodes p and returns a URL-safe base64 token suitable for use
// as a cookie value.
//
// Constraint: fields MUST NOT contain the '|' byte. The current
// callers (OIDC login) supply UUIDs, email addresses, and the closed
// vocabulary {read,write,admin} — none of which permit pipes. If a
// future caller needs pipe-containing data, switch to a tagged JSON
// or length-prefixed encoding rather than papering over here.
func (s *Signer) Sign(p Payload) string {
	body := strings.Join([]string{
		p.ID,
		p.Subject,
		p.Email,
		p.Scope,
		strconv.FormatInt(p.Expires.Unix(), 10),
	}, "|")
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	sig := mac.Sum(nil)
	full := body + "|" + base64.RawURLEncoding.EncodeToString(sig)
	return base64.RawURLEncoding.EncodeToString([]byte(full))
}

// ErrInvalid is returned for tampered, malformed, or wrong-key tokens.
var ErrInvalid = errors.New("sessionsign: invalid token")

// ErrExpired is returned for a well-signed token whose expiry has
// passed. Distinct from ErrInvalid so the api can return 401 with
// WWW-Authenticate: oidc and let the UI auto-redirect to /auth/login.
var ErrExpired = errors.New("sessionsign: token expired")

// Verify decodes token, checks the HMAC, and rejects expired payloads.
// Returns ErrInvalid for tampered / malformed input, ErrExpired for a
// well-signed but stale token. Subjects are never trusted without a
// successful Verify; callers that need a quick "is there a session
// cookie at all" should use Decode (which does not check the HMAC).
func (s *Signer) Verify(token string) (Payload, error) {
	if token == "" {
		return Payload{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	full := string(raw)
	// Split off the trailing sig from the body. The body itself may
	// contain pipes inside fields (none of ours do, but encode for it
	// anyway) — we use the LAST pipe as the boundary.
	i := strings.LastIndexByte(full, '|')
	if i < 0 {
		return Payload{}, ErrInvalid
	}
	body, sigStr := full[:i], full[i+1:]
	gotSig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	wantSig := mac.Sum(nil)
	if !hmac.Equal(gotSig, wantSig) {
		return Payload{}, ErrInvalid
	}
	parts := strings.SplitN(body, "|", 5)
	if len(parts) != 5 {
		return Payload{}, ErrInvalid
	}
	expUnix, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return Payload{}, ErrInvalid
	}
	p := Payload{
		ID:      parts[0],
		Subject: parts[1],
		Email:   parts[2],
		Scope:   parts[3],
		Expires: time.Unix(expUnix, 0),
	}
	if time.Now().After(p.Expires) {
		return p, ErrExpired
	}
	return p, nil
}
