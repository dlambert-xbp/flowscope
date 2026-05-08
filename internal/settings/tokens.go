package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// TokenPlaintextPrefix is prepended to every minted plaintext token
// so leaked secrets are easy to recognise in logs and PR diffs. The
// prefix is part of the plaintext but NOT stored separately on the
// row — token_prefix on the row is the first 6 chars of the
// post-prefix random portion (so the operator-visible "prefix" reads
// like a meaningful identifier, not just "fls_").
const TokenPlaintextPrefix = "fls_"

// validScopes is closed; api handlers should reject unknown scopes
// before they reach the store.
var validScopes = map[string]bool{"read": true, "write": true, "admin": true}

func newAPITokensStore(conn driver.Conn) APITokensStore {
	return &chAPITokensStore{
		conn:        conn,
		lastUseSeen: make(map[string]time.Time),
	}
}

type chAPITokensStore struct {
	conn driver.Conn

	// lastUseSeen throttles last_used_at writes. Without this, every
	// authenticated request would write to ClickHouse — overkill for
	// what is effectively a "when was this token last alive" stat.
	lastMu      sync.Mutex
	lastUseSeen map[string]time.Time
}

func (s *chAPITokensStore) List(ctx context.Context) ([]APIToken, error) {
	const q = `
SELECT id, name, token_prefix, scope,
       created_at, created_by, last_used_at, expires_at, revoked_at
FROM api_tokens FINAL
ORDER BY created_at DESC`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("api_tokens: list: %w", err)
	}
	defer rows.Close()
	out := make([]APIToken, 0, 8)
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scope,
			&t.CreatedAt, &t.CreatedBy, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, fmt.Errorf("api_tokens: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *chAPITokensStore) Get(ctx context.Context, id string) (*APIToken, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("api_tokens: parse id: %w", err)
	}
	const q = `
SELECT id, name, token_prefix, scope,
       created_at, created_by, last_used_at, expires_at, revoked_at
FROM api_tokens FINAL WHERE id = ?`
	var t APIToken
	if err := s.conn.QueryRow(ctx, q, uid).Scan(
		&t.ID, &t.Name, &t.Prefix, &t.Scope,
		&t.CreatedAt, &t.CreatedBy, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt,
	); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("api_tokens: get: %w", err)
	}
	return &t, nil
}

func (s *chAPITokensStore) Create(ctx context.Context, name, scope, actor string) (*APIToken, error) {
	scope = strings.TrimSpace(scope)
	if !validScopes[scope] {
		return nil, fmt.Errorf("api_tokens: invalid scope %q", scope)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("api_tokens: name required")
	}

	plaintext, err := mintTokenPlaintext()
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("api_tokens: bcrypt: %w", err)
	}
	// Prefix shown to humans is the first 6 chars after fls_ — readable
	// without leaking enough entropy to be useful.
	prefix := plaintext[len(TokenPlaintextPrefix) : len(TokenPlaintextPrefix)+6]

	now := time.Now().UTC()
	t := APIToken{
		ID:        uuid.New(),
		Name:      name,
		Prefix:    prefix,
		Scope:     scope,
		Plaintext: plaintext,
		CreatedAt: now,
		CreatedBy: actorOr(actor),
	}

	const ins = `
INSERT INTO api_tokens
   (id, name, token_prefix, token_hash, scope, created_at, created_by, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		t.ID, t.Name, t.Prefix, string(hash), t.Scope,
		t.CreatedAt, t.CreatedBy, t.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("api_tokens: insert: %w", err)
	}
	return &t, nil
}

func (s *chAPITokensStore) Revoke(ctx context.Context, id, actor string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("api_tokens: parse id: %w", err)
	}
	// Read the current row so we can preserve immutable fields and
	// only bump revoked_at + updated_at.
	cur, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	const ins = `
INSERT INTO api_tokens
   (id, name, token_prefix, token_hash, scope, created_at, created_by, last_used_at, expires_at, revoked_at, updated_at)
 SELECT ?, ?, ?, token_hash, ?, ?, ?, last_used_at, expires_at, ?, ?
   FROM api_tokens FINAL WHERE id = ? LIMIT 1`
	if err := s.conn.Exec(ctx, ins,
		uid, cur.Name, cur.Prefix, cur.Scope, cur.CreatedAt, cur.CreatedBy, now, now,
		uid,
	); err != nil {
		return fmt.Errorf("api_tokens: revoke: %w", err)
	}
	_ = actor
	return nil
}

func (s *chAPITokensStore) Verify(ctx context.Context, plaintext string) (*APIToken, error) {
	if !strings.HasPrefix(plaintext, TokenPlaintextPrefix) {
		return nil, ErrNotFound
	}
	if len(plaintext) < len(TokenPlaintextPrefix)+6 {
		return nil, ErrNotFound
	}
	prefix := plaintext[len(TokenPlaintextPrefix) : len(TokenPlaintextPrefix)+6]

	const q = `
SELECT id, name, token_prefix, token_hash, scope,
       created_at, created_by, last_used_at, expires_at, revoked_at
FROM api_tokens FINAL WHERE token_prefix = ? AND revoked_at = toDateTime64(0, 3, 'UTC')`
	rows, err := s.conn.Query(ctx, q, prefix)
	if err != nil {
		return nil, fmt.Errorf("api_tokens: verify query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			t    APIToken
			hash string
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &hash, &t.Scope,
			&t.CreatedAt, &t.CreatedBy, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, fmt.Errorf("api_tokens: verify scan: %w", err)
		}
		if !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt) {
			continue
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)); err == nil {
			return &t, nil
		}
	}
	return nil, ErrNotFound
}

func (s *chAPITokensStore) MarkUsed(ctx context.Context, id string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("api_tokens: parse id: %w", err)
	}

	now := time.Now()
	s.lastMu.Lock()
	if last, ok := s.lastUseSeen[id]; ok && now.Sub(last) < time.Minute {
		s.lastMu.Unlock()
		return nil
	}
	s.lastUseSeen[id] = now
	s.lastMu.Unlock()

	cur, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	const ins = `
INSERT INTO api_tokens
   (id, name, token_prefix, token_hash, scope, created_at, created_by, last_used_at, expires_at, revoked_at, updated_at)
 SELECT ?, ?, ?, token_hash, ?, ?, ?, ?, expires_at, revoked_at, ?
   FROM api_tokens FINAL WHERE id = ? LIMIT 1`
	if err := s.conn.Exec(ctx, ins,
		uid, cur.Name, cur.Prefix, cur.Scope, cur.CreatedAt, cur.CreatedBy,
		now.UTC(), now.UTC(),
		uid,
	); err != nil {
		return fmt.Errorf("api_tokens: mark used: %w", err)
	}
	return nil
}

func mintTokenPlaintext() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("api_tokens: rand: %w", err)
	}
	return TokenPlaintextPrefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
