package settings

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

const oidcSingletonID = "singleton"

// LoginFlowStatusPhase2 is the user-visible message we attach to OIDC
// reads. Centralised here so the api and the UI agree on the wording
// without one hard-coding it.
const LoginFlowStatusPhase2 = "Phase 2 — login flow not yet active. Configure here so it's ready for the rollout."

func newOIDCStore(conn driver.Conn, crypter *snmpx.Crypter) OIDCStore {
	return &chOIDCStore{conn: conn, crypter: crypter}
}

type chOIDCStore struct {
	conn    driver.Conn
	crypter *snmpx.Crypter
}

func (s *chOIDCStore) Get(ctx context.Context) (*OIDCConfig, error) {
	const q = `
SELECT enabled, issuer, client_id,
       length(client_secret_ct) > 0 AS has_secret,
       redirect_uri, scopes, updated_at, updated_by
FROM oidc_config FINAL WHERE id = ?`
	var (
		c         OIDCConfig
		enabled   uint8
		hasSecret uint8
	)
	err := s.conn.QueryRow(ctx, q, oidcSingletonID).Scan(
		&enabled, &c.Issuer, &c.ClientID, &hasSecret,
		&c.RedirectURI, &c.Scopes, &c.UpdatedAt, &c.UpdatedBy,
	)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			// Return default empty config so the UI can render a fresh
			// form on first load.
			return &OIDCConfig{LoginFlowStatus: LoginFlowStatusPhase2}, nil
		}
		return nil, fmt.Errorf("oidc: get: %w", err)
	}
	c.Enabled = enabled == 1
	c.HasSecret = hasSecret == 1
	c.LoginFlowStatus = LoginFlowStatusPhase2
	return &c, nil
}

func (s *chOIDCStore) Set(ctx context.Context, c OIDCConfig, actor string) error {
	c.Issuer = strings.TrimSpace(c.Issuer)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.RedirectURI = strings.TrimSpace(c.RedirectURI)
	if c.Enabled {
		// Soft validation: when an operator turns the flag on we still
		// don't gate routes (Phase 2), but we want minimum viable
		// fields populated so the rollout doesn't surface a half-empty
		// row. A bad config saved while the flag is off is fine.
		if c.Issuer == "" || c.ClientID == "" || c.RedirectURI == "" {
			return fmt.Errorf("oidc: enabled requires issuer, client_id, redirect_uri")
		}
	}

	var secretCT string
	switch {
	case c.ClientSecret != "":
		if s.crypter == nil {
			return ErrSecretsDisabled
		}
		ct, err := s.crypter.Encrypt(c.ClientSecret)
		if err != nil {
			return fmt.Errorf("oidc: encrypt secret: %w", err)
		}
		secretCT = ct
	case c.HasSecret:
		// Preserve existing
		const q = `SELECT client_secret_ct FROM oidc_config FINAL WHERE id = ?`
		_ = s.conn.QueryRow(ctx, q, oidcSingletonID).Scan(&secretCT)
	}

	if c.Scopes == "" {
		c.Scopes = "openid email profile"
	}
	enabled := uint8(0)
	if c.Enabled {
		enabled = 1
	}
	const ins = `
INSERT INTO oidc_config
   (id, enabled, issuer, client_id, client_secret_ct, redirect_uri, scopes, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		oidcSingletonID, enabled, c.Issuer, c.ClientID, secretCT,
		c.RedirectURI, c.Scopes, time.Now().UTC(), actorOr(actor),
	); err != nil {
		return fmt.Errorf("oidc: insert: %w", err)
	}
	return nil
}
