// Package notifier implements the FlowScope webhook dispatcher.
//
// The dispatcher polls alert_events for new opened / closed
// transitions, joins each event against the enabled webhook
// endpoints, applies the per-endpoint severity_filter, and POSTs a
// kind-appropriate body (Slack Block Kit, Teams MessageCard,
// PagerDuty Events API v2, or raw JSON) with bounded exponential
// retry. Permanent failures land in audit_events so an operator can
// see at a glance which channel went silent and why.
//
// Per ARCHITECTURE.md: the dispatcher runs in-process inside
// cmd/alert alongside the engine. It inherits the single-replica
// constraint (no leader election yet — P0 #5) and that's documented
// as a known tradeoff. Cursor / idempotency live in two dedicated
// ClickHouse tables introduced by 000009_webhook_dispatcher.sql.
package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event is the payload the dispatcher feeds to formatters. It is the
// transport shape between the alert engine ledger and the wire body
// each provider expects. Kept independent of internal/store types so
// the dispatcher can also synthesise a test event for the
// /webhooks/{id}/test endpoint.
type Event struct {
	// Signature is sha256(ts|rule_id|scope|group_key|state) hex. The
	// dispatcher uses this as the idempotency key across attempts and
	// passes it through to PagerDuty as dedup_key.
	Signature string `json:"signature"`

	Timestamp time.Time         `json:"ts"`
	RuleID    string            `json:"rule_id"`
	Severity  string            `json:"severity"`
	State     string            `json:"state"` // 'opened' | 'closed'
	Scope     string            `json:"scope"`
	GroupKey  string            `json:"group_key"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Runbook   string            `json:"runbook,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Endpoint is the dispatcher's view of one webhook_endpoints row.
// The handler resolves header_template_json into Headers and decrypts
// secret_ct into Secret before calling Dispatch.
type Endpoint struct {
	ID             string
	Name           string
	Kind           string // 'slack' | 'teams' | 'pagerduty' | 'http'
	URL            string
	Secret         string            // plaintext, post-decrypt
	Headers        map[string]string // already-resolved header_template
	SeverityFilter []string          // subset of {'critical','warning','info'}
}

// SeverityAllowed reports whether ev passes the endpoint's severity
// filter. Empty filter = accept-all (matches the engine's earlier
// behaviour where rules with no override fire to every channel).
func (e Endpoint) SeverityAllowed(severity string) bool {
	if len(e.SeverityFilter) == 0 {
		return true
	}
	for _, s := range e.SeverityFilter {
		if strings.EqualFold(strings.TrimSpace(s), severity) {
			return true
		}
	}
	return false
}

// FormatBody builds the wire body and content-type header for ev
// against the given endpoint kind. Pure function (no I/O) so it can
// be tested with table-driven cases.
func FormatBody(ep Endpoint, ev Event) (body []byte, contentType string, err error) {
	switch strings.ToLower(strings.TrimSpace(ep.Kind)) {
	case "slack":
		b, err := formatSlack(ev)
		return b, "application/json", err
	case "teams":
		b, err := formatTeams(ev)
		return b, "application/json", err
	case "pagerduty":
		b, err := formatPagerDuty(ep, ev)
		return b, "application/json", err
	case "http":
		b, err := formatHTTP(ev)
		return b, "application/json", err
	default:
		return nil, "", fmt.Errorf("notifier: unsupported webhook kind %q", ep.Kind)
	}
}

// ---------------------------------------------------------------- Slack

// formatSlack emits a Slack Block Kit payload. The Slack incoming-
// webhook contract accepts either a flat text field or blocks; we
// always populate both so workspaces with text-only rendering still
// surface something useful.
func formatSlack(ev Event) ([]byte, error) {
	emoji := severityEmoji(ev.Severity)
	stateVerb := stateVerb(ev.State)
	headline := fmt.Sprintf("%s %s — %s", emoji, stateVerb, ev.Title)

	fields := []map[string]any{
		{"type": "mrkdwn", "text": fmt.Sprintf("*Rule*\n`%s`", ev.RuleID)},
		{"type": "mrkdwn", "text": fmt.Sprintf("*Severity*\n%s", ev.Severity)},
	}
	if ev.Scope != "" {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("*Scope*\n`%s`", ev.Scope)})
	}
	if ev.GroupKey != "" {
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("*Group*\n`%s`", ev.GroupKey)})
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": headline, "emoji": true},
		},
	}
	if ev.Body != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": ev.Body},
		})
	}
	blocks = append(blocks, map[string]any{"type": "section", "fields": fields})

	if ev.Runbook != "" {
		blocks = append(blocks, map[string]any{
			"type": "context",
			"elements": []any{
				map[string]any{"type": "mrkdwn", "text": fmt.Sprintf("Runbook: %s", ev.Runbook)},
			},
		})
	}

	payload := map[string]any{
		"text":   headline,
		"blocks": blocks,
	}
	return marshal(payload)
}

// ---------------------------------------------------------------- Teams

// formatTeams emits a legacy MessageCard. Teams workflow webhooks
// also accept this shape; the newer Adaptive Cards format is more
// expressive but workflow connectors fall back to MessageCard, so
// the legacy shape is the lowest-common-denominator that works
// across every Teams target FlowScope is likely to encounter.
func formatTeams(ev Event) ([]byte, error) {
	color := severityHex(ev.Severity)
	facts := []map[string]string{
		{"name": "Rule", "value": ev.RuleID},
		{"name": "Severity", "value": ev.Severity},
		{"name": "State", "value": ev.State},
	}
	if ev.Scope != "" {
		facts = append(facts, map[string]string{"name": "Scope", "value": ev.Scope})
	}
	if ev.GroupKey != "" {
		facts = append(facts, map[string]string{"name": "Group", "value": ev.GroupKey})
	}
	if !ev.Timestamp.IsZero() {
		facts = append(facts, map[string]string{"name": "Time", "value": ev.Timestamp.UTC().Format(time.RFC3339)})
	}

	section := map[string]any{
		"activityTitle":    ev.Title,
		"activitySubtitle": fmt.Sprintf("FlowScope alert · %s", ev.State),
		"facts":            facts,
		"markdown":         true,
	}
	if ev.Body != "" {
		section["text"] = ev.Body
	}

	payload := map[string]any{
		"@type":      "MessageCard",
		"@context":   "https://schema.org/extensions",
		"summary":    ev.Title,
		"themeColor": color,
		"title":      ev.Title,
		"sections":   []any{section},
	}

	if ev.Runbook != "" {
		payload["potentialAction"] = []any{
			map[string]any{
				"@type": "OpenUri",
				"name":  "Open runbook",
				"targets": []any{
					map[string]any{"os": "default", "uri": ev.Runbook},
				},
			},
		}
	}

	return marshal(payload)
}

// ---------------------------------------------------------------- PagerDuty

// formatPagerDuty emits an Events API v2 payload. The routing key
// (a.k.a. integration key) comes from the endpoint's stored secret —
// the operator pastes a PagerDuty integration key into the
// "secret" field on create. dedup_key derives from group_key so PD
// can correlate related events into one incident.
func formatPagerDuty(ep Endpoint, ev Event) ([]byte, error) {
	routingKey := strings.TrimSpace(ep.Secret)
	if routingKey == "" {
		return nil, fmt.Errorf("notifier: pagerduty endpoint missing routing key (set 'secret' field)")
	}

	action := "trigger"
	if ev.State == "closed" {
		action = "resolve"
	}

	dedupKey := ev.GroupKey
	if dedupKey == "" {
		// PagerDuty needs dedup_key to correlate trigger/resolve pairs.
		// Falling back to (rule_id, scope) keeps the correlation tight
		// even when the engine didn't emit a group_key.
		dedupKey = ev.RuleID
		if ev.Scope != "" {
			dedupKey = ev.RuleID + ":" + ev.Scope
		}
	}

	pdSeverity := pagerdutySeverity(ev.Severity)

	payload := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"dedup_key":    dedupKey,
	}
	if action == "trigger" {
		details := map[string]any{
			"rule_id":   ev.RuleID,
			"scope":     ev.Scope,
			"group_key": ev.GroupKey,
			"state":     ev.State,
		}
		if ev.Body != "" {
			details["body"] = ev.Body
		}
		if ev.Runbook != "" {
			details["runbook"] = ev.Runbook
		}
		for k, v := range ev.Labels {
			details["label_"+k] = v
		}
		payload["payload"] = map[string]any{
			"summary":   ev.Title,
			"severity":  pdSeverity,
			"source":    nonEmpty(ev.Scope, "flowscope"),
			"component": ev.RuleID,
			"timestamp": tsString(ev.Timestamp),
			"custom_details": details,
		}
	}
	return marshal(payload)
}

// ---------------------------------------------------------------- Generic HTTP

// formatHTTP emits the raw FlowScope event JSON. Operators wiring a
// custom listener get the canonical shape; the header_template_json
// from the endpoint provides any auth header construction.
func formatHTTP(ev Event) ([]byte, error) {
	return marshal(ev)
}

// ---------------------------------------------------------------- helpers

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Drop the trailing newline that json.Encoder appends so the
	// outbound HTTP body is exactly the JSON document and nothing more.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}

func severityEmoji(s string) string {
	switch strings.ToLower(s) {
	case "critical":
		return ":rotating_light:"
	case "warning":
		return ":warning:"
	case "info":
		return ":information_source:"
	}
	return ":bell:"
}

func severityHex(s string) string {
	switch strings.ToLower(s) {
	case "critical":
		return "D32F2F"
	case "warning":
		return "F9A825"
	case "info":
		return "1976D2"
	}
	return "424242"
}

func pagerdutySeverity(s string) string {
	switch strings.ToLower(s) {
	case "critical":
		return "critical"
	case "warning":
		return "warning"
	case "info":
		return "info"
	}
	return "warning"
}

func stateVerb(s string) string {
	switch strings.ToLower(s) {
	case "opened":
		return "OPEN"
	case "closed":
		return "RESOLVED"
	case "acknowledged":
		return "ACK"
	}
	return strings.ToUpper(s)
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func tsString(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339)
}
