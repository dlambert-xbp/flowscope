package store

import (
	"strings"
	"testing"
)

func TestStripInlineComment(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain line", "SELECT 1", "SELECT 1"},
		{"trailing comment", "SELECT 1 -- foo", "SELECT 1 "},
		{"semicolon in comment", "x UInt32, -- 32-bit ASN; 16-bit fits", "x UInt32, "},
		{"comment with parens", "x UInt32, -- (pre-cbgp / pre-jnxbgp)", "x UInt32, "},
		{"string literal containing dashes is preserved", "DEFAULT '--keep me' -- but not me", "DEFAULT '--keep me' "},
		{"escaped apostrophe in string", "name = 'it''s ok' -- gone", "name = 'it''s ok' "},
		{"only a comment", "-- header line", ""},
		{"leading spaces + comment", "    -- indented", "    "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripInlineComment(tc.in)
			if got != tc.want {
				t.Errorf("stripInlineComment(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitStatements_SemicolonInsideInlineCommentDoesNotSplit(t *testing.T) {
	// Regression for migration 000018_bgp_peers.sql: a `--` comment
	// containing a semicolon must not split the surrounding statement.
	body := `CREATE TABLE foo (
    a UInt32, -- 32-bit ASN; 16-bit fits naturally
    b UInt32
)`
	got := splitStatements(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 statement, got %d: %q", len(got), got)
	}
	if !strings.Contains(got[0], "CREATE TABLE foo") {
		t.Errorf("statement missing CREATE TABLE: %q", got[0])
	}
	if strings.Contains(got[0], "--") {
		t.Errorf("statement still contains stripped comment: %q", got[0])
	}
}

func TestSplitStatements_MultipleStatementsStillSplit(t *testing.T) {
	body := `CREATE TABLE a (x UInt32);
ALTER TABLE a ADD COLUMN y String DEFAULT '';
INSERT INTO a (x) SELECT 1`
	got := splitStatements(body)
	if len(got) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(got))
	}
}

func TestSplitStatements_StringLiteralWithDashesPreserved(t *testing.T) {
	body := `INSERT INTO t (k) VALUES ('--literal-dashes')`
	got := splitStatements(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(got))
	}
	if !strings.Contains(got[0], "'--literal-dashes'") {
		t.Errorf("string literal got mangled: %q", got[0])
	}
}
