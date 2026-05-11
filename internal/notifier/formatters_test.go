package notifier

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sampleEvent() Event {
	return Event{
		Signature: "abc123",
		Timestamp: time.Date(2026, 5, 11, 14, 30, 0, 0, time.UTC),
		RuleID:    "exporter_silent",
		Severity:  "critical",
		State:     "opened",
		Scope:     "exporter=10.0.0.1",
		GroupKey:  "10.0.0.1",
		Title:     "Exporter 10.0.0.1 stopped sending flows",
		Body:      "No flows for 90s; last seen at 14:28:30Z.",
		Runbook:   "https://runbooks.example.com/exporter-silent",
		Labels:    map[string]string{"exporter": "10.0.0.1", "vendor": "cisco"},
	}
}

func TestFormatBody_Slack(t *testing.T) {
	ep := Endpoint{Kind: "slack", URL: "https://hooks.slack.com/x"}
	body, ct, err := FormatBody(ep, sampleEvent())
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not valid json: %v\n%s", err, body)
	}
	// Must have a fallback text and a blocks array.
	if _, ok := parsed["text"]; !ok {
		t.Errorf("slack body missing 'text' fallback")
	}
	blocks, ok := parsed["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Errorf("slack body missing 'blocks' array")
	}
	if !strings.Contains(string(body), "Exporter 10.0.0.1") {
		t.Errorf("slack body missing event title:\n%s", body)
	}
}

func TestFormatBody_Teams(t *testing.T) {
	ep := Endpoint{Kind: "teams", URL: "https://outlook.office.com/webhook/x"}
	body, _, err := FormatBody(ep, sampleEvent())
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not valid json: %v\n%s", err, body)
	}
	if parsed["@type"] != "MessageCard" {
		t.Errorf("@type = %v, want MessageCard", parsed["@type"])
	}
	if parsed["themeColor"] == nil {
		t.Errorf("missing themeColor")
	}
	if parsed["potentialAction"] == nil {
		t.Errorf("missing potentialAction for runbook link")
	}
}

func TestFormatBody_PagerDuty_Trigger(t *testing.T) {
	ep := Endpoint{Kind: "pagerduty", URL: "https://events.pagerduty.com/v2/enqueue", Secret: "routing-key-123"}
	body, _, err := FormatBody(ep, sampleEvent())
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not valid json: %v\n%s", err, body)
	}
	if parsed["routing_key"] != "routing-key-123" {
		t.Errorf("routing_key = %v, want routing-key-123", parsed["routing_key"])
	}
	if parsed["event_action"] != "trigger" {
		t.Errorf("event_action = %v, want trigger", parsed["event_action"])
	}
	if parsed["dedup_key"] != "10.0.0.1" {
		t.Errorf("dedup_key = %v, want 10.0.0.1", parsed["dedup_key"])
	}
	payload, ok := parsed["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not a map: %T", parsed["payload"])
	}
	if payload["severity"] != "critical" {
		t.Errorf("payload.severity = %v, want critical", payload["severity"])
	}
}

func TestFormatBody_PagerDuty_Resolve(t *testing.T) {
	ev := sampleEvent()
	ev.State = "closed"
	ep := Endpoint{Kind: "pagerduty", Secret: "routing-key-123"}
	body, _, err := FormatBody(ep, ev)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	if parsed["event_action"] != "resolve" {
		t.Errorf("event_action = %v, want resolve", parsed["event_action"])
	}
	// resolve actions must NOT include a payload (PD rejects with one).
	if _, present := parsed["payload"]; present {
		t.Errorf("resolve action should omit payload field")
	}
}

func TestFormatBody_PagerDuty_MissingRoutingKey(t *testing.T) {
	ep := Endpoint{Kind: "pagerduty"}
	_, _, err := FormatBody(ep, sampleEvent())
	if err == nil {
		t.Fatalf("expected error when routing key (secret) is empty")
	}
}

func TestFormatBody_HTTP(t *testing.T) {
	ep := Endpoint{Kind: "http"}
	body, _, err := FormatBody(ep, sampleEvent())
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("http body should round-trip as Event: %v\n%s", err, body)
	}
	if ev.RuleID != "exporter_silent" {
		t.Errorf("rule_id round-trip = %q, want exporter_silent", ev.RuleID)
	}
}

func TestFormatBody_UnknownKind(t *testing.T) {
	ep := Endpoint{Kind: "discord"}
	_, _, err := FormatBody(ep, sampleEvent())
	if err == nil {
		t.Fatalf("expected error for unknown kind")
	}
}

func TestSeverityAllowed(t *testing.T) {
	cases := []struct {
		name   string
		filter []string
		sev    string
		want   bool
	}{
		{"empty filter accepts critical", nil, "critical", true},
		{"empty filter accepts info", nil, "info", true},
		{"explicit match", []string{"critical", "warning"}, "warning", true},
		{"explicit miss", []string{"critical"}, "info", false},
		{"case-insensitive", []string{"Critical"}, "critical", true},
		{"whitespace tolerated", []string{" warning "}, "warning", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := Endpoint{SeverityFilter: tc.filter}
			if got := ep.SeverityAllowed(tc.sev); got != tc.want {
				t.Errorf("SeverityAllowed(%q) = %v, want %v", tc.sev, got, tc.want)
			}
		})
	}
}

func TestSignatureFor_Stable(t *testing.T) {
	ts := time.Date(2026, 5, 11, 14, 30, 0, 0, time.UTC)
	s1 := SignatureFor(ts, "rule", "scope", "gk", "opened")
	s2 := SignatureFor(ts, "rule", "scope", "gk", "opened")
	if s1 != s2 {
		t.Errorf("signature not stable across calls: %s != %s", s1, s2)
	}
	if len(s1) != 64 {
		t.Errorf("signature length = %d, want 64 (sha256 hex)", len(s1))
	}
	s3 := SignatureFor(ts, "rule", "scope", "gk", "closed")
	if s1 == s3 {
		t.Errorf("signature should differ for different state")
	}
}
