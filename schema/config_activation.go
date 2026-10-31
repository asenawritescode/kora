package schema

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// PlanSnapshotActivation loads the live tenant schema and computes the DDL
// needed before a full configuration snapshot can be committed. The returned
// statements must be durably journaled before execution because DDL is not
// transactional on every supported dialect.
func PlanSnapshotActivation(database *sql.DB, dbName string, dialect db.Dialect, registry *doctype.Registry, snapshot *doctype.ConfigSnapshot, changeList, revisionID string) ([]string, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("config activation planning requires a database and dialect")
	}
	liveSchema, err := LoadLiveSchema(database, dbName, dialect)
	if err != nil {
		return nil, err
	}
	var getDocType func(string) *doctype.DocType
	if registry != nil {
		getDocType = registry.Get
	}
	statements, err := GenerateSnapshotActivationDDL(snapshot, getDocType, liveSchema, dialect, changeList, revisionID)
	if err != nil {
		return nil, err
	}
	if dialect.DriverName() == "libsql" {
		if err := appendMissingLibSQLTriggers(database, snapshot, liveSchema, dialect, &statements); err != nil {
			return nil, err
		}
	}
	return statements, nil
}

func appendMissingLibSQLTriggers(database *sql.DB, snapshot *doctype.ConfigSnapshot, liveSchema map[string]*TableInfo, dialect db.Dialect, statements *[]string) error {
	// SQLite schema introspection exposes tables, columns, and indexes to
	// ComputeDiff, but a table can exist while its modified-on-update trigger
	// is missing after an interrupted multi-statement activation. Recreate that
	// required trigger idempotently during every live-schema replan so a durable
	// retry cannot commit a table without it.
	for _, dt := range snapshot.DocTypes {
		if dt == nil {
			continue
		}
		tableName := dt.RawTableName()
		if liveSchema[tableName] == nil {
			continue // CreateTable's original plan includes the trigger.
		}
		triggerName := tableName + "_modified_on_update"
		var exists int
		if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?`, triggerName).Scan(&exists); err != nil {
			return fmt.Errorf("checking LibSQL modified trigger %s: %w", triggerName, err)
		}
		if exists != 0 {
			continue
		}
		createTable := dialect.CreateTable(dt)
		if len(createTable) == 0 {
			return fmt.Errorf("LibSQL dialect returned no create-table DDL for %s", dt.Name)
		}
		*statements = append(*statements, createTable[len(createTable)-1])
	}
	return nil
}

// GenerateSnapshotActivationDDL derives the ordinary live-schema diff and
// preserves explicit historical field removals/type changes from a config
// version's change list. It is pure so both the HTTP compatibility route and
// the durable management executor can use exactly the same DDL plan.
func GenerateSnapshotActivationDDL(snapshot *doctype.ConfigSnapshot, getDocType func(string) *doctype.DocType, liveSchema map[string]*TableInfo, dialect db.Dialect, changeList, revisionID string) ([]string, error) {
	if snapshot == nil || dialect == nil {
		return nil, fmt.Errorf("config activation planning requires a snapshot and dialect")
	}
	// Resolve child doctypes from the target snapshot first. During managed
	// activation the runtime registry still represents the currently active
	// version, so it cannot resolve child doctypes that are being introduced by
	// this snapshot. Falling back to the registry remains useful for historical
	// change-list processing and snapshots that retain references to existing
	// doctypes outside the snapshot.
	targetDocTypes := make(map[string]*doctype.DocType, len(snapshot.DocTypes))
	for _, dt := range snapshot.DocTypes {
		if dt != nil {
			targetDocTypes[dt.Name] = dt
		}
	}
	resolveDocType := func(name string) *doctype.DocType {
		if dt := targetDocTypes[name]; dt != nil {
			return dt
		}
		if getDocType != nil {
			return getDocType(name)
		}
		return nil
	}
	diff := ComputeDiff(snapshot.DocTypes, resolveDocType, liveSchema, dialect)
	statements := diff.GenerateDDL(snapshot.DocTypes, resolveDocType, dialect)
	if changeList == "" {
		return statements, nil
	}
	var fullDiff doctype.ConfigDiffFull
	var changes []doctype.Change
	if err := json.Unmarshal([]byte(changeList), &fullDiff); err == nil && (fullDiff.Doctypes != nil || len(fullDiff.SectionChanges) > 0) {
		var doctypeChanges []doctype.ConfigChange
		if fullDiff.Doctypes != nil {
			doctypeChanges = fullDiff.Doctypes.Changes
		}
		changes = doctype.ConvertConfigChanges(doctypeChanges, fullDiff.SectionChanges, snapshot)
	} else {
		// Earlier versions stored DiffSExpr output as []doctype.Change. Continue
		// applying its explicit field and table removals while new versions store
		// the richer ConfigDiffFull contract.
		if err := json.Unmarshal([]byte(changeList), &changes); err != nil {
			slog.Warn("activation: stored change_list parse failed; live schema plan omits historical removals", "revision", revisionID, "error", err)
			return statements, nil
		}
	}
	var retained []doctype.Change
	var removedDocTypes []doctype.Change
	for _, change := range changes {
		switch change.Type {
		case "remove-doctype":
			removedDocTypes = append(removedDocTypes, change)
		case "remove-field":
			for _, dt := range snapshot.DocTypes {
				if dt == nil || dt.Name != change.Entity {
					continue
				}
				if table := liveSchema[dt.RawTableName()]; table != nil && table.Columns[change.Field] != nil {
					if change.Attrs == nil {
						change.Attrs = make(map[string]any)
					}
					change.Attrs[":quarantine-name"] = doctype.QuarantineNameForRevision(change.Field, revisionID)
					retained = append(retained, change)
				}
				break
			}
		case "change-field-type":
			// Type changes are not inferred by ComputeDiff; retain the explicit
			// migration while rebuilding the rest of the plan from live schema.
			retained = append(retained, change)
		}
	}
	removalDDL, err := doctype.GenerateDDLFromDiff(retained, dialect)
	if err != nil {
		return nil, fmt.Errorf("generating historical field changes for config activation: %w", err)
	}
	statements = append(statements, removalDDL...)
	for _, change := range removedDocTypes {
		if err := appendDocTypeQuarantineDDL(&statements, change.Entity, resolveDocType(change.Entity), liveSchema, dialect, revisionID); err != nil {
			return nil, err
		}
	}
	return statements, nil
}

// appendDocTypeQuarantineDDL retains removed DocType tables and child tables
// under deterministic revision-scoped names. Replanning after partial DDL is
// safe because already-renamed source tables are absent from liveSchema.
func appendDocTypeQuarantineDDL(statements *[]string, name string, old *doctype.DocType, live map[string]*TableInfo, dialect db.Dialect, revision string) error {
	if name == "" || revision == "" {
		return fmt.Errorf("removed DocType quarantine requires a name and revision")
	}
	if old == nil {
		return fmt.Errorf("cannot quarantine removed DocType %q without its previous schema", name)
	}
	rawTables := []string{old.RawTableName()}
	for _, field := range old.TableFields() {
		rawTables = append(rawTables, old.RawChildTableName(field.Fieldname))
	}
	for _, raw := range rawTables {
		if live[raw] == nil {
			continue
		}
		quarantine := doctype.QuarantineTableNameForRevision(raw, revision)
		if live[quarantine] != nil {
			return fmt.Errorf("cannot quarantine removed DocType %q: destination table %q already exists", name, quarantine)
		}
		*statements = append(*statements, fmt.Sprintf("ALTER TABLE %s RENAME TO %s", dialect.QuoteIdent(raw), dialect.QuoteIdent(quarantine)))
	}
	return nil
}

// ApplySnapshotActivationDDL executes an already-journaled plan using the
// supported dialect's DDL semantics. MySQL statements are intentionally
// applied one by one because DDL may commit independently; the durable caller
// must re-plan from live schema after interruption. LibSQL uses its batch
// transport so its supported multi-statement transaction semantics remain
// intact.
func ApplySnapshotActivationDDL(ctx context.Context, database *sql.DB, dialect db.Dialect, statements []string) error {
	if ctx == nil || database == nil || dialect == nil {
		return fmt.Errorf("config activation DDL requires context, database, and dialect")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dialect.DriverName() == "libsql" {
		if len(statements) == 0 {
			return nil
		}
		if err := dialect.ExecuteBatch(database, statements); err != nil {
			return fmt.Errorf("executing LibSQL config activation DDL batch: %w", err)
		}
		return nil
	}
	for i, statement := range statements {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("executing config activation DDL statement %d: %w", i+1, err)
		}
	}
	return nil
}
