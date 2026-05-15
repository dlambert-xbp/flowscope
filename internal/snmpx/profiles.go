package snmpx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Profile is one entry in the named credential library. Each profile
// is a complete v2c or v3 credential set referenced by one or more
// Credential bindings via Credential.ProfileID. Profiles flagged
// use_for_discovery participate in auto-binding for flow-discovered
// exporters, in DiscoveryPriority order.
//
// Same encryption-at-rest as Credential: passphrases and communities
// are AES-GCM-sealed under the master key.
type Profile struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Version     string        `json:"version"` // 'v2c' | 'v3'
	Port        uint16        `json:"port"`
	Interval    time.Duration `json:"-"`
	IntervalSec uint32        `json:"interval_sec"`

	Community string `json:"community,omitempty"`

	V3Username  string `json:"v3_username,omitempty"`
	V3AuthProto string `json:"v3_auth_proto,omitempty"`
	V3AuthPass  string `json:"v3_auth_pass,omitempty"`
	V3PrivProto string `json:"v3_priv_proto,omitempty"`
	V3PrivPass  string `json:"v3_priv_pass,omitempty"`
	V3Context   string `json:"v3_context,omitempty"`

	UseForDiscovery   bool   `json:"use_for_discovery"`
	DiscoveryPriority uint16 `json:"discovery_priority"`

	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`

	HasCommunity bool `json:"has_community"`
	HasAuthPass  bool `json:"has_auth_pass"`
	HasPrivPass  bool `json:"has_priv_pass"`
}

// Reserved IDs assigned by migration 000020 when converting the legacy
// snmp_global_defaults rows into named profiles. Exposed so tests and
// docs can reference them.
const (
	MigratedV2cProfileID = "00000000-0000-0000-0000-0000000002c0"
	MigratedV3ProfileID  = "00000000-0000-0000-0000-0000000003a0"
)

// ErrProfileNotFound is returned by GetProfile when no profile exists
// with the given id (or it has been tombstoned).
var ErrProfileNotFound = fmt.Errorf("snmpx: profile not found")

// ErrProfileInUse is returned by DeleteProfile when one or more
// Credential bindings still reference the profile. The api surfaces
// this as a 409 so the operator unbinds first.
var ErrProfileInUse = fmt.Errorf("snmpx: profile is in use")

// NewProfileID returns a fresh UUIDv4 string for a new profile. Kept
// here so the api handler doesn't depend on google/uuid directly.
func NewProfileID() string { return uuid.NewString() }

// FromProfile projects a Profile (with optional binding overrides for
// port / interval) into a Config the SNMP client can use. The
// scheduler calls this once it has resolved a binding's profile.
func FromProfile(p *Profile, portOverride uint16, intervalOverride time.Duration) Config {
	if p == nil {
		return Config{}
	}
	cfg := Config{
		Version:     p.Version,
		Port:        p.Port,
		Community:   p.Community,
		V3Username:  p.V3Username,
		V3AuthProto: p.V3AuthProto,
		V3AuthPass:  p.V3AuthPass,
		V3PrivProto: p.V3PrivProto,
		V3PrivPass:  p.V3PrivPass,
		V3Context:   p.V3Context,
	}
	if portOverride > 0 {
		cfg.Port = portOverride
	}
	_ = intervalOverride // interval lives outside the SNMP Config; the scheduler reads it separately
	return cfg
}

const profileColsRedacted = `
id, name, version, port, interval_sec,
v3_username, v3_auth_proto, v3_priv_proto, v3_context,
length(community_ct)    > 0 AS has_community,
length(v3_auth_pass_ct) > 0 AS has_auth_pass,
length(v3_priv_pass_ct) > 0 AS has_priv_pass,
use_for_discovery, discovery_priority,
updated_at, updated_by`

// GetProfile returns the profile with secrets DECRYPTED. Called by
// the scheduler at walk time. Returns ErrProfileNotFound when the row
// does not exist or has been tombstoned.
func (s *chCredStore) GetProfile(ctx context.Context, id string) (*Profile, error) {
	if id == "" {
		return nil, ErrProfileNotFound
	}
	const q = `
SELECT
    name, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    updated_at, updated_by
FROM snmp_profiles FINAL
WHERE id = ? AND deleted = 0`
	row := s.conn.QueryRow(ctx, q, id)
	var (
		p                           Profile
		communityCT, authCT, privCT string
		discFlag                    uint8
	)
	if err := row.Scan(
		&p.Name, &p.Version, &p.Port, &p.IntervalSec,
		&communityCT, &p.V3Username, &p.V3AuthProto, &authCT,
		&p.V3PrivProto, &privCT, &p.V3Context,
		&discFlag, &p.DiscoveryPriority,
		&p.UpdatedAt, &p.UpdatedBy,
	); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrProfileNotFound
		}
		return nil, fmt.Errorf("snmpx.profiles: scan: %w", err)
	}
	p.ID = id
	p.Interval = time.Duration(p.IntervalSec) * time.Second
	p.UseForDiscovery = discFlag == 1
	p.HasCommunity = communityCT != ""
	p.HasAuthPass = authCT != ""
	p.HasPrivPass = privCT != ""

	var err error
	if p.Community, err = s.crypter.Decrypt(communityCT); err != nil {
		return nil, fmt.Errorf("snmpx.profiles: decrypt community: %w", err)
	}
	if p.V3AuthPass, err = s.crypter.Decrypt(authCT); err != nil {
		return nil, fmt.Errorf("snmpx.profiles: decrypt auth: %w", err)
	}
	if p.V3PrivPass, err = s.crypter.Decrypt(privCT); err != nil {
		return nil, fmt.Errorf("snmpx.profiles: decrypt priv: %w", err)
	}
	return &p, nil
}

// ListProfiles returns every non-tombstoned profile with secrets
// REDACTED. Ordered by name for stable rendering.
func (s *chCredStore) ListProfiles(ctx context.Context) ([]Profile, error) {
	q := `SELECT ` + profileColsRedacted + ` FROM snmp_profiles FINAL WHERE deleted = 0 ORDER BY name`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmpx.profiles: list: %w", err)
	}
	defer rows.Close()
	out := make([]Profile, 0, 16)
	for rows.Next() {
		var (
			p        Profile
			discFlag uint8
		)
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Version, &p.Port, &p.IntervalSec,
			&p.V3Username, &p.V3AuthProto, &p.V3PrivProto, &p.V3Context,
			&p.HasCommunity, &p.HasAuthPass, &p.HasPrivPass,
			&discFlag, &p.DiscoveryPriority,
			&p.UpdatedAt, &p.UpdatedBy,
		); err != nil {
			return nil, fmt.Errorf("snmpx.profiles: scan list: %w", err)
		}
		p.UseForDiscovery = discFlag == 1
		p.Interval = time.Duration(p.IntervalSec) * time.Second
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProfile upserts a profile. p.ID is required (callers use
// NewProfileID for fresh rows). Empty passphrase fields preserve the
// existing secret — same "leave blank to keep" convention as the
// per-exporter binding store. Name uniqueness is enforced here via a
// best-effort precheck; ClickHouse does not enforce uniqueness so
// concurrent SetProfile calls with the same name can still race, but
// the api layer is single-writer in practice.
func (s *chCredStore) SetProfile(ctx context.Context, p Profile, actor string) error {
	if p.ID == "" {
		return fmt.Errorf("snmpx.profiles: id required")
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("snmpx.profiles: name required")
	}
	switch p.Version {
	case "v2c":
		if p.Community == "" && !p.HasCommunity {
			return fmt.Errorf("snmpx.profiles: v2c profile requires community")
		}
	case "v3":
		if p.V3Username == "" {
			return fmt.Errorf("snmpx.profiles: v3 profile requires username")
		}
	default:
		return fmt.Errorf("snmpx.profiles: invalid version %q (want v2c | v3)", p.Version)
	}
	if p.Port == 0 {
		p.Port = 161
	}
	if p.IntervalSec == 0 {
		p.IntervalSec = 60
	}

	// Preserve existing secrets when caller passed blank (the api layer
	// already does this for normal PUT, but we double-check here so the
	// invariant holds for any caller).
	if p.Community == "" || p.V3AuthPass == "" || p.V3PrivPass == "" {
		if existing, err := s.GetProfile(ctx, p.ID); err == nil {
			if p.Community == "" {
				p.Community = existing.Community
			}
			if p.V3AuthPass == "" {
				p.V3AuthPass = existing.V3AuthPass
			}
			if p.V3PrivPass == "" {
				p.V3PrivPass = existing.V3PrivPass
			}
		}
	}

	// Name uniqueness precheck. We tolerate name collisions if the
	// colliding row IS this profile (same id).
	if err := s.checkNameUnique(ctx, p.ID, p.Name); err != nil {
		return err
	}

	communityCT, err := s.crypter.Encrypt(p.Community)
	if err != nil {
		return fmt.Errorf("snmpx.profiles: encrypt community: %w", err)
	}
	authCT, err := s.crypter.Encrypt(p.V3AuthPass)
	if err != nil {
		return fmt.Errorf("snmpx.profiles: encrypt auth: %w", err)
	}
	privCT, err := s.crypter.Encrypt(p.V3PrivPass)
	if err != nil {
		return fmt.Errorf("snmpx.profiles: encrypt priv: %w", err)
	}
	useFlag := uint8(0)
	if p.UseForDiscovery {
		useFlag = 1
	}
	const ins = `
INSERT INTO snmp_profiles
   (id, name, version, port, interval_sec,
    community_ct,
    v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    deleted, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return s.conn.Exec(ctx, ins,
		p.ID, p.Name, p.Version, p.Port, p.IntervalSec,
		communityCT,
		p.V3Username, p.V3AuthProto, authCT,
		p.V3PrivProto, privCT, p.V3Context,
		useFlag, p.DiscoveryPriority,
		uint8(0), time.Now().UTC(), actorOr(actor),
	)
}

// DeleteProfile tombstones the profile (deleted=1). Refuses with
// ErrProfileInUse when any non-deleted credential row still references
// the id — callers must unbind first.
func (s *chCredStore) DeleteProfile(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("snmpx.profiles: id required")
	}
	// Refuse if any binding still references this profile.
	const refQ = `SELECT count() FROM snmp_credentials FINAL WHERE profile_id = ?`
	row := s.conn.QueryRow(ctx, refQ, id)
	var n uint64
	if err := row.Scan(&n); err != nil {
		return fmt.Errorf("snmpx.profiles: count refs: %w", err)
	}
	if n > 0 {
		return ErrProfileInUse
	}

	// Read existing row so the tombstone preserves the name + identity
	// fields (so the row still scans cleanly).
	existing, err := s.GetProfile(ctx, id)
	if err != nil {
		if err == ErrProfileNotFound {
			return nil // already deleted; idempotent
		}
		return err
	}
	communityCT, _ := s.crypter.Encrypt(existing.Community)
	authCT, _ := s.crypter.Encrypt(existing.V3AuthPass)
	privCT, _ := s.crypter.Encrypt(existing.V3PrivPass)
	useFlag := uint8(0)
	if existing.UseForDiscovery {
		useFlag = 1
	}
	const ins = `
INSERT INTO snmp_profiles
   (id, name, version, port, interval_sec,
    community_ct,
    v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    deleted, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	return s.conn.Exec(ctx, ins,
		existing.ID, existing.Name, existing.Version, existing.Port, existing.IntervalSec,
		communityCT,
		existing.V3Username, existing.V3AuthProto, authCT,
		existing.V3PrivProto, privCT, existing.V3Context,
		useFlag, existing.DiscoveryPriority,
		uint8(1), time.Now().UTC(), "deleted",
	)
}

// DiscoveryProfiles returns the use_for_discovery=1 subset in priority
// order (asc), with ties broken by name. Secrets DECRYPTED so the
// scheduler can build a Client directly.
func (s *chCredStore) DiscoveryProfiles(ctx context.Context) ([]Profile, error) {
	const q = `
SELECT
    id, name, version, port, interval_sec,
    community_ct, v3_username, v3_auth_proto, v3_auth_pass_ct,
    v3_priv_proto, v3_priv_pass_ct, v3_context,
    use_for_discovery, discovery_priority,
    updated_at, updated_by
FROM snmp_profiles FINAL
WHERE deleted = 0 AND use_for_discovery = 1
ORDER BY discovery_priority ASC, name ASC`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("snmpx.profiles: discovery list: %w", err)
	}
	defer rows.Close()
	out := make([]Profile, 0, 8)
	for rows.Next() {
		var (
			p                           Profile
			communityCT, authCT, privCT string
			discFlag                    uint8
		)
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Version, &p.Port, &p.IntervalSec,
			&communityCT, &p.V3Username, &p.V3AuthProto, &authCT,
			&p.V3PrivProto, &privCT, &p.V3Context,
			&discFlag, &p.DiscoveryPriority,
			&p.UpdatedAt, &p.UpdatedBy,
		); err != nil {
			return nil, fmt.Errorf("snmpx.profiles: scan discovery: %w", err)
		}
		p.UseForDiscovery = discFlag == 1
		p.Interval = time.Duration(p.IntervalSec) * time.Second
		p.HasCommunity = communityCT != ""
		p.HasAuthPass = authCT != ""
		p.HasPrivPass = privCT != ""
		if p.Community, err = s.crypter.Decrypt(communityCT); err != nil {
			return nil, fmt.Errorf("snmpx.profiles: decrypt community: %w", err)
		}
		if p.V3AuthPass, err = s.crypter.Decrypt(authCT); err != nil {
			return nil, fmt.Errorf("snmpx.profiles: decrypt auth: %w", err)
		}
		if p.V3PrivPass, err = s.crypter.Decrypt(privCT); err != nil {
			return nil, fmt.Errorf("snmpx.profiles: decrypt priv: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// checkNameUnique returns an error if a non-tombstoned profile with a
// different id already uses the same name (case-sensitive compare).
func (s *chCredStore) checkNameUnique(ctx context.Context, id, name string) error {
	const q = `
SELECT id FROM snmp_profiles FINAL
WHERE deleted = 0 AND name = ? AND id != ?
LIMIT 1`
	row := s.conn.QueryRow(ctx, q, name, id)
	var other string
	if err := row.Scan(&other); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil
		}
		return fmt.Errorf("snmpx.profiles: name check: %w", err)
	}
	return fmt.Errorf("snmpx.profiles: name %q already in use", name)
}
