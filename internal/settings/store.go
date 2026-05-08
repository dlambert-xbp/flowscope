package settings

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

// ErrNotFound is returned when a Get-by-id finds no row. Handlers map
// it to 404 so callers don't have to string-match on driver error
// shapes.
var ErrNotFound = errors.New("settings: not found")

// Store bundles the per-resource store interfaces. cmd/api/main.go
// constructs one Store and threads it into the HTTP handlers — each
// handler reaches for the field it needs. Splitting per resource
// (rather than one giant interface) keeps the test surface honest:
// you mock only what the handler under test calls.
type Store struct {
	CustomServices CustomServicesStore
	APITokens      APITokensStore
	Allowlist      AllowlistStore
	AppSettings    AppSettingsStore
	AlertRules     AlertRulesStore
	Webhooks       WebhooksStore
	OIDC           OIDCStore
}

// New returns a Store backed by ClickHouse. crypter is required for
// the resources that seal secrets (webhooks, OIDC client secret); a
// nil crypter produces stores that refuse mutations on those
// resources (returning ErrSecretsDisabled) so the api can still serve
// reads in degraded mode.
func New(conn driver.Conn, crypter *snmpx.Crypter) *Store {
	return &Store{
		CustomServices: newCustomServicesStore(conn),
		APITokens:      newAPITokensStore(conn),
		Allowlist:      newAllowlistStore(conn),
		AppSettings:    newAppSettingsStore(conn),
		AlertRules:     newAlertRulesStore(conn),
		Webhooks:       newWebhooksStore(conn, crypter),
		OIDC:           newOIDCStore(conn, crypter),
	}
}

// ErrSecretsDisabled is returned by store methods that need the
// optional Crypter when none was provided to New.
var ErrSecretsDisabled = errors.New("settings: secret encryption disabled (FLOWSCOPE_SNMP_KEY not set)")

/* ----------------------------- Per-resource interfaces ----------------------------- */

type CustomServicesStore interface {
	List(ctx context.Context) ([]CustomService, error)
	Get(ctx context.Context, id string) (*CustomService, error)
	Upsert(ctx context.Context, s CustomService, actor string) (*CustomService, error)
	Delete(ctx context.Context, id, actor string) error
}

type APITokensStore interface {
	List(ctx context.Context) ([]APIToken, error)
	Get(ctx context.Context, id string) (*APIToken, error)
	Create(ctx context.Context, name, scope, actor string) (*APIToken, error)
	Revoke(ctx context.Context, id, actor string) error
	// Verify hashes the supplied plaintext and returns the matching
	// token if any (and not revoked / not expired). Used by the auth
	// middleware on /api/settings writes.
	Verify(ctx context.Context, plaintext string) (*APIToken, error)
	// MarkUsed bumps last_used_at, throttled to ~1 write per minute
	// per token by the implementation.
	MarkUsed(ctx context.Context, id string) error
}

type AllowlistStore interface {
	List(ctx context.Context) ([]ExporterEntry, error)
	Get(ctx context.Context, exporter string) (*ExporterEntry, error)
	Upsert(ctx context.Context, e ExporterEntry, actor string) error
	Delete(ctx context.Context, exporter, actor string) error
}

type AppSettingsStore interface {
	Get(ctx context.Context, name string) (*AppSettingValue, error)
	Set(ctx context.Context, v AppSettingValue, actor string) error
	List(ctx context.Context) ([]AppSettingValue, error)
}

type AlertRulesStore interface {
	List(ctx context.Context) ([]AlertRuleSetting, error)
	Get(ctx context.Context, ruleID string) (*AlertRuleSetting, error)
	Upsert(ctx context.Context, s AlertRuleSetting, actor string) error
}

type WebhooksStore interface {
	List(ctx context.Context) ([]Webhook, error)
	Get(ctx context.Context, id string) (*Webhook, error)
	Upsert(ctx context.Context, w Webhook, actor string) (*Webhook, error)
	Delete(ctx context.Context, id, actor string) error
}

type OIDCStore interface {
	Get(ctx context.Context) (*OIDCConfig, error)
	Set(ctx context.Context, c OIDCConfig, actor string) error
}
