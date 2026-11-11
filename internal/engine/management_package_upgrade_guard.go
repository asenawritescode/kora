package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/schema"
)

// packageRowCounter reports how many records a table holds.
type packageRowCounter func(ctx context.Context, table string) (int64, error)

// prepareManagementPackageTarget merges a package into the tenant's configuration and checks it is
// safe for the records the tenant already has. A non-nil result refuses the operation. It runs
// before the activation journal exists, so a retry of a half-applied package never meets it.
func prepareManagementPackageTarget(ctx context.Context, database *sql.DB, dbName string, dialect db.Dialect, current, incoming *doctype.ConfigSnapshot) (*doctype.ConfigSnapshot, *management.FinishResult) {
	target, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		refusal := failed(contract.CodeConflict, "package configuration conflicts with existing tenant configuration")
		return nil, &refusal
	}
	live, err := schema.LoadLiveSchema(database, dbName, dialect)
	if err != nil {
		refusal := packageRecoveryRequired("package safety check could not read the tenant schema")
		return nil, &refusal
	}
	blockers, err := packageUpgradeBlockers(ctx, live, countTableRows(database, dialect), current, incoming)
	if err != nil {
		refusal := packageRecoveryRequired("package safety check could not count the tenant's records")
		return nil, &refusal
	}
	if len(blockers) > 0 {
		refusal := failed(contract.CodeConflict, truncateUTF8("package would invalidate existing records: "+strings.Join(blockers, "; ")+". Publish those fields as optional or with a default.", 480))
		return nil, &refusal
	}
	return target, nil
}

// packageUpgradeBlockers names what a package would break in tables that already hold records: a
// required field with no default (old records would have no value), or a unique field (old
// records cannot be shown to be unique). The same changes on an empty table, an optional field, or
// a field with a default are fine. A table is only counted when a package field could break it.
func packageUpgradeBlockers(ctx context.Context, live map[string]*schema.TableInfo, count packageRowCounter, current, incoming *doctype.ConfigSnapshot) ([]string, error) {
	existing := make(map[string]*doctype.DocType, len(current.DocTypes))
	for _, dt := range current.DocTypes {
		if dt != nil {
			existing[dt.Name] = dt
		}
	}
	var blockers []string
	for _, dt := range incoming.DocTypes {
		if dt == nil || live[dt.RawTableName()] == nil {
			continue
		}
		risky := riskyFields(dt, existing[dt.Name])
		if len(risky) == 0 {
			continue
		}
		records, err := count(ctx, dt.RawTableName())
		if err != nil {
			return nil, err
		}
		if records > 0 {
			blockers = append(blockers, fmt.Sprintf("%s has %d records and the package adds %s", dt.Name, records, strings.Join(risky, ", ")))
		}
	}
	return blockers, nil
}

// riskyFields lists what the package brings to one record type that old records could not satisfy.
// prior is the type as the tenant has it now, nil when the type is new to the tenant.
func riskyFields(dt, prior *doctype.DocType) []string {
	var risky []string
	for _, field := range dt.DataFields() {
		if field.Fieldtype == "Table" {
			continue
		}
		var old *doctype.Field
		if prior != nil {
			old = prior.GetField(field.Fieldname)
		}
		if risk := fieldRisk(field, old); risk != "" {
			risky = append(risky, fmt.Sprintf("%s %q", risk, field.Fieldname))
		}
	}
	return risky
}

// fieldRisk says what is risky about a field the package brings, or "" when nothing is.
func fieldRisk(field doctype.Field, prior *doctype.Field) string {
	switch {
	case prior == nil && field.Reqd && field.Default == "" && field.Unique:
		return "required, unique field"
	case prior == nil && field.Reqd && field.Default == "":
		return "required field"
	case prior == nil && field.Unique, prior != nil && field.Unique && !prior.Unique:
		return "unique field"
	}
	return ""
}

func countTableRows(database *sql.DB, dialect db.Dialect) packageRowCounter {
	return func(ctx context.Context, table string) (int64, error) {
		schemaDialect, ok := dialect.(db.SchemaDialect)
		if !ok {
			return 0, fmt.Errorf("dialect %T cannot quote table names", dialect)
		}
		var records int64
		if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+schemaDialect.QuoteIdent(table)).Scan(&records); err != nil {
			return 0, fmt.Errorf("count records of %s: %w", table, err)
		}
		return records, nil
	}
}
