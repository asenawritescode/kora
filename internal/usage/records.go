package usage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
)

// tableQuoter is the part of the SQL dialect the record counter needs.
type tableQuoter interface {
	QuoteIdent(name string) string
}

// Records counts a tenant's records: the rows of every parent DocType table.
// Child rows belong to a parent record and a single DocType holds one settings
// row, so neither is counted.
type Records struct {
	DB       *sql.DB
	Dialect  tableQuoter
	Registry *doctype.Registry
}

// Count returns the number of records across the tenant's parent DocTypes.
func (r Records) Count(ctx context.Context) (int64, error) {
	var total int64
	for _, dt := range recordDocTypes(r.Registry) {
		n, err := r.countRows(ctx, dt)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (r Records) countRows(ctx context.Context, dt *doctype.DocType) (int64, error) {
	var n int64
	query := "SELECT COUNT(*) FROM " + r.Dialect.QuoteIdent(dt.RawTableName())
	if err := r.DB.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("count records of %s: %w", dt.Name, err)
	}
	return n, nil
}

// recordDocTypes returns the DocTypes whose tables hold records.
func recordDocTypes(registry *doctype.Registry) []*doctype.DocType {
	var parents []*doctype.DocType
	for _, dt := range registry.All() {
		if !dt.IsChildTable && !dt.IsSingle {
			parents = append(parents, dt)
		}
	}
	return parents
}
