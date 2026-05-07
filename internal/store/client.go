// Package store contains the ClickHouse client, schema migrations, and
// batched writers used by every FlowScope service that persists data.
//
// The store package is the only place SQL lives. Services upstream
// (ingest, alert, snmp, gnmi) call typed methods; they do not embed
// queries. New tables get a numbered migration file under
// internal/store/migrations/ and a typed accessor in this package.
package store

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Open returns a ClickHouse driver.Conn configured from the supplied
// DSN, which is a clickhouse:// URI with the canonical form:
//
//	clickhouse://user:password@host:9000/database
//
// Multiple hosts may be comma-separated. Query parameters tune the
// connection (compress, secure, dial_timeout). On failure to connect,
// Open returns the underlying error so callers can log a clear cause.
func Open(ctx context.Context, dsn string) (driver.Conn, error) {
	opts, err := parseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("store: open clickhouse: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.Ping(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("store: ping clickhouse: %w", err)
	}
	return conn, nil
}

// parseDSN converts a clickhouse:// URL into a clickhouse.Options
// struct. Lives here (not inline) so tests can exercise it without a
// running database.
func parseDSN(dsn string) (*clickhouse.Options, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "clickhouse" && u.Scheme != "clickhouse+tcp" {
		return nil, fmt.Errorf("store: unsupported scheme %q", u.Scheme)
	}
	addr := u.Host
	if addr == "" {
		addr = "localhost:9000"
	}
	user := "default"
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	db := "default"
	if len(u.Path) > 1 {
		db = u.Path[1:] // strip leading /
	}

	opts := &clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: db,
			Username: user,
			Password: pass,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
		DialTimeout:     5 * time.Second,
		MaxOpenConns:    16,
		MaxIdleConns:    4,
		ConnMaxLifetime: time.Hour,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
	}

	q := u.Query()
	if q.Get("secure") == "true" {
		opts.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}
