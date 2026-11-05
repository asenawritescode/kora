package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/query"
)

// tenantConfigQueries returns the queries carried by a tenant's active
// configuration; a tenant without one has none.
func tenantConfigQueries(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect, site string, reg *doctype.Registry) ([]*query.Definition, error) {
	snapshot, _, err := configstore.NewStore(database, dialect).LoadActiveSnapshotContext(ctx, site)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load active configuration: %w", err)
	}
	return query.Decode(snapshot.Queries, reg)
}
