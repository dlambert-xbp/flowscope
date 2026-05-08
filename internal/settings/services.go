package settings

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
)

// validProtos mirrors internal/services.validProto. Kept duplicate to
// avoid an import cycle and because the storage layer is the right
// place to refuse a bad row before it lands.
var validProtos = map[string]bool{"tcp": true, "udp": true, "sctp": true, "dccp": true}

func newCustomServicesStore(conn driver.Conn) CustomServicesStore {
	return &chCustomServicesStore{conn: conn}
}

type chCustomServicesStore struct {
	conn driver.Conn
}

const customServicesCols = `id, proto, port_lo, port_hi, name, description, grp, owner, updated_at, updated_by`

func (s *chCustomServicesStore) List(ctx context.Context) ([]CustomService, error) {
	q := `SELECT ` + customServicesCols + ` FROM custom_services FINAL ORDER BY proto, port_lo, port_hi, name`
	rows, err := s.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("custom_services: list: %w", err)
	}
	defer rows.Close()
	out := make([]CustomService, 0, 16)
	for rows.Next() {
		c, err := scanCustomService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *chCustomServicesStore) Get(ctx context.Context, id string) (*CustomService, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("custom_services: parse id: %w", err)
	}
	q := `SELECT ` + customServicesCols + ` FROM custom_services FINAL WHERE id = ?`
	row := s.conn.QueryRow(ctx, q, uid)
	c, err := scanCustomService(row)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *chCustomServicesStore) Upsert(ctx context.Context, c CustomService, actor string) (*CustomService, error) {
	c.Proto = strings.ToLower(strings.TrimSpace(c.Proto))
	if !validProtos[c.Proto] {
		return nil, fmt.Errorf("custom_services: invalid proto %q", c.Proto)
	}
	if c.PortLo == 0 || c.PortHi == 0 {
		return nil, fmt.Errorf("custom_services: ports must be 1..65535")
	}
	if c.PortLo > c.PortHi {
		return nil, fmt.Errorf("custom_services: port_lo > port_hi")
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return nil, fmt.Errorf("custom_services: name required")
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.UpdatedAt = time.Now().UTC()
	c.UpdatedBy = actorOr(actor)

	const ins = `
INSERT INTO custom_services
   (id, proto, port_lo, port_hi, name, description, grp, owner, updated_at, updated_by)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, ins,
		c.ID, c.Proto, c.PortLo, c.PortHi,
		c.Name, c.Description, c.Group, c.Owner,
		c.UpdatedAt, c.UpdatedBy,
	); err != nil {
		return nil, fmt.Errorf("custom_services: insert: %w", err)
	}
	return &c, nil
}

func (s *chCustomServicesStore) Delete(ctx context.Context, id, actor string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("custom_services: parse id: %w", err)
	}
	const q = `ALTER TABLE custom_services DELETE WHERE id = ?`
	if err := s.conn.Exec(ctx, q, uid); err != nil {
		return fmt.Errorf("custom_services: delete: %w", err)
	}
	_ = actor // captured in audit log by the handler
	return nil
}

// scanCustomService accepts either *sql.Row or driver.Rows shape.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCustomService(r rowScanner) (*CustomService, error) {
	var c CustomService
	if err := r.Scan(
		&c.ID, &c.Proto, &c.PortLo, &c.PortHi,
		&c.Name, &c.Description, &c.Group, &c.Owner,
		&c.UpdatedAt, &c.UpdatedBy,
	); err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("custom_services: scan: %w", err)
	}
	return &c, nil
}

func actorOr(s string) string {
	if s == "" {
		return "anonymous"
	}
	return s
}
