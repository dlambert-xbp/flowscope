package alerteng

// instance.go — adapter between settings.AlertRuleInstance and the
// engine's evaluation loop.
//
// An InstanceRule closes over (Template, ScopeSelector, params,
// InstanceID, severity) and implements the legacy Rule interface so
// the engine loop can dispatch to it without caring whether the
// underlying source is a built-in rule struct or an operator-created
// instance row. The InstanceID flows into Violation.Labels so the api
// can render the per-instance alert listing.

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dlambert-xbp/flowscope/internal/settings"
)

// InstanceSettingsSource is the slice of settings.AlertInstancesStore
// the engine actually uses. Defining it here means the engine package
// can stay independent of the full settings DI graph in tests.
type InstanceSettingsSource interface {
	List(ctx context.Context) ([]settings.AlertRuleInstance, error)
	EnsureSeed(ctx context.Context, templateID, name string, defaultParams map[string]any, defaultSeverity string) (*settings.AlertRuleInstance, error)
}

// InstanceRule wraps a Template with the operator-supplied scope and
// params. It implements Rule so the existing engine evaluation loop
// works unchanged structurally — only the source of the slice differs.
type InstanceRule struct {
	tpl        Template
	instanceID string
	scope      ScopeSelector
	params     map[string]any
	severity   string // override; empty falls back to tpl.DefaultSeverity()
	runbook    string // override; empty falls back to tpl.Runbook()
}

// NewInstanceRule constructs an InstanceRule. The engine builds these
// on every refresh tick.
func NewInstanceRule(tpl Template, inst settings.AlertRuleInstance) InstanceRule {
	params := inst.Params
	if params == nil {
		params = tpl.DefaultParams()
	}
	severity := inst.Severity
	if severity == "" {
		severity = tpl.DefaultSeverity()
	}
	return InstanceRule{
		tpl:        tpl,
		instanceID: inst.InstanceID,
		scope:      inst.Scope,
		params:     params,
		severity:   severity,
		runbook:    inst.Runbook,
	}
}

// InstanceID exposes the underlying instance id so the engine can key
// open-alerts on it.
func (r InstanceRule) InstanceID() string { return r.instanceID }

// TemplateID delegates to the underlying template.
func (r InstanceRule) TemplateID() string { return r.tpl.ID() }

// ID is the legacy Rule.ID() — returns the template id so existing
// alert_events rows continue to group by rule_id (and we additionally
// tag with instance_id on the new column).
func (r InstanceRule) ID() string { return r.tpl.ID() }

// Severity returns the effective severity for this instance.
func (r InstanceRule) Severity() string { return r.severity }

// Runbook returns the operator override when set, otherwise the
// template's default.
func (r InstanceRule) Runbook() string {
	if r.runbook != "" {
		return r.runbook
	}
	return r.tpl.Runbook()
}

// Evaluate delegates to the template's scoped evaluator and tags
// every resulting violation with the instance_id so the engine writes
// the new alert_events column. The severity override (if any) is
// applied here so a single Template implementation can serve many
// instances with different severities.
func (r InstanceRule) Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error) {
	violations, err := r.tpl.EvaluateScoped(ctx, conn, r.scope, r.params)
	if err != nil {
		return nil, err
	}
	for i := range violations {
		if violations[i].Labels == nil {
			violations[i].Labels = map[string]string{}
		}
		violations[i].Labels["instance_id"] = r.instanceID
		violations[i].Labels["template_id"] = r.tpl.ID()
		// Severity override semantics: if the instance has an explicit
		// severity, force it onto every violation. Templates that emit
		// dynamic severities (e.g. warning→critical at threshold +
		// bump) only get overridden when the operator has explicitly
		// chosen one.
		if r.severity != "" && r.severity != r.tpl.DefaultSeverity() {
			violations[i].Severity = r.severity
		}
	}
	return violations, nil
}

// LoadInstanceRules materializes the engine's rule slice from the
// instance store. Disabled rows are skipped. Unknown template IDs are
// logged and skipped — they can sit in the table without crashing the
// engine, which matters during multi-replica upgrades where a newer
// api has written rows for a template the older engine doesn't know.
//
// version is the max(updated_at) across the returned rows; the engine
// uses it to short-circuit refresh ticks when nothing has changed.
func LoadInstanceRules(ctx context.Context, src InstanceSettingsSource, registry TemplateRegistry) ([]Rule, time.Time, error) {
	if src == nil {
		// No instance source — fall back to the legacy default-rules
		// behavior so the engine still runs in environments where the
		// settings store wasn't wired (tests, degraded mode).
		return defaultRules(), time.Time{}, nil
	}
	// Seed any missing default instances so a freshly-migrated db
	// (or a new template added in this binary release) doesn't have
	// a hole in evaluation coverage. Idempotent.
	for _, t := range BuiltinTemplates() {
		if _, err := src.EnsureSeed(ctx, t.ID(),
			"Default · "+t.ID(),
			t.DefaultParams(),
			t.DefaultSeverity(),
		); err != nil {
			slog.Warn("alerteng: ensure seed failed", "template", t.ID(), "err", err)
		}
	}
	rows, err := src.List(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make([]Rule, 0, len(rows))
	var version time.Time
	for _, inst := range rows {
		if !inst.Enabled {
			continue
		}
		tpl, ok := registry[inst.TemplateID]
		if !ok {
			slog.Warn("alerteng: unknown template_id in alert_rule_instances; skipping",
				"template_id", inst.TemplateID, "instance_id", inst.InstanceID)
			continue
		}
		out = append(out, NewInstanceRule(tpl, inst))
		if inst.UpdatedAt.After(version) {
			version = inst.UpdatedAt
		}
	}
	return out, version, nil
}

// defaultRules is the legacy fallback used when no instance store is
// wired. It mirrors what DefaultRules used to return — the engine
// keeps behaving exactly like it did pre-instance for tests and
// degraded-mode deployments.
func defaultRules() []Rule {
	tpls := BuiltinTemplates()
	out := make([]Rule, 0, len(tpls))
	for _, t := range tpls {
		// Templates also satisfy Rule (since Evaluate is on the rule
		// struct). The engine sees them as Rule values.
		if r, ok := t.(Rule); ok {
			out = append(out, r)
		}
	}
	return out
}
