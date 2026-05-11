package leaderlease

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ConnAdapter wraps a clickhouse-go driver.Conn so it satisfies the
// leaderlease.DB interface. The signatures are otherwise identical;
// the only thing in our way is that driver.QueryRow returns
// driver.Row (concrete type from the driver) and DB.QueryRow returns
// leaderlease.Row (our test seam). The wrapper round-trips the
// concrete row through our interface in one allocation.
type ConnAdapter struct {
	Conn driver.Conn
}

// FromConn is the convenience constructor for cmd/alert.
func FromConn(c driver.Conn) *ConnAdapter { return &ConnAdapter{Conn: c} }

// Exec forwards directly to the underlying driver.
func (a *ConnAdapter) Exec(ctx context.Context, query string, args ...any) error {
	return a.Conn.Exec(ctx, query, args...)
}

// QueryRow wraps the driver's row in a tiny adapter so the return
// type matches leaderlease.Row. The wrapper doesn't add any logic —
// it just narrows the method set.
func (a *ConnAdapter) QueryRow(ctx context.Context, query string, args ...any) Row {
	return driverRow{row: a.Conn.QueryRow(ctx, query, args...)}
}

type driverRow struct {
	row driver.Row
}

func (r driverRow) Scan(dest ...any) error { return r.row.Scan(dest...) }
func (r driverRow) Err() error             { return r.row.Err() }
