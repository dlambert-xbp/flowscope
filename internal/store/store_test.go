package store

import (
	"strings"
	"testing"
)

func TestParseDSN_Basic(t *testing.T) {
	opts, err := parseDSN("clickhouse://flowscope:secret@10.0.0.5:9000/flowscope")
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.Addr[0]; got != "10.0.0.5:9000" {
		t.Errorf("Addr = %q", got)
	}
	if got := opts.Auth.Username; got != "flowscope" {
		t.Errorf("Username = %q", got)
	}
	if got := opts.Auth.Password; got != "secret" {
		t.Errorf("Password = %q", got)
	}
	if got := opts.Auth.Database; got != "flowscope" {
		t.Errorf("Database = %q", got)
	}
}

func TestParseDSN_NoPath_DefaultsDatabase(t *testing.T) {
	opts, err := parseDSN("clickhouse://default@localhost:9000")
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.Auth.Database; got != "default" {
		t.Errorf("Database = %q, want default", got)
	}
}

func TestParseDSN_BadScheme(t *testing.T) {
	if _, err := parseDSN("postgres://x@localhost"); err == nil {
		t.Fatal("expected error for non-clickhouse scheme")
	}
}

func TestParseDSN_Secure(t *testing.T) {
	opts, err := parseDSN("clickhouse://u:p@host:9440/db?secure=true")
	if err != nil {
		t.Fatal(err)
	}
	if opts.TLS == nil {
		t.Fatal("TLS not set despite secure=true")
	}
}

func TestSplitStatements_DropsCommentsAndEmpty(t *testing.T) {
	body := `
-- this is a comment
CREATE TABLE foo (id UInt32) ENGINE = Memory;

-- another comment
INSERT INTO foo VALUES (1);

;
`
	stmts := splitStatements(body)
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2: %#v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], "CREATE TABLE foo") {
		t.Errorf("stmts[0] = %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "INSERT INTO foo") {
		t.Errorf("stmts[1] = %q", stmts[1])
	}
}

// Regression: a semicolon inside a -- comment must not fragment the
// next real statement. Earlier the function split on ; first, then
// stripped comments, which let "never edit; CREATE TABLE..." leak
// through with the comment prefix lost on the second half.
func TestSplitStatements_SemicolonInsideComment(t *testing.T) {
	body := `
-- forward-only; never edit a migration after release.
CREATE TABLE flows (id UInt32) ENGINE = Memory;
`
	stmts := splitStatements(body)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1: %#v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], "CREATE TABLE flows") {
		t.Errorf("stmts[0] = %q", stmts[0])
	}
	if strings.Contains(stmts[0], "never edit") {
		t.Errorf("comment leaked into statement: %q", stmts[0])
	}
}
