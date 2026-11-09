package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/query"
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

// candidateConfigResources returns the commands, DocTypes and config queries a
// candidate configuration would activate, layered over the process-wide
// commands. Activation preflight must bind these rather than the active
// configuration's: a package that brings its own local commands (for example
// on a fresh tenant) has none of them active yet.
func candidateConfigResources(base *kernel.CommandRegistry, candidate *doctype.ConfigSnapshot) (*kernel.CommandRegistry, *doctype.Registry, []*query.Definition, error) {
	defs, err := kernel.DecodeCommandResources(candidate.Commands)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode candidate commands: %w", err)
	}
	commands, err := base.Extend(defs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("register candidate commands: %w", err)
	}
	registry := doctype.NewRegistry()
	for _, dt := range candidate.DocTypes {
		registry.Register(dt)
	}
	queries, err := query.Decode(candidate.Queries, registry)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode candidate queries: %w", err)
	}
	return commands, registry, queries, nil
}
