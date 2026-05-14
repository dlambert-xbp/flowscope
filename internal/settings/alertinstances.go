package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// AlertInstancesStore is the per-resource interface for the
// alert_rule_instances table. The api uses it for CRUD; the alert
// engine uses List on its refresh tick to materialize evaluators.
//
// Identity:
//   - Operator-created rows have instance_id = 'inst_<uuid>'.
//   - Seed rows have instance_id = 'seed_<template_id>' so the
//     migration's deterministic seeding stays idempotent and the api
//     can lazy-create a missing seed without race-checking by uuid.
type AlertInstancesStore interface {
	List(ctx context.Context) ([]AlertRuleInstance, error)
	ListByTemplate(ctx context.Context, templateID string) ([]AlertRuleInstance, error)
	Get(ctx context.Context, instanceID string) (*AlertRuleInstance, error)
	Create(ctx context.Context, in AlertRuleInstance, actor string) (*AlertRuleInstance, error)
	Update(ctx context.Context, in AlertRuleInstance, actor string) (*AlertRuleInstance, error)
	Delete(ctx context.Context, instanceID, actor string) error
	// EnsureSeed lazy-creates the per-template default instance if
	// none exists. Idempotent. The api calls it on first read so
	// templates added after migration 000017 get a default row
	// without requiring a follow-up migration.
	EnsureSeed(ctx context.Context, templateID, name string, defaultParams map[string]any, defaultSeverity string) (*AlertRuleInstance, error)
}

func newAlertInstancesStore(conn driver.Conn) AlertInstancesStore {
	return &chAlertInstancesStore{conn: conn}
}

type chAlertInstancesStore struct {
	conn driver.Conn
}

const seedPrefix = "seed_"

// SeedInstanceID returns the deterministic instance_id used for seed
// rows. Exposed so the engine bootstrap can compute the same id when
// reattributing pre-migration open alerts.
func SeedInstanceID(templateID string) string { return seedPrefix + templateID }

// IsSeedID reports whether the id is in the deterministic seed form.
// Operator-created instances use 'inst_<uuid>' and never collide.
func IsSeedID(id string) bool { return strings.HasPrefix(id, seedPrefix) }

func (s *chAlertInstancesStore) List(ctx context.Context) ([]AlertRuleInstance, error) {
	const q = `
SELECT instance_id, template_id, name, enabled, severity,
       scope_json, params_json, runbook, channels,
       is_seed, created_at, updated_at, updated_by
FROM alert_rule_instances FINAL
WHERE updated_by != '__deleted__'
ORDER BY template_id, is_seed DESC, name`
	return s.queryRows(ctx, q)
}

func (s *chAlertInstancesStore) ListByTemplate(ctx context.Context, templateID string) ([]AlertRuleInstance, error) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return nil, fmt.Errorf("alert_rule_instances: template_id required")
	}
	const q = `
SELECT instance_id, template_id, name, enabled, severity,
       scope_json, params_json, runbook, channels,
       is_seed, created_at, updated_at, updated_by
FROM alert_rule_instances FINAL
WHERE template_id = ? AND updated_by != '__deleted__'
ORDER BY is_seed DESC, name`
	rows, err := s.conn.Query(ctx, q, templateID)
	if err != nil {
		return nil, fmt.Errorf("alert_rule_instances: list: %w", err)
	}
	defer rows.Close()
	return scanInstances(rows)
}

func (s *chAlertInstancesStore) Get(ctx context.Context, instanceID string) (*AlertRuleInstance, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return nil, fmt.Errorf("alert_rule_instances: instance_id required")
	}
	const q = `
SELECT instance_id, template_id, name, enabled, severity,
       scope_json, params_json, runbook, channels,
       is_seed, created_at, updated_at, updated_by
FROM alert_rule_instances FINAL
WHERE instance_id = ? AND updated_by != '__deleted__'`
	row := s.conn.QueryRow(ctx, q, instanceID)
	r, err := scanInstance(row)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *chAlertInstancesStore) Create(ctx context.Context, in AlertRuleInstance, actor string) (*AlertRuleInstance, error) {
	if in.TemplateID == "" {
		return nil, fmt.Errorf("alert_rule_instances: template_id required")
	}
	if in.Name == "" {
		return nil, fmt.Errorf("alert_rule_instances: name required")
	}
	if in.InstanceID == "" {
		in.InstanceID = "inst_" + uuid.NewString()
	}
	if IsSeedID(in.InstanceID) && !in.IsSeed {
		return nil, fmt.Errorf("alert_rule_instances: 'seed_' id prefix is reserved for is_seed=true rows")
	}
	now := time.Now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	in.UpdatedBy = actorOr(actor)
	if err := s.upsert(ctx, in); err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *chAlertInstancesStore) Update(ctx context.Context, in AlertRuleInstance, actor string) (*AlertRuleInstance, error) {
	if in.InstanceID == "" {
		return nil, fmt.Errorf("alert_rule_instances: instance_id required")
	}
	cur, err := s.Get(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	// Preserve fields the operator should not be able to change.
	in.TemplateID = cur.TemplateID
	in.IsSeed = cur.IsSeed
	in.CreatedAt = cur.CreatedAt
	in.UpdatedAt = time.Now().UTC()
	in.UpdatedBy = actorOr(actor)
	if err := s.upsert(ctx, in); err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *chAlertInstancesStore) Delete(ctx context.Context, instanceID, actor string) error {
	cur, err := s.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	if cur.IsSeed {
		return fmt.Errorf("alert_rule_instances: seed instances cannot be deleted (disable instead)")
	}
	// Soft-delete via tombstone — ReplacingMergeTree collapses on
	// updated_at, so a fresh row with updated_by='__deleted__' wins.
	// The List query filters those out. Avoids ALTER TABLE DELETE
	// for what should be a cheap mutation.
	cur.UpdatedAt = time.Now().UTC()
	cur.UpdatedBy = "__deleted__"
	cur.Enabled = false
	return s.upsert(ctx, *cur)
}

func (s *chAlertInstancesStore) EnsureSeed(ctx context.Context, templateID, name string, defaultParams map[string]any, defaultSeverity string) (*AlertRuleInstance, error) {
	id := SeedInstanceID(templateID)
	if cur, err := s.Get(ctx, id); err == nil {
		return cur, nil
	}
	now := time.Now().UTC()
	in := AlertRuleInstance{
		InstanceID: id,
		TemplateID: templateID,
		Name:       name,
		Enabled:    true,
		Severity:   defaultSeverity,
		Scope:      ScopeSelector{},
		Params:     defaultParams,
		IsSeed:     true,
		CreatedAt:  now,
		UpdatedAt:  now,
		UpdatedBy:  "system",
	}
	if err := s.upsert(ctx, in); err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *chAlertInstancesStore) upsert(ctx context.Context, in AlertRuleInstance) error {
	enabled := uint8(0)
	if in.Enabled {
		enabled = 1
	}
	isSeed := uint8(0)
	if in.IsSeed {
		isSeed = 1
	}
	scopeJSON, err := json.Marshal(in.Scope)
	if err != nil {
		return fmt.Errorf("alert_rule_instances: encode scope: %w", err)
	}
	paramsJSON := []byte("{}")
	if in.Params != nil {
		paramsJSON, err = json.Marshal(in.Params)
		if err != nil {
			return fmt.Errorf("alert_rule_instances: encode params: %w", err)
		}
	}
	channelsJSON := []byte("[]")
	if len(in.Channels) > 0 {
		channelsJSON, err = json.Marshal(in.Channels)
		if err != nil {
			return fmt.Errorf("alert_rule_instances: encode channels: %w", err)
		}
	}
	const ins = `
INSERT INTO alert_rule_instances
   (instance_id, template_id, name, enabled, severity,
    scope_json, params_json, runbook, channels,
    is_seed, created_at, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		in.InstanceID, in.TemplateID, in.Name, enabled, in.Severity,
		string(scopeJSON), string(paramsJSON), in.Runbook, string(channelsJSON),
		isSeed, in.CreatedAt, in.UpdatedAt, in.UpdatedBy,
	); err != nil {
		return fmt.Errorf("alert_rule_instances: insert: %w", err)
	}
	return nil
}

func (s *chAlertInstancesStore) queryRows(ctx context.Context, q string) ([]AlertRuleInstance, error) {
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("alert_rule_instances: query: %w", err)
	}
	defer rows.Close()
	return scanInstances(rows)
}

func scanInstances(rows driver.Rows) ([]AlertRuleInstance, error) {
	out := make([]AlertRuleInstance, 0, 8)
	for rows.Next() {
		r, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanInstance(r rowScanner) (*AlertRuleInstance, error) {
	var (
		out          AlertRuleInstance
		enabled      uint8
		isSeed       uint8
		scopeJSON    string
		paramsJSON   string
		channelsJSON string
	)
	if err := r.Scan(
		&out.InstanceID, &out.TemplateID, &out.Name, &enabled, &out.Severity,
		&scopeJSON, &paramsJSON, &out.Runbook, &channelsJSON,
		&isSeed, &out.CreatedAt, &out.UpdatedAt, &out.UpdatedBy,
	); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("alert_rule_instances: scan: %w", err)
	}
	out.Enabled = enabled == 1
	out.IsSeed = isSeed == 1
	if scopeJSON != "" {
		_ = json.Unmarshal([]byte(scopeJSON), &out.Scope)
	}
	if paramsJSON != "" {
		var p map[string]any
		if err := json.Unmarshal([]byte(paramsJSON), &p); err == nil {
			out.Params = p
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
