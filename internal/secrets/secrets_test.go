package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvSource(t *testing.T) {
	t.Setenv("FLOWSCOPE_TEST_SECRET", "hunter2")

	got, err := EnvSource{}.Load(context.Background(), "env:FLOWSCOPE_TEST_SECRET")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("got %q, want hunter2", got)
	}
}

func TestEnvSource_MissingVar(t *testing.T) {
	os.Unsetenv("FLOWSCOPE_TEST_MISSING")
	_, err := EnvSource{}.Load(context.Background(), "env:FLOWSCOPE_TEST_MISSING")
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
}

func TestEnvSource_EmptyVar(t *testing.T) {
	t.Setenv("FLOWSCOPE_TEST_EMPTY", "")
	_, err := EnvSource{}.Load(context.Background(), "env:FLOWSCOPE_TEST_EMPTY")
	if err == nil {
		t.Fatal("expected error for empty env var")
	}
}

func TestEnvSource_NoName(t *testing.T) {
	_, err := EnvSource{}.Load(context.Background(), "env:")
	if err == nil {
		t.Fatal("expected error for missing variable name")
	}
}

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	// Trailing newline is common when operators echo into a file; we
	// must trim it so the fingerprint stays stable.
	if err := os.WriteFile(p, []byte("supersecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileSource{}.Load(context.Background(), "file:"+p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "supersecret" {
		t.Fatalf("got %q, want supersecret", got)
	}
}

func TestFileSource_CRLF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if err := os.WriteFile(p, []byte("supersecret\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileSource{}.Load(context.Background(), "file:"+p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "supersecret" {
		t.Fatalf("got %q, want supersecret", got)
	}
}

func TestFileSource_Missing(t *testing.T) {
	_, err := FileSource{}.Load(context.Background(), "file:/nonexistent/path/should/not/exist")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileSource_Empty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty")
	if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := FileSource{}.Load(context.Background(), "file:"+p)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestFileSource_NoPath(t *testing.T) {
	_, err := FileSource{}.Load(context.Background(), "file:")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestResolve_UnknownScheme(t *testing.T) {
	_, err := Resolve(context.Background(), "azure:literal-not-supported")
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("got %v, want ErrUnknownScheme", err)
	}
}

func TestResolve_Empty(t *testing.T) {
	_, err := Resolve(context.Background(), "")
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
}

func TestResolve_DispatchesEnv(t *testing.T) {
	t.Setenv("FLOWSCOPE_TEST_DISPATCH", "v1")
	got, err := Resolve(context.Background(), "env:FLOWSCOPE_TEST_DISPATCH")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "v1" {
		t.Fatalf("got %q, want v1", got)
	}
}

func TestResolve_DispatchesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if err := os.WriteFile(p, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(context.Background(), "file:"+p)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-file" {
		t.Fatalf("got %q, want from-file", got)
	}
}

func TestFingerprint_Stable(t *testing.T) {
	a := Fingerprint("hunter2")
	b := Fingerprint("hunter2")
	if a != b {
		t.Fatalf("fingerprint not stable: %q vs %q", a, b)
	}
	if a == Fingerprint("hunter3") {
		t.Fatal("fingerprint collision for distinct inputs")
	}
	// 8 bytes hex-encoded = 16 chars; this is the contract operators
	// rely on for visual comparison in startup logs.
	if len(a) != 16 {
		t.Fatalf("fingerprint length %d, want 16", len(a))
	}
}

func TestResolveSNMPMaster_RefPreferredOverLegacy(t *testing.T) {
	// Both set: the _REF form wins, and the legacy literal must NOT be
	// silently used. This protects the master-key invariant.
	dir := t.TempDir()
	p := filepath.Join(dir, "snmp_master")
	if err := os.WriteFile(p, []byte("from-ref"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWSCOPE_SNMP_KEY_REF", "file:"+p)
	t.Setenv("FLOWSCOPE_SNMP_KEY", "from-legacy")

	got, err := ResolveSNMPMaster(context.Background())
	if err != nil {
		t.Fatalf("ResolveSNMPMaster: %v", err)
	}
	if got != "from-ref" {
		t.Fatalf("got %q, want from-ref (legacy must not shadow ref)", got)
	}
}

func TestResolveSNMPMaster_LegacyFallback(t *testing.T) {
	os.Unsetenv("FLOWSCOPE_SNMP_KEY_REF")
	t.Setenv("FLOWSCOPE_SNMP_KEY", "legacy-master")

	got, err := ResolveSNMPMaster(context.Background())
	if err != nil {
		t.Fatalf("ResolveSNMPMaster: %v", err)
	}
	if got != "legacy-master" {
		t.Fatalf("got %q, want legacy-master", got)
	}
}

func TestResolveSNMPMaster_NeitherSet(t *testing.T) {
	os.Unsetenv("FLOWSCOPE_SNMP_KEY_REF")
	os.Unsetenv("FLOWSCOPE_SNMP_KEY")

	got, err := ResolveSNMPMaster(context.Background())
	if err != nil {
		t.Fatalf("ResolveSNMPMaster: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestResolveSNMPMaster_RefFailsDoesNotFallBack(t *testing.T) {
	// If _REF is set but unreadable, we MUST NOT silently use the
	// legacy literal. That would corrupt the credential store by
	// loading a different key than the operator intended.
	t.Setenv("FLOWSCOPE_SNMP_KEY_REF", "file:/this/path/does/not/exist")
	t.Setenv("FLOWSCOPE_SNMP_KEY", "should-not-be-used")

	_, err := ResolveSNMPMaster(context.Background())
	if err == nil {
		t.Fatal("expected error when _REF is set but resolution fails")
	}
	if !strings.Contains(err.Error(), "FLOWSCOPE_SNMP_KEY_REF") {
		t.Fatalf("error %v should reference FLOWSCOPE_SNMP_KEY_REF", err)
	}
}
