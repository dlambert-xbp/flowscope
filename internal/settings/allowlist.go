package settings

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func newAllowlistStore(conn driver.Conn) AllowlistStore {
	return &chAllowlistStore{conn: conn}
}

type chAllowlistStore struct {
	conn driver.Conn
}

func (s *chAllowlistStore) List(ctx context.Context) ([]ExporterEntry, error) {
	const q = `
SELECT IPv6NumToString(exporter), label, enabled, notes, updated_at, updated_by
FROM exporter_allowlist FINAL
ORDER BY exporter`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("allowlist: list: %w", err)
	}
	defer rows.Close()
	out := make([]ExporterEntry, 0, 8)
	for rows.Next() {
		var (
			e       ExporterEntry
			raw     string
			enabled uint8
		)
		if err := rows.Scan(&raw, &e.Label, &enabled, &e.Notes, &e.UpdatedAt, &e.UpdatedBy); err != nil {
			return nil, fmt.Errorf("allowlist: scan: %w", err)
		}
		e.Exporter = unmap4in6(raw)
		e.Enabled = enabled == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *chAllowlistStore) Get(ctx context.Context, exporter string) (*ExporterEntry, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(exporter))
	if err != nil {
		return nil, fmt.Errorf("allowlist: parse exporter: %w", err)
	}
	expIP := toIPv6(addr)
	const q = `
SELECT label, enabled, notes, updated_at, updated_by
FROM exporter_allowlist FINAL WHERE exporter = ?`
	var (
		e       ExporterEntry
		enabled uint8
	)
	if err := s.conn.QueryRow(ctx, q, expIP).Scan(&e.Label, &enabled, &e.Notes, &e.UpdatedAt, &e.UpdatedBy); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("allowlist: get: %w", err)
	}
	e.Exporter = addr.Unmap().String()
	e.Enabled = enabled == 1
	return &e, nil
}

func (s *chAllowlistStore) Upsert(ctx context.Context, e ExporterEntry, actor string) error {
	addr, err := netip.ParseAddr(strings.TrimSpace(e.Exporter))
	if err != nil {
		return fmt.Errorf("allowlist: parse exporter: %w", err)
	}
	expIP := toIPv6(addr)
	enabled := uint8(0)
	if e.Enabled {
		enabled = 1
	}
	const ins = `
INSERT INTO exporter_allowlist (exporter, label, enabled, notes, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins, expIP, e.Label, enabled, e.Notes,
		time.Now().UTC(), actorOr(actor)); err != nil {
		return fmt.Errorf("allowlist: upsert: %w", err)
	}
	return nil
}

func (s *chAllowlistStore) Delete(ctx context.Context, exporter, actor string) error {
	addr, err := netip.ParseAddr(strings.TrimSpace(exporter))
	if err != nil {
		return fmt.Errorf("allowlist: parse exporter: %w", err)
	}
	expIP := toIPv6(addr)
	const q = `ALTER TABLE exporter_allowlist DELETE WHERE exporter = ?`
	if err := s.conn.Exec(ctx, q, expIP); err != nil {
		return fmt.Errorf("allowlist: delete: %w", err)
	}
	_ = actor
	return nil
}

// toIPv6 returns the 16-byte big-endian net.IP form for the IPv6
// ClickHouse column. Matches the helper in internal/snmpx.
func toIPv6(addr netip.Addr) net.IP {
	a := addr.As16()
	return a[:]
}

func unmap4in6(s string) string {
	const pfx = "::ffff:"
	if len(s) > len(pfx) && s[:len(pfx)] == pfx {
		return s[len(pfx):]
	}
	return s
}
