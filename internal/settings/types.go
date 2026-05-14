// Package settings holds the typed accessors that the api uses to
// read and write the FlowScope configuration tables introduced in
// migration 000005_settings.sql. One Store per concern (services,
// tokens, allowlist, …), bundled into Store for convenient
// dependency injection from cmd/api/main.go.
//
// All stores are safe for concurrent calls. Secrets that need
// confidentiality at rest (webhook secret, OIDC client secret) are
// sealed via *snmpx.Crypter so we don't grow a second secret root.
// API tokens are stored as bcrypt hashes — the plaintext is shown to
// the operator exactly once on creation.
package settings

import (
	"time"

	"github.com/google/uuid"
)

/* ----------------------------- Custom services ----------------------------- */

// CustomService is one operator-defined port → service-name binding,
// optionally a port range. A custom service always outranks the
// built-in dataset (see internal/services.Resolver).
type CustomService struct {
	ID          uuid.UUID `json:"id"`
	Proto       string    `json:"proto"`     // 'tcp' | 'udp' | 'sctp' | 'dccp'
	PortLo      uint16    `json:"port_lo"`
	PortHi      uint16    `json:"port_hi"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Group       string    `json:"group,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

/* ----------------------------- API tokens ----------------------------- */

// APIToken is the operator-visible shape of a token row. Plaintext is
// only ever populated on Create — afterwards the hash + prefix are
// the only forms that exist anywhere.
type APIToken struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`            // first 6 chars of plaintext
	Plaintext  string    `json:"plaintext,omitempty"` // populated on Create only
	Scope      string    `json:"scope"`             // 'read' | 'write' | 'admin'
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	RevokedAt  time.Time `json:"revoked_at,omitempty"`
}

/* ----------------------------- Exporter allowlist ----------------------------- */

// ExporterEntry is one row of the allowlist. enabled=false keeps the
// row but stops accepting flows from that exporter — useful for
// "muted" devices during maintenance windows.
type ExporterEntry struct {
	Exporter  string    `json:"exporter"`
	Label     string    `json:"label,omitempty"`
	Enabled   bool      `json:"enabled"`
	Notes     string    `json:"notes,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

/* ----------------------------- App settings (KV) ----------------------------- */

// AppSettingValue carries the value blob for one named setting. The
// value is JSON so the api can store any shape without a schema
// migration; handler-side validators are the source of truth for
// valid keys.
type AppSettingValue struct {
	Name      string    `json:"name"`
	Value     any       `json:"value"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

/* ----------------------------- Alert rule tunables ----------------------------- */

// AlertRuleSetting is the operator-tunable wrapper around a Go-coded
// rule in internal/alerteng. Params is rule-specific JSON validated
// by the rule's loader; severity overrides the rule's default if
// non-empty.
//
// Deprecated: superseded by AlertRuleInstance. Reads still work for
// one release cycle; writes go through the instance store. The
// migration in 000017 seeds an instance per alert_rule_settings row.
type AlertRuleSetting struct {
	RuleID     string    `json:"rule_id"`
	Enabled    bool      `json:"enabled"`
	Severity   string    `json:"severity,omitempty"`
	Params     any       `json:"params,omitempty"`
	Runbook    string    `json:"runbook,omitempty"`
	Channels   []string  `json:"channels,omitempty"` // webhook IDs
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
}

/* ----------------------------- Alert rule instances ----------------------------- */

// AlertRuleInstance is one operator-created binding of a built-in
// template to a scope and a parameter set. The alert engine iterates
// instances on every tick; each violation it emits carries the
// InstanceID that produced it, so ack/silence/dedup are per-instance.
//
// Two instances of the same template with overlapping scopes are
// legitimate ("warn at 80%" + "page at 95%") — they fire as
// independent alerts the operator can act on separately.
//
// IsSeed marks the per-template "default · all devices" instance the
// migration auto-seeded (or the api lazy-seeded on first read for
// templates with no prior alert_rule_settings override). Operators
// can edit a seed row's params to change the global default; they
// cannot delete it.
type AlertRuleInstance struct {
	InstanceID string         `json:"instance_id"`
	TemplateID string         `json:"template_id"`
	Name       string         `json:"name"`
	Enabled    bool           `json:"enabled"`
	Severity   string         `json:"severity,omitempty"`
	Scope      ScopeSelector  `json:"scope"`
	Params     map[string]any `json:"params,omitempty"`
	Runbook    string         `json:"runbook,omitempty"`
	Channels   []string       `json:"channels,omitempty"`
	IsSeed     bool           `json:"is_seed"`
	CreatedAt  time.Time      `json:"created_at,omitempty"`
	UpdatedAt  time.Time      `json:"updated_at,omitempty"`
	UpdatedBy  string         `json:"updated_by,omitempty"`
}

// ScopeSelector is the structured matcher that binds an instance to
// a subset of devices, interfaces, peers, etc. Empty fields mean
// "no filter on this dimension"; an empty selector matches
// everything (that's how seed instances behave).
//
// Phase 1 supports exact-IP and ifindex matching. CIDR matching and
// label-based scoping (role=wan_edge) land in phase 3 with the
// device label substrate.
type ScopeSelector struct {
	Exporters     []string          `json:"exporters,omitempty"`        // exact IPs (canonical exporter form)
	ExporterCIDRs []string          `json:"exporter_cidrs,omitempty"`   // CIDR matchers — phase 3
	ExporterLabels map[string]string `json:"exporter_labels,omitempty"` // label matchers — phase 3
	IfIndex       []uint32          `json:"ifindex,omitempty"`          // interface-scoped templates only
	IfNameGlob    string            `json:"ifname_glob,omitempty"`      // glob on if_descr — phase 3
	BGPPeers      []string          `json:"bgp_peers,omitempty"`        // BGP-template only — phase 2
	ASNRemote     []uint32          `json:"asn_remote,omitempty"`       // BGP-template only — phase 2
	VRFs          []string          `json:"vrfs,omitempty"`             // BGP-template only — match peers in these VRFs
}

// IsEmpty reports whether the selector has no filters set. The engine
// uses this to short-circuit the WHERE-clause builder for seed
// instances — an empty selector compiles to no extra predicates.
func (s ScopeSelector) IsEmpty() bool {
	return len(s.Exporters) == 0 &&
		len(s.ExporterCIDRs) == 0 &&
		len(s.ExporterLabels) == 0 &&
		len(s.IfIndex) == 0 &&
		s.IfNameGlob == "" &&
		len(s.BGPPeers) == 0 &&
		len(s.ASNRemote) == 0 &&
		len(s.VRFs) == 0
}

/* ----------------------------- Webhooks ----------------------------- */

// Webhook is one outbound integration target.
type Webhook struct {
	ID              uuid.UUID         `json:"id"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"` // 'slack' | 'teams' | 'pagerduty' | 'http'
	URL             string            `json:"url"`
	Secret          string            `json:"secret,omitempty"` // populated only on PUT; redacted on GET
	HasSecret       bool              `json:"has_secret"`
	HeaderTemplate  map[string]string `json:"header_template,omitempty"`
	Enabled         bool              `json:"enabled"`
	SeverityFilter  []string          `json:"severity_filter,omitempty"` // subset of {'critical','warning','info'}
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
	UpdatedBy       string            `json:"updated_by,omitempty"`
}

/* ----------------------------- OIDC ----------------------------- */

// OIDCConfig is the singleton OIDC integration record. Login flow is
// disabled in v1 even when Enabled=true; the api logs a warning if
// the operator turns it on. Phase 2 wires the actual gating.
type OIDCConfig struct {
	Enabled       bool      `json:"enabled"`
	Issuer        string    `json:"issuer,omitempty"`
	ClientID      string    `json:"client_id,omitempty"`
	ClientSecret  string    `json:"client_secret,omitempty"` // PUT-only
	HasSecret     bool      `json:"has_secret"`
	RedirectURI   string    `json:"redirect_uri,omitempty"`
	Scopes        string    `json:"scopes,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
	UpdatedBy     string    `json:"updated_by,omitempty"`
	// Note for the operator: the api emits this in GET responses so
	// the UI can show "Phase 2 — login flow not yet active" without
	// hard-coding the message.
	LoginFlowStatus string `json:"login_flow_status"`
}
