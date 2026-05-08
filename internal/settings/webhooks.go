package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

// validKinds is closed; api handlers reject unknown kinds.
var validKinds = map[string]bool{
	"slack":     true,
	"teams":     true,
	"pagerduty": true,
	"http":      true,
}

// validSeverities mirrors the alert engine's set; used for filter validation.
var validSeverities = map[string]bool{
	"critical": true,
	"warning":  true,
	"info":     true,
}

func newWebhooksStore(conn driver.Conn, crypter *snmpx.Crypter) WebhooksStore {
	return &chWebhooksStore{conn: conn, crypter: crypter}
}

type chWebhooksStore struct {
	conn    driver.Conn
	crypter *snmpx.Crypter
}

func (s *chWebhooksStore) List(ctx context.Context) ([]Webhook, error) {
	const q = `
SELECT id, name, kind, url,
       length(secret_ct) > 0 AS has_secret,
       header_template_json, enabled, severity_filter, updated_at, updated_by
FROM webhook_endpoints FINAL
ORDER BY name`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("webhooks: list: %w", err)
	}
	defer rows.Close()
	out := make([]Webhook, 0, 8)
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

func (s *chWebhooksStore) Get(ctx context.Context, id string) (*Webhook, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("webhooks: parse id: %w", err)
	}
	const q = `
SELECT id, name, kind, url,
       length(secret_ct) > 0 AS has_secret,
       header_template_json, enabled, severity_filter, updated_at, updated_by
FROM webhook_endpoints FINAL WHERE id = ?`
	w, err := scanWebhook(s.conn.QueryRow(ctx, q, uid))
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (s *chWebhooksStore) Upsert(ctx context.Context, w Webhook, actor string) (*Webhook, error) {
	w.Kind = strings.ToLower(strings.TrimSpace(w.Kind))
	if !validKinds[w.Kind] {
		return nil, fmt.Errorf("webhooks: invalid kind %q", w.Kind)
	}
	w.Name = strings.TrimSpace(w.Name)
	if w.Name == "" {
		return nil, fmt.Errorf("webhooks: name required")
	}
	if w.URL == "" {
		return nil, fmt.Errorf("webhooks: url required")
	}
	for _, sev := range w.SeverityFilter {
		if !validSeverities[strings.ToLower(sev)] {
			return nil, fmt.Errorf("webhooks: invalid severity %q in filter", sev)
		}
	}

	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}

	// Encrypt secret if provided. Empty secret + HasSecret=false means
	// "no secret"; empty secret + HasSecret=true means "preserve
	// existing", which we resolve by leaving secret_ct alone — i.e.
	// reading current row first. Caller (handler) sets HasSecret based
	// on whether they want to preserve.
	var secretCT string
	if w.Secret != "" {
		if s.crypter == nil {
			return nil, ErrSecretsDisabled
		}
		ct, err := s.crypter.Encrypt(w.Secret)
		if err != nil {
			return nil, fmt.Errorf("webhooks: encrypt secret: %w", err)
		}
		secretCT = ct
	} else if w.HasSecret {
		// Preserve existing
		const q = `SELECT secret_ct FROM webhook_endpoints FINAL WHERE id = ?`
		_ = s.conn.QueryRow(ctx, q, w.ID).Scan(&secretCT)
	}

	headerJSON := "{}"
	if len(w.HeaderTemplate) > 0 {
		b, err := json.Marshal(w.HeaderTemplate)
		if err != nil {
			return nil, fmt.Errorf("webhooks: encode headers: %w", err)
		}
		headerJSON = string(b)
	}
	severityFilter := strings.Join(w.SeverityFilter, ",")
	if severityFilter == "" {
		severityFilter = "critical,warning,info"
	}
	enabled := uint8(0)
	if w.Enabled {
		enabled = 1
	}
	now := time.Now().UTC()
	const ins = `
INSERT INTO webhook_endpoints
   (id, name, kind, url, secret_ct, header_template_json, enabled, severity_filter, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		w.ID, w.Name, w.Kind, w.URL, secretCT, headerJSON,
		enabled, severityFilter, now, actorOr(actor),
	); err != nil {
		return nil, fmt.Errorf("webhooks: insert: %w", err)
	}
	w.UpdatedAt = now
	w.UpdatedBy = actorOr(actor)
	w.Secret = ""
	w.HasSecret = secretCT != ""
	return &w, nil
}

func (s *chWebhooksStore) Delete(ctx context.Context, id, actor string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("webhooks: parse id: %w", err)
	}
	const q = `ALTER TABLE webhook_endpoints DELETE WHERE id = ?`
	if err := s.conn.Exec(ctx, q, uid); err != nil {
		return fmt.Errorf("webhooks: delete: %w", err)
	}
	_ = actor
	return nil
}

func scanWebhook(r rowScanner) (*Webhook, error) {
	var (
		w          Webhook
		hasSecret  uint8
		headerJSON string
		enabled    uint8
		sevFilter  string
	)
	if err := r.Scan(&w.ID, &w.Name, &w.Kind, &w.URL, &hasSecret,
		&headerJSON, &enabled, &sevFilter, &w.UpdatedAt, &w.UpdatedBy); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("webhooks: scan: %w", err)
	}
	w.HasSecret = hasSecret == 1
	w.Enabled = enabled == 1
	if sevFilter != "" {
		w.SeverityFilter = strings.Split(sevFilter, ",")
	}
	if headerJSON != "" {
		_ = json.Unmarshal([]byte(headerJSON), &w.HeaderTemplate)
	}
	return &w, nil
}
