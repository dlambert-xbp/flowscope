package notifier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/dlambert-xbp/flowscope/internal/audit"
	"github.com/dlambert-xbp/flowscope/internal/obs"
	"github.com/dlambert-xbp/flowscope/internal/snmpx"
)

// Constants for delivery status values written to webhook_deliveries.
const (
	statusQueued = "queued"
	statusSent   = "sent"
	statusFailed = "failed"
	statusTest   = "test"
)

// Defaults. Override via Dispatcher fields before calling Run.
const (
	defaultTick          = 5 * time.Second
	defaultBatchLimit    = 200
	defaultRescueWindow  = 5 * time.Minute
	defaultMaxAttempts   = 3
	defaultPostTimeout   = 8 * time.Second
	defaultInitialBackoff = 1 * time.Second
)

// HTTPClient is the subset of *http.Client the dispatcher uses. The
// indirection lets tests inject a fake without depending on the
// httptest package from production code.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Dispatcher is the long-lived webhook fan-out loop. One per
// cmd/alert replica; constructed once and started via Run.
type Dispatcher struct {
	conn     driver.Conn
	crypter  *snmpx.Crypter
	audit    audit.Writer
	http     HTTPClient
	tick     time.Duration
	limit    int
	rescue   time.Duration
	maxAttempts int
	postTimeout time.Duration
	initialBackoff time.Duration

	// nowFn lets tests inject a fixed clock. Production keeps it as
	// time.Now and the field is package-private so callers can't
	// override accidentally.
	nowFn func() time.Time
}

// New returns a Dispatcher ready to Run. crypter is required (matches
// the Engine — without it secret_ct can't be opened so we'd post
// nothing useful). audit may be a no-op writer in degraded mode.
func New(conn driver.Conn, crypter *snmpx.Crypter, auditW audit.Writer) *Dispatcher {
	return &Dispatcher{
		conn:        conn,
		crypter:     crypter,
		audit:       auditW,
		http:        &http.Client{Timeout: defaultPostTimeout},
		tick:        defaultTick,
		limit:       defaultBatchLimit,
		rescue:      defaultRescueWindow,
		maxAttempts: defaultMaxAttempts,
		postTimeout: defaultPostTimeout,
		initialBackoff: defaultInitialBackoff,
		nowFn:       func() time.Time { return time.Now().UTC() },
	}
}

// WithHTTPClient overrides the http client. Tests inject httptest
// servers' clients; production keeps the default.
func (d *Dispatcher) WithHTTPClient(c HTTPClient) *Dispatcher {
	d.http = c
	return d
}

// WithTick overrides the poll cadence. Production stays at 5s; tests
// drive a faster tick.
func (d *Dispatcher) WithTick(t time.Duration) *Dispatcher {
	if t > 0 {
		d.tick = t
	}
	return d
}

// WithMaxAttempts overrides the retry cap.
func (d *Dispatcher) WithMaxAttempts(n int) *Dispatcher {
	if n > 0 {
		d.maxAttempts = n
	}
	return d
}

// WithInitialBackoff overrides the first retry delay. Backoffs double
// from this value (1s → 2s → 4s by default).
func (d *Dispatcher) WithInitialBackoff(t time.Duration) *Dispatcher {
	if t >= 0 {
		d.initialBackoff = t
	}
	return d
}

// Run blocks until ctx is cancelled, polling alert_events on the
// configured tick. Returns nil on clean shutdown.
func (d *Dispatcher) Run(ctx context.Context) error {
	slog.Info("notifier: dispatcher starting",
		"tick", d.tick.String(),
		"max_attempts", d.maxAttempts,
		"rescue_window", d.rescue.String(),
	)
	t := time.NewTicker(d.tick)
	defer t.Stop()

	// Fire once immediately so a freshly-restarted dispatcher catches
	// up without waiting a full tick.
	d.dispatchOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			d.dispatchOnce(ctx)
		}
	}
}

// dispatchOnce reads the cursor, pulls the next batch of opened/closed
// events, fans out to each enabled webhook endpoint, and advances the
// cursor on success. Idempotency lives in webhook_deliveries — a
// crash mid-batch means the next dispatchOnce sees the same events
// but skips (signature, endpoint) tuples already marked 'sent'.
func (d *Dispatcher) dispatchOnce(ctx context.Context) {
	cur, err := d.loadCursor(ctx)
	if err != nil {
		slog.Warn("notifier: load cursor failed", "err", err)
		return
	}

	events, err := d.fetchEvents(ctx, cur.LastTS, d.limit)
	if err != nil {
		slog.Warn("notifier: fetch events failed", "err", err)
		return
	}
	if len(events) == 0 {
		return
	}

	endpoints, err := d.fetchEndpoints(ctx)
	if err != nil {
		slog.Warn("notifier: fetch endpoints failed", "err", err)
		return
	}

	// Without endpoints there's nothing to do — but still advance the
	// cursor so we don't re-scan the same events forever.
	if len(endpoints) == 0 {
		d.advanceCursor(ctx, events)
		return
	}

	// Idempotency: pre-load sent (signature, endpoint) tuples for the
	// candidate event signatures so a crash-resume doesn't double-fire.
	sigs := make([]string, 0, len(events))
	for _, ev := range events {
		sigs = append(sigs, ev.Signature)
	}
	alreadySent, err := d.alreadySent(ctx, sigs)
	if err != nil {
		slog.Warn("notifier: load delivery ledger failed", "err", err)
		// Fall through — duplicate sends are preferable to silence.
		alreadySent = map[string]bool{}
	}

	var (
		wg      sync.WaitGroup
		dispatched int
	)
	for _, ev := range events {
		for _, ep := range endpoints {
			if !ep.SeverityAllowed(ev.Severity) {
				continue
			}
			if alreadySent[ev.Signature+"|"+ep.ID] {
				continue
			}
			dispatched++
			wg.Add(1)
			go func(ep Endpoint, ev Event) {
				defer wg.Done()
				d.deliverWithRetry(ctx, ep, ev)
			}(ep, ev)
		}
	}
	wg.Wait()

	if dispatched > 0 {
		slog.Info("notifier: tick complete", "events", len(events), "dispatched", dispatched)
	}
	d.advanceCursor(ctx, events)
}

// deliverWithRetry sends one event to one endpoint with exponential
// backoff. Each attempt writes a webhook_deliveries row. On permanent
// failure (maxAttempts exhausted) an audit_events row is also written.
func (d *Dispatcher) deliverWithRetry(ctx context.Context, ep Endpoint, ev Event) {
	body, contentType, err := FormatBody(ep, ev)
	if err != nil {
		// Formatter / config errors don't get retried — they will fail
		// the same way next tick.
		d.recordDelivery(ctx, ep, ev, 1, statusFailed, 0, err.Error(), 0)
		d.recordAuditFailure(ctx, ep, ev, err)
		obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "format").Inc()
		return
	}

	backoff := d.initialBackoff
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		start := d.nowFn()
		status, postErr := d.post(ctx, ep, body, contentType)
		dur := uint32(d.nowFn().Sub(start) / time.Millisecond)

		if postErr == nil && status >= 200 && status < 300 {
			d.recordDelivery(ctx, ep, ev, uint8(attempt), statusSent, status, "", dur)
			obs.WebhookDispatchTotal.WithLabelValues(ep.Kind, "ok").Inc()
			return
		}

		errStr := ""
		if postErr != nil {
			errStr = postErr.Error()
		} else {
			errStr = fmt.Sprintf("http %d", status)
		}
		d.recordDelivery(ctx, ep, ev, uint8(attempt), statusFailed, status, errStr, dur)
		obs.WebhookDispatchRetriesTotal.WithLabelValues(ep.Kind).Inc()

		// 4xx (except 408 / 429) is a client error — don't retry, they
		// will just fail the same way next attempt. 5xx and network
		// errors retry up to the cap.
		if postErr == nil && status >= 400 && status < 500 && status != 408 && status != 429 {
			obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "client_error").Inc()
			d.recordAuditFailure(ctx, ep, ev, fmt.Errorf("permanent %d: %s", status, errStr))
			return
		}

		if attempt == d.maxAttempts {
			obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "exhausted").Inc()
			d.recordAuditFailure(ctx, ep, ev, fmt.Errorf("retries exhausted: %s", errStr))
			return
		}

		// Sleep, but bail early on ctx cancel.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// post executes one HTTP attempt and returns (status, error). A nil
// error with a 2xx status is success; any other combination is a
// candidate for retry.
func (d *Dispatcher) post(ctx context.Context, ep Endpoint, body []byte, contentType string) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, d.postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "FlowScope-Notifier/1.0")
	for k, v := range ep.Headers {
		// Skip our own headers — operators shouldn't be overriding
		// Content-Type by mistake.
		if strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "User-Agent") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// SignatureFor returns the dispatcher-stable signature for an alert
// event. Exposed for the /webhooks/{id}/test handler so the test
// alert can be marked with a distinct, traceable hash.
func SignatureFor(ts time.Time, ruleID, scope, groupKey, state string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s", ts.UTC().UnixNano(), ruleID, scope, groupKey, state)
	return hex.EncodeToString(h.Sum(nil))
}

// --------------------------------------------------------------- I/O

type cursor struct {
	LastTS    time.Time
	LastEvent string
	UpdatedAt time.Time
}

func (d *Dispatcher) loadCursor(ctx context.Context) (cursor, error) {
	const q = `
SELECT last_ts, last_event, updated_at
FROM webhook_dispatcher_state FINAL
WHERE id = 'singleton'`
	var c cursor
	err := d.conn.QueryRow(ctx, q).Scan(&c.LastTS, &c.LastEvent, &c.UpdatedAt)
	if err != nil {
		// No row yet — fall back to "5 minutes ago" so a freshly-
		// installed cluster doesn't replay the entire alert ledger on
		// first start.
		if errors.Is(err, errNoRows) || err.Error() == "sql: no rows in result set" {
			return cursor{LastTS: d.nowFn().Add(-d.rescue)}, nil
		}
		return cursor{}, err
	}
	return c, nil
}

// errNoRows is a sentinel matched by string compare against the
// ClickHouse driver's no-rows error.
var errNoRows = errors.New("sql: no rows in result set")

func (d *Dispatcher) saveCursor(ctx context.Context, c cursor) error {
	const ins = `
INSERT INTO webhook_dispatcher_state (id, last_ts, last_event, updated_at)
VALUES ('singleton', ?, ?, ?)`
	return d.conn.Exec(ctx, ins, c.LastTS, c.LastEvent, d.nowFn())
}

// advanceCursor moves the high-water mark forward by the rescue window
// behind the most-recent event. Keeping the cursor slightly behind
// gives slow alert_events rows time to land before we move past them
// — at-least-once delivery is fine because webhook_deliveries dedupes.
func (d *Dispatcher) advanceCursor(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	var maxTS time.Time
	var maxSig string
	for _, ev := range events {
		if ev.Timestamp.After(maxTS) {
			maxTS = ev.Timestamp
			maxSig = ev.Signature
		}
	}
	// Move to maxTS - 1ms so any concurrent insert at exactly maxTS
	// is still picked up next tick. webhook_deliveries dedupes.
	newCursor := cursor{
		LastTS:    maxTS.Add(-time.Millisecond),
		LastEvent: maxSig,
	}
	if err := d.saveCursor(ctx, newCursor); err != nil {
		slog.Warn("notifier: save cursor failed", "err", err)
	}
}

// fetchEvents returns opened/closed alert_events strictly newer than
// since. The cursor includes a 1ms rewind on advance so this `>=`
// behaviour catches concurrent inserts at the boundary, with
// webhook_deliveries providing the idempotency guard.
func (d *Dispatcher) fetchEvents(ctx context.Context, since time.Time, limit int) ([]Event, error) {
	const q = `
SELECT ts, rule_id, severity, state, scope, group_key, title, body, runbook, labels
FROM alert_events
WHERE ts > ? AND state IN ('opened', 'closed')
ORDER BY ts ASC
LIMIT ?`
	rows, err := d.conn.Query(ctx, q, since, uint64(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0, 16)
	for rows.Next() {
		var ev Event
		if err := rows.Scan(
			&ev.Timestamp, &ev.RuleID, &ev.Severity, &ev.State,
			&ev.Scope, &ev.GroupKey, &ev.Title, &ev.Body, &ev.Runbook, &ev.Labels,
		); err != nil {
			return nil, err
		}
		ev.Signature = SignatureFor(ev.Timestamp, ev.RuleID, ev.Scope, ev.GroupKey, ev.State)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// fetchEndpoints returns the enabled webhook_endpoints rows with
// secret_ct decrypted in-place. Endpoints whose secret won't decrypt
// are logged + skipped (audit + counter) so one bad row can't block
// the rest of the fan-out.
func (d *Dispatcher) fetchEndpoints(ctx context.Context) ([]Endpoint, error) {
	const q = `
SELECT id, name, kind, url, secret_ct, header_template_json, severity_filter
FROM webhook_endpoints FINAL
WHERE enabled = 1`
	rows, err := d.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Endpoint, 0, 4)
	for rows.Next() {
		var (
			id            uuid.UUID
			ep            Endpoint
			secretCT      string
			headerJSON    string
			severityField string
		)
		if err := rows.Scan(&id, &ep.Name, &ep.Kind, &ep.URL, &secretCT, &headerJSON, &severityField); err != nil {
			return nil, err
		}
		ep.ID = id.String()
		if secretCT != "" {
			if d.crypter == nil {
				obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "no_crypter").Inc()
				slog.Warn("notifier: endpoint has secret but FLOWSCOPE_SNMP_KEY not set, skipping", "id", ep.ID, "name", ep.Name)
				continue
			}
			pt, err := d.crypter.Decrypt(secretCT)
			if err != nil {
				obs.WebhookDispatchFailuresTotal.WithLabelValues(ep.Kind, "decrypt").Inc()
				slog.Warn("notifier: endpoint secret decrypt failed, skipping", "id", ep.ID, "name", ep.Name, "err", err)
				continue
			}
			ep.Secret = pt
		}
		if headerJSON != "" && headerJSON != "{}" {
			_ = json.Unmarshal([]byte(headerJSON), &ep.Headers)
		}
		if severityField != "" {
			ep.SeverityFilter = strings.Split(severityField, ",")
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

func (d *Dispatcher) alreadySent(ctx context.Context, signatures []string) (map[string]bool, error) {
	if len(signatures) == 0 {
		return map[string]bool{}, nil
	}
	const q = `
SELECT signature, endpoint_id
FROM webhook_deliveries
WHERE signature IN (?) AND status = 'sent'`
	rows, err := d.conn.Query(ctx, q, signatures)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool, len(signatures))
	for rows.Next() {
		var sig string
		var endpointID uuid.UUID
		if err := rows.Scan(&sig, &endpointID); err != nil {
			return nil, err
		}
		out[sig+"|"+endpointID.String()] = true
	}
	return out, rows.Err()
}

func (d *Dispatcher) recordDelivery(
	ctx context.Context,
	ep Endpoint,
	ev Event,
	attempt uint8,
	status string,
	httpStatus int,
	errStr string,
	durMS uint32,
) {
	if d.conn == nil {
		return
	}
	endpointUUID, _ := uuid.Parse(ep.ID)
	const ins = `
INSERT INTO webhook_deliveries
   (ts, signature, endpoint_id, attempt, status, http_status, error, duration_ms, event_ts, rule_id, severity)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := d.conn.Exec(ctx, ins,
		d.nowFn(), ev.Signature, endpointUUID, attempt, status,
		uint16(httpStatus), truncate(errStr, 4096), durMS,
		ev.Timestamp, ev.RuleID, ev.Severity,
	); err != nil {
		slog.Warn("notifier: record delivery failed", "err", err, "endpoint", ep.Name)
	}
}

// recordAuditFailure writes an audit_events row when a webhook
// dispatch fails permanently — either a 4xx response, a formatter
// error, or maxAttempts exhausted. Surfacing this in the audit ledger
// is the "no silent failures" guarantee from CLAUDE.md.
func (d *Dispatcher) recordAuditFailure(ctx context.Context, ep Endpoint, ev Event, cause error) {
	if d.audit == nil {
		return
	}
	_ = d.audit.Record(ctx, audit.Event{
		Actor:    "notifier",
		Action:   audit.ActionUpdate,
		Resource: audit.ResourceWebhook,
		Target:   ep.ID,
		After: map[string]any{
			"webhook_name": ep.Name,
			"webhook_kind": ep.Kind,
			"event_rule":   ev.RuleID,
			"event_scope":  ev.Scope,
			"event_state":  ev.State,
			"event_severity": ev.Severity,
			"signature":    ev.Signature,
			"error":        cause.Error(),
		},
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
