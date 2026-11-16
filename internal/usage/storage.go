package usage

import (
	"context"
	"database/sql"
	"fmt"
)

// sizeDialect is the part of the SQL dialect the storage meter needs.
type sizeDialect interface {
	DatabaseSizeQuery() string
}

// Storage measures a tenant's database size.
type Storage struct {
	DB      *sql.DB
	Dialect sizeDialect
}

// Bytes returns the current database's data and index size in bytes.
func (s Storage) Bytes(ctx context.Context) (int64, error) {
	var n int64
	if err := s.DB.QueryRowContext(ctx, s.Dialect.DatabaseSizeQuery()).Scan(&n); err != nil {
		return 0, fmt.Errorf("measure database size: %w", err)
	}
	return n, nil
}
