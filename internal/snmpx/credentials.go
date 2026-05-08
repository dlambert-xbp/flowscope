package snmpx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Credential is the per-exporter SNMP binding the operator configures
// in the Settings tab. v2c uses Community; v3 uses Username + Auth/Priv.
//
// Plaintext form: what the api accepts on PUT and emits on GET (with
// secrets redacted). Storage form: what lives in ClickHouse, with
// every passphrase / community AES-GCM-encrypted under the master.
type Credential struct {
	Exporter      string        `json:"exporter"`
	Version       string        `json:"version"` // 'v2c' | 'v3'
	Port          uint16        `json:"port"`
	Interval      time.Duration `json:"-"`
	IntervalSec   uint32        `json:"interval_sec"`

	// v2c
	Community string `json:"community,omitempty"` // omitted on GET; required on PUT for v2c

	// v3
	V3Username     string `json:"v3_username,omitempty"`
	V3AuthProto    string `json:"v3_auth_proto,omitempty"` // '' | MD5 | SHA | SHA-224 | SHA-256 | SHA-384 | SHA-512
	V3AuthPass     string `json:"v3_auth_pass,omitempty"`
	V3PrivProto    string `json:"v3_priv_proto,omitempty"` // '' | DES | AES | AES-192 | AES-256
	V3PrivPass     string `json:"v3_priv_pass,omitempty"`
	V3Context      string `json:"v3_context,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`

	// HasCommunity / HasAuthPass / HasPrivPass let the api expose
	// "is a secret configured" state on GET without leaking the
	// secret itself — the UI uses these to render checkmarks vs
	// empty fields.
	HasCommunity bool `json:"has_community"`
	HasAuthPass  bool `json:"has_auth_pass"`
	HasPrivPass  bool `json:"has_priv_pass"`
}

// CredentialStore persists Credentials with their secrets encrypted
// at rest. Implementations are safe for concurrent calls — they are
// hit by the snmp scheduler and the api in parallel.
type CredentialStore interface {
	Get(ctx context.Context, exporter string) (*Credential, error)
	List(ctx context.Context) ([]Credential, error)
	Set(ctx context.Context, c Credential, actor string) error
	Delete(ctx context.Context, exporter string) error

	// RequestWalk enqueues an immediate-walk request for exporter.
	// The snmp scheduler picks it up on its next dispatch tick and
	// walks regardless of cadence. Idempotent — duplicate requests
	// are harmless.
	RequestWalk(ctx context.Context, exporter string, actor string) error

	// WalkRequests returns max(requested_at) per exporter from the
	// outstanding-request queue. Cheap — a small aggregate query.
	// The scheduler calls this every dispatch pass.
	WalkRequests(ctx context.Context) (map[string]time.Time, error)
}

// NewClickHouseCredentialStore returns a store backed by the
// snmp_credentials table, using crypter to seal/unseal secrets.
func NewClickHouseCredentialStore(conn driver.Conn, crypter *Crypter) CredentialStore {
	return &chCredStore{conn: conn, crypter: crypter}
}

type chCredStore struct {
	conn    driver.Conn
	crypter *Crypter
}

const colsRedacted = `
exporter, version, port, interval_sec,
v3_username, v3_auth_proto, v3_priv_proto, v3_context,
length(community_ct)    > 0 AS has_community,
length(v3_auth_pass_ct) > 0 AS has_auth_pass,
length(v3_priv_pass_ct) > 0 AS has_priv_pass,
updated_at, updated_by`

// Get returns the binding for exporter with secrets DECRYPTED. Used
// by the scheduler at walk time and by the api's /test endpoint.
// Returns (nil, ErrNotFound) when no binding is configured.
func (s *chCredStore) Get(ctx context.Context, exporter string) (*Credential, error) {
	addr, err := netip.ParseAddr(exporter)
	if err != nil {
		return nil, fmt.Errorf("snmpx.creds: parse %q: %w", exporter, err)
	}
	expIP := toIPv6(addr)
	const q = `
SELECT
    version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    updated_at, updated_by
FROM snmp_credentials FINAL
WHERE exporter = ?`
	row := s.conn.QueryRow(ctx, q, expIP)
	var (
		c                                                                              Credential
		communityCT, authCT, privCT                                                    string
		intervalSec                                                                    uint32
	)
	if err := row.Scan(
		&c.Version, &c.Port, &intervalSec,
		&communityCT, &c.V3Username, &c.V3AuthProto, &authCT,
		&c.V3PrivProto, &privCT, &c.V3Context,
		&c.UpdatedAt, &c.UpdatedBy,
	); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrCredNotFound
		}
		return nil, fmt.Errorf("snmpx.creds: scan: %w", err)
	}
	c.Exporter = addr.Unmap().String()
	c.IntervalSec = intervalSec
	c.Interval = time.Duration(intervalSec) * time.Second
	c.HasCommunity = communityCT != ""
	c.HasAuthPass = authCT != ""
	c.HasPrivPass = privCT != ""

	if c.Community, err = s.crypter.Decrypt(communityCT); err != nil {
		return nil, fmt.Errorf("snmpx.creds: decrypt community: %w", err)
	}
	if c.V3AuthPass, err = s.crypter.Decrypt(authCT); err != nil {
		return nil, fmt.Errorf("snmpx.creds: decrypt auth: %w", err)
	}
	if c.V3PrivPass, err = s.crypter.Decrypt(privCT); err != nil {
		return nil, fmt.Errorf("snmpx.creds: decrypt priv: %w", err)
	}
	return &c, nil
}

// List returns every binding with secrets REDACTED — passphrases and
// communities are blank, only the *Has* booleans report whether they
// are set.
func (s *chCredStore) List(ctx context.Context) ([]Credential, error) {
	q := `SELECT ` + colsRedacted + ` FROM snmp_credentials FINAL ORDER BY exporter`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmpx.creds: list: %w", err)
	}
	defer rows.Close()
	out := make([]Credential, 0, 16)
	for rows.Next() {
		var (
			c    Credential
			addr netip.Addr
		)
		if err := rows.Scan(
			&addr, &c.Version, &c.Port, &c.IntervalSec,
			&c.V3Username, &c.V3AuthProto, &c.V3PrivProto, &c.V3Context,
			&c.HasCommunity, &c.HasAuthPass, &c.HasPrivPass,
			&c.UpdatedAt, &c.UpdatedBy,
		); err != nil {
			return nil, fmt.Errorf("snmpx.creds: scan list: %w", err)
		}
		c.Exporter = addr.Unmap().String()
		c.Interval = time.Duration(c.IntervalSec) * time.Second
		out = append(out, c)
	}
	return out, rows.Err()
}

// Set inserts or replaces the binding for c.Exporter. The
// ReplacingMergeTree engine collapses old rows on background merge;
// SELECT FINAL in Get/List sees only the latest row.
func (s *chCredStore) Set(ctx context.Context, c Credential, actor string) error {
	addr, err := netip.ParseAddr(c.Exporter)
	if err != nil {
		return fmt.Errorf("snmpx.creds: parse exporter: %w", err)
	}
	expIP := toIPv6(addr)

	switch c.Version {
	case "v2c":
		if c.Community == "" && !c.HasCommunity {
			return fmt.Errorf("snmpx.creds: v2c binding requires community")
		}
	case "v3":
		if c.V3Username == "" {
			return fmt.Errorf("snmpx.creds: v3 binding requires username")
		}
	default:
		return fmt.Errorf("snmpx.creds: invalid version %q (want v2c | v3)", c.Version)
	}
	if c.Port == 0 {
		c.Port = 161
	}
	if c.IntervalSec == 0 {
		c.IntervalSec = 900
	}

	communityCT, err := s.crypter.Encrypt(c.Community)
	if err != nil {
		return fmt.Errorf("snmpx.creds: encrypt community: %w", err)
	}
	authCT, err := s.crypter.Encrypt(c.V3AuthPass)
	if err != nil {
		return fmt.Errorf("snmpx.creds: encrypt auth: %w", err)
	}
	privCT, err := s.crypter.Encrypt(c.V3PrivPass)
	if err != nil {
		return fmt.Errorf("snmpx.creds: encrypt priv: %w", err)
	}
	const ins = `
INSERT INTO snmp_credentials
   (exporter, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return s.conn.Exec(ctx, ins,
		expIP, c.Version, c.Port, c.IntervalSec,
		communityCT, c.V3Username, c.V3AuthProto, authCT,
		c.V3PrivProto, privCT, c.V3Context,
		time.Now().UTC(), actorOr(actor),
	)
}

// Delete removes the binding for exporter via a TRUNCATE-style
// delete. ReplacingMergeTree doesn't have a native "remove" so we
// use ALTER TABLE … DELETE WHERE.
func (s *chCredStore) Delete(ctx context.Context, exporter string) error {
	addr, err := netip.ParseAddr(exporter)
	if err != nil {
		return fmt.Errorf("snmpx.creds: parse %q: %w", exporter, err)
	}
	expIP := toIPv6(addr)
	const q = `ALTER TABLE snmp_credentials DELETE WHERE exporter = ?`
	return s.conn.Exec(ctx, q, expIP)
}

// RequestWalk enqueues an immediate-walk request. Append-only — the
// scheduler decides what to do via max(requested_at) > lastWalked.
func (s *chCredStore) RequestWalk(ctx context.Context, exporter string, actor string) error {
	addr, err := netip.ParseAddr(exporter)
	if err != nil {
		return fmt.Errorf("snmpx.creds: parse %q: %w", exporter, err)
	}
	const ins = `INSERT INTO snmp_walk_requests (exporter, requested_at, requested_by) VALUES (?, ?, ?)`
	return s.conn.Exec(ctx, ins, toIPv6(addr), time.Now().UTC(), actorOr(actor))
}

// WalkRequests returns the latest request time per exporter. Bounded
// by the table's 1-day TTL so the result set stays small.
func (s *chCredStore) WalkRequests(ctx context.Context) (map[string]time.Time, error) {
	const q = `SELECT IPv6NumToString(exporter), max(requested_at) FROM snmp_walk_requests GROUP BY exporter`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmpx.creds: walk requests: %w", err)
	}
	defer rows.Close()
	out := make(map[string]time.Time, 16)
	for rows.Next() {
		var (
			raw string
			ts  time.Time
		)
		if err := rows.Scan(&raw, &ts); err != nil {
			return nil, fmt.Errorf("snmpx.creds: scan walk request: %w", err)
		}
		out[unmap4in6(raw)] = ts
	}
	return out, rows.Err()
}

// toIPv6 returns the 16-byte big-endian net.IP form for the IPv6
// ClickHouse column. The clickhouse-go v2 driver special-cases
// net.IP for IPv6; passing a raw []byte serializes as
// Array(UInt64) and ClickHouse rejects with "Illegal type". Mirrors
// the helper in internal/store/batcher.go.
func toIPv6(addr netip.Addr) net.IP {
	a := addr.As16()
	return a[:]
}

// ErrCredNotFound is returned by Get when no binding exists.
var ErrCredNotFound = fmt.Errorf("snmpx: credential not found")

func actorOr(s string) string {
	if s == "" {
		return "anonymous"
	}
	return s
}
