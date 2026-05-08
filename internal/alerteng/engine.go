// Package alerteng implements the FlowScope alert engine.
//
// The engine evaluates a small, opinionated set of built-in Rules on
// a ticker. Each Rule is a pure function over ClickHouse: it queries
// the data plane and returns the set of currently-violating
// (scope, group_key) tuples. The engine diffs that against the last
// known open set and writes one append-only row to alert_events for
// each transition (opened, heartbeat, closed).
//
// Per VISION.md §6 — JSON rules, no DSL, no YAML inheritance, alert
// hygiene is a feature. Auto-close (rules clearing themselves when
// the condition recovers) is built in; webhook / email / syslog
// channels arrive in a follow-up slice.
package alerteng

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Severity values. Stable strings — reused as Prometheus labels and
// in the JSON contract.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// Alert state values.
const (
	StateOpened       = "opened"
	StateHeartbeat    = "heartbeat"
	StateClosed       = "closed"
	StateAcknowledged = "acknowledged"
)

// Violation is what a Rule returns: one row per current breach. The
// engine builds (rule_id, scope, group_key) keys from these values.
type Violation struct {
	Scope    string
	GroupKey string
	Title    string
	Body     string
	Severity string
	Labels   map[string]string
}

// Rule is a single alert rule. Implementations should be cheap and
// run in a few hundred ms — the engine evaluates the full set on
// every tick.
type Rule interface {
	ID() string
	Severity() string
	Runbook() string
	Evaluate(ctx context.Context, conn driver.Conn) ([]Violation, error)
}

// Engine evaluates Rules on a tick and writes alert_events rows.
type Engine struct {
	conn            driver.Conn
	tick            time.Duration
	refreshTick     time.Duration
	stabilityWindow time.Duration
	src             RuleSettingsSource

	mu      sync.Mutex
	rules   []Rule    // guarded by mu
	version time.Time // max(updated_at) at the time `rules` was last loaded
	open    map[string]openAlert // key = rule_id|scope|group_key
}

type openAlert struct {
	severity   string
	openedAt   time.Time
	lastActive time.Time
	clearedAt  time.Time // zero while violating; set to now() on first clear tick
}

// New returns an Engine ready to Run. tick controls evaluation
// cadence; production setting is 5–10 seconds.
//
// src is optional. When non-nil the engine reloads rules from
// alert_rule_settings every refreshTick (default 60s) and swaps the
// in-flight rule slice when a newer max(updated_at) is observed —
// edits in the Settings UI propagate without a process restart.
func New(conn driver.Conn, rules []Rule, tick time.Duration) *Engine {
	if tick <= 0 {
		tick = 10 * time.Second
	}
	return &Engine{
		conn:            conn,
		rules:           rules,
		tick:            tick,
		refreshTick:     60 * time.Second,
		stabilityWindow: 60 * time.Second,
		open:            make(map[string]openAlert),
	}
}

// WithStabilityWindow sets the dwell time the condition must stay
// cleared before the engine fires a close event. 0 closes immediately
// on the first clear tick (legacy behavior). Production rules
// typically want 60s to absorb flapping.
func (e *Engine) WithStabilityWindow(d time.Duration) *Engine {
	if d < 0 {
		d = 0
	}
	e.stabilityWindow = d
	return e
}

// WithSettingsSource wires a live-edit source for rule overrides.
// Calling this before Run captures the initial version stamp so the
// next refresh tick only fires when an operator has actually changed
// something.
func (e *Engine) WithSettingsSource(src RuleSettingsSource, version time.Time) *Engine {
	e.src = src
	e.version = version
	return e
}

// Run blocks until ctx is cancelled. On the first tick it reconciles
// open state from the existing ledger so a restart doesn't dupe
// already-open alerts.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.bootstrapOpenState(ctx); err != nil {
		slog.Warn("alerteng: bootstrap open state failed", "err", err)
	}
	t := time.NewTicker(e.tick)
	defer t.Stop()

	var refreshC <-chan time.Time
	if e.src != nil {
		rt := time.NewTicker(e.refreshTick)
		defer rt.Stop()
		refreshC = rt.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.evaluate(ctx)
		case <-refreshC:
			e.refreshRules(ctx)
		}
	}
}

// refreshRules reloads from the settings source and swaps the active
// rule slice when max(updated_at) has advanced. Called on the refresh
// ticker; cheap when nothing has changed (one small SELECT).
func (e *Engine) refreshRules(ctx context.Context) {
	if e.src == nil {
		return
	}
	rules, version, err := LoadRules(ctx, e.src)
	if err != nil {
		slog.Warn("alerteng: refresh load failed", "err", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !version.After(e.version) {
		return
	}
	e.rules = rules
	e.version = version
	slog.Info("alerteng: rules reloaded", "rules", len(rules), "version", version)
}

// evaluate runs every Rule once and reconciles open state.
func (e *Engine) evaluate(ctx context.Context) {
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()

	currentlyViolating := make(map[string]bool, len(e.open))

	for _, r := range e.rules {
		violations, err := r.Evaluate(ctx, e.conn)
		if err != nil {
			slog.Warn("alerteng: rule failed", "rule", r.ID(), "err", err)
			continue
		}
		for _, v := range violations {
			key := alertKey(r.ID(), v.Scope, v.GroupKey)
			currentlyViolating[key] = true

			if existing, ok := e.open[key]; ok {
				// Heartbeat — already open. clearedAt resets to zero so
				// a brief clear tick followed by a re-fire doesn't count
				// against the stability window.
				e.open[key] = openAlert{
					severity:   existing.severity,
					openedAt:   existing.openedAt,
					lastActive: now,
				}
				e.write(ctx, alertRow{
					ts: now, ruleID: r.ID(), severity: existing.severity, state: StateHeartbeat,
					scope: v.Scope, groupKey: v.GroupKey, title: v.Title, body: v.Body, runbook: r.Runbook(),
					labels: v.Labels,
				})
			} else {
				severity := v.Severity
				if severity == "" {
					severity = r.Severity()
				}
				e.open[key] = openAlert{severity: severity, openedAt: now, lastActive: now}
				e.write(ctx, alertRow{
					ts: now, ruleID: r.ID(), severity: severity, state: StateOpened,
					scope: v.Scope, groupKey: v.GroupKey, title: v.Title, body: v.Body, runbook: r.Runbook(),
					labels: v.Labels,
				})
				slog.Info("alerteng: opened",
					"rule", r.ID(), "severity", severity,
					"scope", v.Scope, "title", v.Title,
				)
			}
		}
	}

	// Close anything that was open last tick but isn't now — except
	// require stabilityWindow of continuous clearance before firing
	// the close event so a single missed eval tick doesn't dupe-fire
	// open/close on a flapping condition.
	for key, oa := range e.open {
		if currentlyViolating[key] {
			continue
		}
		if oa.clearedAt.IsZero() {
			oa.clearedAt = now
			e.open[key] = oa
		}
		if now.Sub(oa.clearedAt) < e.stabilityWindow {
			continue
		}
		ruleID, scope, groupKey := splitAlertKey(key)
		e.write(ctx, alertRow{
			ts: now, ruleID: ruleID, severity: oa.severity, state: StateClosed,
			scope: scope, groupKey: groupKey,
			title: "auto-closed: condition cleared",
			body:  "condition cleared at evaluation tick",
		})
		slog.Info("alerteng: closed", "rule", ruleID, "scope", scope, "stable_for", now.Sub(oa.clearedAt))
		delete(e.open, key)
	}
}

// bootstrapOpenState reconciles the in-memory open set from the
// existing event ledger so a restart doesn't re-open alerts that
// were already open and doesn't close ones that should remain open.
func (e *Engine) bootstrapOpenState(ctx context.Context) error {
	const q = `
SELECT
    rule_id,
    scope,
    group_key,
    argMax(severity, ts) AS severity,
    min(ts) AS opened_at,
    max(ts) AS last_active_at,
    argMax(state, ts) AS state
FROM alert_events
WHERE ts >= now() - INTERVAL 7 DAY
GROUP BY rule_id, scope, group_key`
	rows, err := e.conn.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("alerteng: bootstrap query: %w", err)
	}
	defer rows.Close()
	loaded := 0
	for rows.Next() {
		var (
			ruleID, scope, groupKey, severity, state string
			openedAt, lastActiveAt                   time.Time
		)
		if err := rows.Scan(&ruleID, &scope, &groupKey, &severity, &openedAt, &lastActiveAt, &state); err != nil {
			return err
		}
		if state == StateClosed {
			continue
		}
		e.open[alertKey(ruleID, scope, groupKey)] = openAlert{
			severity: severity, openedAt: openedAt, lastActive: lastActiveAt,
		}
		loaded++
	}
	if loaded > 0 {
		slog.Info("alerteng: bootstrap restored open alerts", "count", loaded)
	}
	return rows.Err()
}

// alertRow is what the engine writes to alert_events.
type alertRow struct {
	ts                                                 time.Time
	ruleID, severity, state, scope, groupKey           string
	title, body, runbook                               string
	actor                                              string
	labels                                             map[string]string
}

func (e *Engine) write(ctx context.Context, r alertRow) {
	if r.actor == "" {
		r.actor = "engine"
	}
	if r.labels == nil {
		r.labels = map[string]string{}
	}
	if err := e.conn.AsyncInsert(ctx,
		`INSERT INTO alert_events
		   (ts, rule_id, severity, state, scope, group_key, title, body, runbook, actor, labels)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		false,
		r.ts, r.ruleID, r.severity, r.state, r.scope, r.groupKey,
		r.title, r.body, r.runbook, r.actor, r.labels,
	); err != nil {
		slog.Warn("alerteng: write failed", "rule", r.ruleID, "err", err)
	}
}

func alertKey(ruleID, scope, groupKey string) string {
	return ruleID + "\x1f" + scope + "\x1f" + groupKey
}

func splitAlertKey(k string) (ruleID, scope, groupKey string) {
	parts := strings.SplitN(k, "\x1f", 3)
	if len(parts) != 3 {
		return k, "", ""
	}
	return parts[0], parts[1], parts[2]
}
