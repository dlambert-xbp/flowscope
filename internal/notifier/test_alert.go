package notifier

import (
	"context"
	"fmt"
	"time"

	"github.com/dlambert-xbp/flowscope/internal/obs"
)

// SendTest fires a synthetic event through the same delivery pipeline
// as production traffic — formatter + retry + delivery ledger — so an
// operator can verify a new webhook before relying on it. Returns the
// final HTTP status and an error suitable for the API response.
//
// The synthetic event is marked with state 'opened' and severity
// 'info' so receivers that gate on severity still see it (only
// endpoints whose severity_filter omits 'info' will silently drop —
// the API handler surfaces that case to the caller).
func (d *Dispatcher) SendTest(ctx context.Context, ep Endpoint) (TestResult, error) {
	if !ep.SeverityAllowed("info") {
		return TestResult{
			Skipped: true,
			Reason:  "endpoint severity_filter excludes 'info' — broaden the filter to test",
		}, nil
	}

	now := d.nowFn()
	ev := Event{
		Timestamp: now,
		RuleID:    "test_webhook",
		Severity:  "info",
		State:     "opened",
		Scope:     "flowscope-test",
		GroupKey:  fmt.Sprintf("test-%d", now.UnixNano()),
		Title:     "FlowScope webhook test",
		Body:      "This is a synthetic test event from the FlowScope alert dispatcher. If you see this message, the webhook integration is working.",
		Runbook:   "",
		Labels:    map[string]string{"source": "test"},
	}
	ev.Signature = SignatureFor(ev.Timestamp, ev.RuleID, ev.Scope, ev.GroupKey, ev.State)

	body, contentType, err := FormatBody(ep, ev)
	if err != nil {
		obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "format").Inc()
		return TestResult{Error: err.Error()}, err
	}

	status, postErr := d.post(ctx, ep, body, contentType)
	dur := uint32(d.nowFn().Sub(now) / time.Millisecond)

	// Record the test attempt in webhook_deliveries with a dedicated
	// status so it's distinguishable from production deliveries.
	errStr := ""
	if postErr != nil {
		errStr = postErr.Error()
	}
	if d.conn != nil {
		d.recordDelivery(ctx, ep, ev, 1, statusTest, status, errStr, dur)
	}

	res := TestResult{
		HTTPStatus: status,
		DurationMS: int64(dur),
	}
	if postErr != nil {
		res.Error = postErr.Error()
		obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "test_network").Inc()
		return res, postErr
	}
	if status < 200 || status >= 300 {
		res.Error = fmt.Sprintf("http %d", status)
		obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "test_status").Inc()
		return res, fmt.Errorf("webhook test returned status %d", status)
	}
	obs.WebhookDispatchTotal.WithLabelValues(ep.Kind, "test").Inc()
	res.OK = true
	return res, nil
}

// TestResult is the API-facing response for POST /webhooks/{id}/test.
// All fields are populated even on failure so the UI can render a
// useful diagnostic ("Status 401: Unauthorized — check the token in
// the Authorization header").
type TestResult struct {
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Reason     string `json:"reason,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}
