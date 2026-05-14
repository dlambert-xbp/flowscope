package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Migration files live next to this package and are embedded into the
// binary. Files are named NNNNNN_description.sql and applied in
// numeric order. New schema changes land as new files; never edit a
// migration after release.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies any unapplied migrations to the connected ClickHouse
// instance, in numeric filename order. It is safe to call on every
// startup — migrations are tracked in a meta table that this function
// also creates if absent.
//
// Forward-only by design (VISION.md §8.4). There is no down migration.
func Migrate(ctx context.Context, conn driver.Conn) error {
	if err := ensureMetaTable(ctx, conn); err != nil {
		return fmt.Errorf("store: ensure meta table: %w", err)
	}
	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return fmt.Errorf("store: load applied: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		// Each file may contain multiple statements separated by `;`.
		// We split conservatively — string literals containing semicolons
		// would break this, but our migrations don't use any. Keep it
		// honest and simple.
		for _, stmt := range splitStatements(string(body)) {
			if err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("store: apply %s: %w", name, err)
			}
		}
		if err := conn.Exec(ctx,
			"INSERT INTO _flowscope_migrations (name, applied_at) VALUES (?, now64(3))",
			name,
		); err != nil {
			return fmt.Errorf("store: record %s: %w", name, err)
		}
	}
	return nil
}

func ensureMetaTable(ctx context.Context, conn driver.Conn) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS _flowscope_migrations (
    name        String,
    applied_at  DateTime64(3, 'UTC')
)
ENGINE = MergeTree
ORDER BY name
SETTINGS index_granularity = 8192
`
	return conn.Exec(ctx, ddl)
}

func loadApplied(ctx context.Context, conn driver.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT name FROM _flowscope_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// splitStatements breaks a SQL file into individual statements on `;`.
// Both line-leading and inline `--` comments are stripped first so
// that a semicolon inside an inline comment (e.g.
// `peer_asn UInt32, -- 32-bit ASN; 16-bit fits naturally`) does not
// fragment the surrounding statement. `--` inside a single-quoted
// string literal is preserved. Migrations must not embed semicolons
// inside string literals.
func splitStatements(body string) []string {
	var stripped strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = stripInlineComment(line)
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		stripped.WriteString(line)
		stripped.WriteString("\n")
	}
	var out []string
	for _, raw := range strings.Split(stripped.String(), ";") {
		stmt := strings.TrimSpace(raw)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}

// stripInlineComment returns line truncated at the first `--` that is
// not inside a single-quoted string literal. SQL `--` comments run to
// end-of-line, so removing them at split time is equivalent to
// letting ClickHouse skip them at parse time — but it also keeps
// semicolons inside those comments from breaking the `;`-split.
func stripInlineComment(line string) string {
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\'' {
			// Doubled '' is a SQL escape for a literal apostrophe and
			// stays inside the string. Treat it as one paired character.
			if inStr && i+1 < len(line) && line[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if !inStr && c == '-' && i+1 < len(line) && line[i+1] == '-' {
			return line[:i]
		}
	}
	return line
}
