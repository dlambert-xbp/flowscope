// Package secrets provides a unified loader for secret material that
// FlowScope services need at startup — most importantly the SNMP master
// key used by internal/snmpx to derive AES-256-GCM record keys (see
// VISION.md §4.2 and CLAUDE.md's "Configuration surface" table).
//
// The package speaks a tiny URI scheme on top of plain strings so a
// single env var (typically FLOWSCOPE_SNMP_KEY_REF) can drive different
// backends across environments without per-binary code changes:
//
//	env:VAR_NAME                               read $VAR_NAME (plaintext literal)
//	file:/path/to/secret                       read file contents, trim trailing \n
//	kv:https://<vault>.vault.azure.net/secrets/<name>[/<version>]
//	                                           Azure Key Vault via Workload Identity
//
// Resolve(ctx, ref) dispatches on the scheme prefix and returns the
// plaintext secret. The same ref MUST return the same plaintext across
// process restarts — the snmp master-key invariant in CLAUDE.md says
// rotating it invalidates every stored v3 credential, so the loader
// itself must be deterministic. (Key rotation is an explicit operator
// action that re-encrypts the credential store; not something we do
// transparently here.)
//
// Backward compatibility: services may also call ResolveSNMPMaster()
// which reads FLOWSCOPE_SNMP_KEY_REF first and falls back to the legacy
// FLOWSCOPE_SNMP_KEY plaintext env var, logging a deprecation warning.
// New deployments should set the _REF form and let it resolve through
// env: / file: / kv: as appropriate.
package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Source loads the plaintext value of a secret given an opaque ref.
// Implementations should treat ref as immutable: the same ref must
// resolve to the same plaintext for the lifetime of the source's
// underlying backing store. Network-backed sources are allowed to
// fail with a transient error; callers (Resolve) do not retry — the
// expectation is that startup logic exits non-zero on failure rather
// than running with an empty key (no silent failures, per CLAUDE.md).
type Source interface {
	Load(ctx context.Context, ref string) (string, error)
}

// ErrUnknownScheme is returned by Resolve when the ref does not start
// with a recognised prefix.
var ErrUnknownScheme = errors.New("secrets: unknown ref scheme")

// ErrEmpty is returned when a source resolves to an empty value. We
// treat empty as a configuration error rather than a valid secret to
// avoid silently disabling crypto downstream.
var ErrEmpty = errors.New("secrets: resolved value is empty")

// Resolve dispatches a ref to the matching source implementation.
//
//	"env:NAME"    → EnvSource
//	"file:/path"  → FileSource
//	"kv:<url>"    → KeyVaultSource (Azure SDK + Workload Identity)
//
// A bare value with no recognised prefix is rejected; callers wanting
// to accept a literal should pass "env:VAR_NAME" and set VAR_NAME, or
// use the convenience ResolveSNMPMaster() which handles the legacy
// plaintext FLOWSCOPE_SNMP_KEY fallback explicitly.
func Resolve(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", ErrEmpty
	}
	switch {
	case strings.HasPrefix(ref, "env:"):
		return EnvSource{}.Load(ctx, ref)
	case strings.HasPrefix(ref, "file:"):
		return FileSource{}.Load(ctx, ref)
	case strings.HasPrefix(ref, "kv:"):
		src, err := NewKeyVaultSource()
		if err != nil {
			return "", fmt.Errorf("secrets: init keyvault source: %w", err)
		}
		return src.Load(ctx, ref)
	default:
		return "", fmt.Errorf("%w: %q (expected env:/file:/kv: prefix)", ErrUnknownScheme, ref)
	}
}

// EnvSource resolves "env:VAR_NAME" by reading the named environment
// variable. The plaintext lives in the process environment for the
// lifetime of the process; this is the lowest-friction backend and is
// appropriate for docker-compose dev stacks where the operator has
// already provisioned the value externally.
type EnvSource struct{}

// Load implements Source.
func (EnvSource) Load(_ context.Context, ref string) (string, error) {
	name := strings.TrimPrefix(ref, "env:")
	if name == "" {
		return "", fmt.Errorf("secrets: env ref %q: missing variable name", ref)
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("secrets: env var %q not set", name)
	}
	if v == "" {
		return "", fmt.Errorf("secrets: env var %q is empty", name)
	}
	return v, nil
}

// FileSource resolves "file:/abs/path" by reading the file's contents
// and trimming a single trailing newline if present (so heredoc-style
// secret files behave like the operator wrote the value with no extra
// whitespace). Permissions are not enforced here — kubelet / docker
// already mount the secret with 0400 when configured correctly.
type FileSource struct{}

// Load implements Source.
func (FileSource) Load(_ context.Context, ref string) (string, error) {
	path := strings.TrimPrefix(ref, "file:")
	if path == "" {
		return "", fmt.Errorf("secrets: file ref %q: missing path", ref)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secrets: read %q: %w", path, err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", fmt.Errorf("secrets: file %q is empty", path)
	}
	return s, nil
}

// Fingerprint returns a stable, non-reversible identifier for a
// plaintext secret. We log the first 8 bytes of SHA-256 (16 hex chars)
// so operators can confirm two replicas / two restarts loaded the same
// key without ever exposing the key itself. The fingerprint is
// deterministic, so it leaks the fact that two secrets are equal, but
// for an operator-facing health signal that's exactly what we want.
func Fingerprint(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:8])
}

// ResolveSessionKey is the convenience entry point for cmd/api's
// Phase 2 OIDC login flow. It mirrors ResolveSNMPMaster: prefers
// FLOWSCOPE_SESSION_KEY_REF (any backend), falls back to plaintext
// FLOWSCOPE_SESSION_KEY with a deprecation warning, returns ("", nil)
// when neither is set (api then disables the /auth/* endpoints and
// runs with the legacy shared/per-token paths only).
//
// Distinct from FLOWSCOPE_SNMP_KEY by design: rotating the session
// key invalidates outstanding signed cookies (operators sign back in)
// but does NOT corrupt the SNMP credential store. The two roots
// independent so an operator can rotate one without disturbing the
// other.
func ResolveSessionKey(ctx context.Context) (string, error) {
	if ref := os.Getenv("FLOWSCOPE_SESSION_KEY_REF"); ref != "" {
		v, err := Resolve(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("FLOWSCOPE_SESSION_KEY_REF resolve: %w", err)
		}
		return v, nil
	}
	if v := os.Getenv("FLOWSCOPE_SESSION_KEY"); v != "" {
		slog.Warn("FLOWSCOPE_SESSION_KEY is set as plaintext; prefer FLOWSCOPE_SESSION_KEY_REF=env:FLOWSCOPE_SESSION_KEY (or file:/path, kv:https://...) in production",
			"fp", Fingerprint(v),
		)
		return v, nil
	}
	return "", nil
}

// ResolveSNMPMaster is the convenience entry point for cmd/snmp,
// cmd/alert and cmd/api. It implements the documented fallback order:
//
//  1. FLOWSCOPE_SNMP_KEY_REF (preferred) → dispatched through Resolve()
//  2. FLOWSCOPE_SNMP_KEY (legacy plaintext) → returned verbatim with a
//     deprecation warning emitted via slog so operators see the cutover
//     note in their startup logs.
//
// If both are unset the function returns ("", nil) — callers interpret
// that as "credential management is disabled" (the existing behaviour
// in cmd/snmp / cmd/api). If FLOWSCOPE_SNMP_KEY_REF is set but Resolve
// fails the error is propagated; we do NOT silently fall back to the
// legacy literal in that case, because doing so could load a different
// key than the operator intended and corrupt the at-rest credential
// store. Per CLAUDE.md "no silent failures" — startup must exit
// non-zero on a botched ref.
func ResolveSNMPMaster(ctx context.Context) (string, error) {
	if ref := os.Getenv("FLOWSCOPE_SNMP_KEY_REF"); ref != "" {
		v, err := Resolve(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("FLOWSCOPE_SNMP_KEY_REF resolve: %w", err)
		}
		return v, nil
	}
	if v := os.Getenv("FLOWSCOPE_SNMP_KEY"); v != "" {
		slog.Warn("FLOWSCOPE_SNMP_KEY is deprecated; set FLOWSCOPE_SNMP_KEY_REF=env:FLOWSCOPE_SNMP_KEY (or file:/path, kv:https://...) instead",
			"fp", Fingerprint(v),
		)
		return v, nil
	}
	return "", nil
}
