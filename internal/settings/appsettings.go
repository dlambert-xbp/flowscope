package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func newAppSettingsStore(conn driver.Conn) AppSettingsStore {
	return &chAppSettingsStore{conn: conn}
}

type chAppSettingsStore struct {
	conn driver.Conn
}

func (s *chAppSettingsStore) Get(ctx context.Context, name string) (*AppSettingValue, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("app_settings: name required")
	}
	const q = `SELECT name, value_json, updated_at, updated_by FROM app_settings FINAL WHERE name = ?`
	var (
		v       AppSettingValue
		valJSON string
	)
	if err := s.conn.QueryRow(ctx, q, name).Scan(&v.Name, &valJSON, &v.UpdatedAt, &v.UpdatedBy); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("app_settings: get: %w", err)
	}
	if err := json.Unmarshal([]byte(valJSON), &v.Value); err != nil {
		return nil, fmt.Errorf("app_settings: decode value: %w", err)
	}
	return &v, nil
}

func (s *chAppSettingsStore) Set(ctx context.Context, v AppSettingValue, actor string) error {
	v.Name = strings.TrimSpace(v.Name)
	if v.Name == "" {
		return fmt.Errorf("app_settings: name required")
	}
	body, err := json.Marshal(v.Value)
	if err != nil {
		return fmt.Errorf("app_settings: encode value: %w", err)
	}
	const ins = `INSERT INTO app_settings (name, value_json, updated_at, updated_by) VALUES (?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins, v.Name, string(body), time.Now().UTC(), actorOr(actor)); err != nil {
		return fmt.Errorf("app_settings: insert: %w", err)
	}
	return nil
}

func (s *chAppSettingsStore) List(ctx context.Context) ([]AppSettingValue, error) {
	const q = `SELECT name, value_json, updated_at, updated_by FROM app_settings FINAL ORDER BY name`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("app_settings: list: %w", err)
	}
	defer rows.Close()
	out := make([]AppSettingValue, 0, 8)
	for rows.Next() {
		var (
			v       AppSettingValue
			valJSON string
		)
		if err := rows.Scan(&v.Name, &valJSON, &v.UpdatedAt, &v.UpdatedBy); err != nil {
			return nil, fmt.Errorf("app_settings: scan: %w", err)
		}
		if err := json.Unmarshal([]byte(valJSON), &v.Value); err != nil {
			// Skip rows with un-decodable values rather than 500 the
			// whole list — handler-side validation is the source of
			// truth for value shape.
			continue
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
