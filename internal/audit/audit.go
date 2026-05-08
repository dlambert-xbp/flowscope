// Package audit records mutations to settings tables in an
// append-only ClickHouse ledger (audit_events). Every write handler
// in cmd/api should call Writer.Record before responding so the
// "who changed what when" question has a single, authoritative answer.
//
// The ledger is intentionally separate from the resource tables it
// describes — the tables themselves use ReplacingMergeTree and
// background merges collapse history, so a separate write-only
// ledger is the only durable place to look up "what did this row
// look like before yesterday?".
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Action is the high-level mutation kind. The set is closed; the api
// asserts these constants when constructing events.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
)

// Resource names. Kept centralised so a typo in one handler can't
// silently fragment the audit log.
const (
	ResourceCustomService     = "custom_service"
	ResourceAPIToken          = "api_token"
	ResourceExporterAllowlist = "exporter_allowlist"
	ResourceAppSetting        = "app_setting"
	ResourceAlertRuleSetting  = "alert_rule_setting"
	ResourceWebhook           = "webhook"
	ResourceOIDCConfig        = "oidc_config"
	ResourceSNMPCredential    = "snmp_credential"
)

// Event is one ledger row. Before / After are arbitrary structs the
// caller wants captured; they're JSON-encoded by Record so handlers
// don't need to think about serialisation. SourceIP and RequestID
// come off the request the handler is processing.
type Event struct {
	Actor     string
	Action    Action
	Resource  string
	Target    string
	Before    any
	After     any
	RequestID string
	SourceIP  string
}

// Writer persists Events. Implementations are safe for concurrent
// calls — handlers may share one Writer.
type Writer interface {
	Record(ctx context.Context, e Event) error
}

// NewClickHouseWriter returns a Writer that inserts into the
// audit_events table on conn. If conn is nil the returned Writer is
// a no-op — useful in tests and when the api boots in a degraded
// state without a database.
func NewClickHouseWriter(conn driver.Conn) Writer {
	if conn == nil {
		return noopWriter{}
	}
	return &chWriter{conn: conn}
}

type chWriter struct {
	conn driver.Conn
}

func (w *chWriter) Record(ctx context.Context, e Event) error {
	beforeJSON := encodeJSON(e.Before)
	afterJSON := encodeJSON(e.After)

	srcIP := e.SourceIP
	if srcIP == "" {
		srcIP = "::"
	}
	addr, err := netip.ParseAddr(srcIP)
	if err != nil {
		// Fall back to all-zeros so a malformed RemoteAddr never blocks
		// the audit write — losing the IP is far better than losing
		// the audit row entirely.
		addr = netip.IPv6Unspecified()
	}
	ipBytes := addr.As16()

	const ins = `
INSERT INTO audit_events
   (ts, actor, action, resource, target, before_json, after_json, request_id, source_ip)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return w.conn.Exec(ctx, ins,
		time.Now().UTC(),
		e.Actor,
		string(e.Action),
		e.Resource,
		e.Target,
		beforeJSON,
		afterJSON,
		e.RequestID,
		ipBytes[:],
	)
}

type noopWriter struct{}

func (noopWriter) Record(context.Context, Event) error { return nil }

// LedgerEntry is the read shape returned by the /api/settings/audit
// endpoint. Mirrors the table columns one-for-one.
type LedgerEntry struct {
	Timestamp time.Time `json:"ts"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Resource  string    `json:"resource"`
	Target    string    `json:"target"`
	Before    string    `json:"before_json,omitempty"`
	After     string    `json:"after_json,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	SourceIP  string    `json:"source_ip,omitempty"`
}

// Reader is implemented by the same writer; we keep the interface
// separate so handlers depend only on what they need.
type Reader interface {
	List(ctx context.Context, q ListQuery) ([]LedgerEntry, error)
}

// ListQuery filters and paginates the ledger.
type ListQuery struct {
	Resource string    // optional
	Actor    string    // optional
	Action   string    // optional
	Since    time.Time // optional
	Until    time.Time // optional
	Limit    int       // <= 0 means default 100; capped at 1000
	Offset   int
}

// NewClickHouseReader returns a Reader over the same audit_events
// table. nil-safe: returns an empty-result reader if conn is nil.
func NewClickHouseReader(conn driver.Conn) Reader {
	if conn == nil {
		return noopReader{}
	}
	return &chReader{conn: conn}
}

type chReader struct {
	conn driver.Conn
}

func (r *chReader) List(ctx context.Context, q ListQuery) ([]LedgerEntry, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	where := "1"
	args := []any{}
	if q.Resource != "" {
		where += " AND resource = ?"
		args = append(args, q.Resource)
	}
	if q.Actor != "" {
		where += " AND actor = ?"
		args = append(args, q.Actor)
	}
	if q.Action != "" {
		where += " AND action = ?"
		args = append(args, q.Action)
	}
	if !q.Since.IsZero() {
		where += " AND ts >= ?"
		args = append(args, q.Since.UTC())
	}
	if !q.Until.IsZero() {
		where += " AND ts < ?"
		args = append(args, q.Until.UTC())
	}

	query := fmt.Sprintf(`
SELECT ts, actor, action, resource, target, before_json, after_json, request_id, IPv6NumToString(source_ip)
FROM audit_events
WHERE %s
ORDER BY ts DESC
LIMIT %d OFFSET %d`, where, limit, q.Offset)

	rows, err := r.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: query: %w", err)
	}
	defer rows.Close()

	out := make([]LedgerEntry, 0, limit)
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(
			&e.Timestamp, &e.Actor, &e.Action, &e.Resource, &e.Target,
			&e.Before, &e.After, &e.RequestID, &e.SourceIP,
		); err != nil {
			return nil, fmt.Errorf("audit: scan: %w", err)
		}
		// Strip ::ffff: from IPv4-mapped addresses for display.
		const pfx = "::ffff:"
		if len(e.SourceIP) > len(pfx) && e.SourceIP[:len(pfx)] == pfx {
			e.SourceIP = e.SourceIP[len(pfx):]
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type noopReader struct{}

func (noopReader) List(context.Context, ListQuery) ([]LedgerEntry, error) {
	return nil, nil
}

func encodeJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Audit writes must never fail because of an unrelated marshal
		// error — record the failure instead and move on.
		return fmt.Sprintf("{\"_audit_marshal_error\":%q}", err.Error())
	}
	return string(b)
}
