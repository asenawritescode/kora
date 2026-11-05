package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/kernel"
)

// tenantCommandRegistry layers the commands carried by a tenant's active
// configuration over the process-wide registry. A tenant without an active
// configuration version simply inherits the process-wide commands.
func tenantCommandRegistry(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect, site string, base *kernel.CommandRegistry) (*kernel.CommandRegistry, error) {
	snapshot, _, err := configstore.NewStore(database, dialect).LoadActiveSnapshotContext(ctx, site)
	if errors.Is(err, sql.ErrNoRows) {
		return base.Extend(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("load active configuration: %w", err)
	}
	defs, err := kernel.DecodeCommandResources(snapshot.Commands)
	if err != nil {
		return nil, err
	}
	return base.Extend(defs)
}
