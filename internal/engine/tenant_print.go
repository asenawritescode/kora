package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/print"
)

// tenantPrintFormats decodes the print formats carried by a tenant's active
// configuration, keyed by name. An undecodable format fails the tenant load.
func tenantPrintFormats(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect, site string) (map[string]*print.Format, error) {
	snapshot, _, err := configstore.NewStore(database, dialect).LoadActiveSnapshotContext(ctx, site)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load active configuration: %w", err)
	}
	formats := make(map[string]*print.Format, len(snapshot.PrintFormats))
	for _, raw := range snapshot.PrintFormats {
		f, err := print.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode print format: %w", err)
		}
		formats[f.Name] = f
	}
	return formats, nil
}
