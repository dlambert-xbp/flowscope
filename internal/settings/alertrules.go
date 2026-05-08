package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func newAlertRulesStore(conn driver.Conn) AlertRulesStore {
	return &chAlertRulesStore{conn: conn}
}

type chAlertRulesStore struct {
	conn driver.Conn
}

func (s *chAlertRulesStore) List(ctx context.Context) ([]AlertRuleSetting, error) {
	const q = `
SELECT rule_id, enabled, severity, params_json, runbook, channels, updated_at, updated_by
FROM alert_rule_settings FINAL
ORDER BY rule_id`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("alert_rule_settings: list: %w", err)
	}
	defer rows.Close()
	out := make([]AlertRuleSetting, 0, 4)
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *chAlertRulesStore) Get(ctx context.Context, ruleID string) (*AlertRuleSetting, error) {
	ruleID = strings.TrimSpace(ruleID)
	if ruleID == "" {
		return nil, fmt.Errorf("alert_rule_settings: rule_id required")
	}
	const q = `
SELECT rule_id, enabled, severity, params_json, runbook, channels, updated_at, updated_by
FROM alert_rule_settings FINAL WHERE rule_id = ?`
	row := s.conn.QueryRow(ctx, q, ruleID)
	r, err := scanAlertRule(row)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *chAlertRulesStore) Upsert(ctx context.Context, r AlertRuleSetting, actor string) error {
	r.RuleID = strings.TrimSpace(r.RuleID)
	if r.RuleID == "" {
		return fmt.Errorf("alert_rule_settings: rule_id required")
	}
	enabled := uint8(0)
	if r.Enabled {
		enabled = 1
	}
	paramsJSON := "{}"
	if r.Params != nil {
		b, err := json.Marshal(r.Params)
		if err != nil {
			return fmt.Errorf("alert_rule_settings: encode params: %w", err)
		}
		paramsJSON = string(b)
	}
	channelsJSON := "[]"
	if len(r.Channels) > 0 {
		b, err := json.Marshal(r.Channels)
		if err != nil {
			return fmt.Errorf("alert_rule_settings: encode channels: %w", err)
		}
		channelsJSON = string(b)
	}
	const ins = `
INSERT INTO alert_rule_settings
   (rule_id, enabled, severity, params_json, runbook, channels, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		r.RuleID, enabled, r.Severity, paramsJSON, r.Runbook, channelsJSON,
		time.Now().UTC(), actorOr(actor),
	); err != nil {
		return fmt.Errorf("alert_rule_settings: insert: %w", err)
	}
	return nil
}

func scanAlertRule(r rowScanner) (*AlertRuleSetting, error) {
	var (
		out          AlertRuleSetting
		enabled      uint8
		paramsJSON   string
		channelsJSON string
	)
	if err := r.Scan(&out.RuleID, &enabled, &out.Severity, &paramsJSON, &out.Runbook,
		&channelsJSON, &out.UpdatedAt, &out.UpdatedBy); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("alert_rule_settings: scan: %w", err)
	}
	out.Enabled = enabled == 1
	if paramsJSON != "" {
		// Decode as a free-form map so the api can echo back whatever
		// shape the rule loader was happy with on PUT.
		var params any
		if err := json.Unmarshal([]byte(paramsJSON), &params); err == nil {
			out.Params = params
		}
	}
	if channelsJSON != "" {
		var ch []string
		if err := json.Unmarshal([]byte(channelsJSON), &ch); err == nil {
			out.Channels = ch
		}
	}
	return &out, nil
}
