package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
)

// packageRowCounter reports how many records a table holds.
type packageRowCounter func(ctx context.Context, table string) (int64, error)

// packageTenantDB is the tenant database a package is checked against.
type packageTenantDB struct {
	conn    *sql.DB
	name    string
	dialect db.Dialect
	tenant  contract.TenantID
}

// prepareManagementPackageTarget merges a package into the tenant's configuration and checks it is
// safe for the records the tenant already has. A non-nil result refuses the operation. It runs
// before the activation journal exists, so a retry of a half-applied package never meets it.
func prepareManagementPackageTarget(ctx context.Context, tenant packageTenantDB, current, incoming *doctype.ConfigSnapshot, authority packageAuthority) (*doctype.ConfigSnapshot, *management.FinishResult) {
	target, err := mergeManagementPackageSnapshot(current, incoming, authority)
	if err != nil {
		refusal := failed(contract.CodeConflict, truncateUTF8("package configuration conflicts with existing tenant configuration: "+err.Error(), 480))
		return nil, &refusal
	}
	registry := doctype.NewRegistry()
	registry.LoadFull(target.DocTypes, target.Roles, target.Permissions)
	if err = site.ValidateWorkflowDefinitions(target.WorkflowDefinitions, registry); err != nil {
		refusal := failed(contract.CodeValidationFailed, truncateUTF8("package workflow definitions failed validation: "+err.Error(), 480))
		return nil, &refusal
	}
	live, err := schema.LoadLiveSchema(tenant.conn, tenant.name, tenant.dialect)
	if err != nil {
		refusal := packageRecoveryRequired("package safety check could not read the tenant schema")
		return nil, &refusal
	}
	blockers, err := packageUpgradeBlockers(ctx, live, countTableRows(tenant.conn, tenant.dialect), current, incoming)
	if err != nil {
		refusal := packageRecoveryRequired("package safety check could not count the tenant's records")
		return nil, &refusal
	}
	if len(blockers) > 0 {
		refusal := failed(contract.CodeConflict, truncateUTF8("package would invalidate existing records: "+strings.Join(blockers, "; ")+". Publish those fields as optional or with a default.", 480))
		return nil, &refusal
	}
	if refusal, ok := refuseInFlightReplacement(ctx, tenant, replacedModuleIDs(current.Modules, target.Modules)); !ok {
		return nil, refusal
	}
	return target, nil
}

// moduleInFlightError refuses a replacement while the replaced module still has
// published operations in progress. Its receipts are pinned to the old binding.
type moduleInFlightError struct{ module contract.ModuleID }

func (e moduleInFlightError) Error() string {
	return fmt.Sprintf("module %q has operations in progress; retry the upgrade when they finish", e.module)
}

// checkReplacedModulesQuiet returns moduleInFlightError for the first replaced
// module, in ID order, that has an operation in progress.
func checkReplacedModulesQuiet(replaced map[contract.ModuleID]bool, inFlight map[contract.ModuleID]int) error {
	for _, id := range slices.Sorted(maps.Keys(replaced)) {
		if inFlight[id] > 0 {
			return moduleInFlightError{module: id}
		}
	}
	return nil
}

// refuseReplacedModulesInFlight reads the tenant's in-flight operations and checks
// them against the modules the activation replaces. It runs in the activation
// transaction as well, so a refusal there is final and nothing is applied.
func refuseReplacedModulesInFlight(ctx context.Context, q configstore.ReceiptQuerier, dialect db.QueryDialect, tenant contract.TenantID, replaced map[contract.ModuleID]bool) error {
	if len(replaced) == 0 {
		return nil
	}
	inFlight, err := configstore.OperationOwnersInFlight(ctx, q, dialect, tenant)
	if err != nil {
		return err
	}
	return checkReplacedModulesQuiet(replaced, inFlight)
}

// refuseInFlightReplacement is the pre-journal check. It creates the receipt
// table first, outside any transaction, because DDL inside one may commit early.
func refuseInFlightReplacement(ctx context.Context, tenant packageTenantDB, replaced map[contract.ModuleID]bool) (*management.FinishResult, bool) {
	if len(replaced) == 0 {
		return nil, true
	}
	if err := configstore.NewModuleStore(tenant.conn, tenant.dialect).EnsurePublishedOperationReceiptSchema(ctx); err != nil {
		refusal := packageRecoveryRequired("package safety check could not prepare the operation receipt store")
		return &refusal, false
	}
	err := refuseReplacedModulesInFlight(ctx, tenant.conn, tenant.dialect, tenant.tenant, replaced)
	if err == nil {
		return nil, true
	}
	if refusal, ok := inFlightFailure(err); ok {
		return &refusal, false
	}
	refusal := packageRecoveryRequired("package safety check could not read operations in progress")
	return &refusal, false
}

// inFlightFailure classifies moduleInFlightError as a final refusal.
func inFlightFailure(err error) (management.FinishResult, bool) {
	var inFlight moduleInFlightError
	if !errors.As(err, &inFlight) {
		return management.FinishResult{}, false
	}
	return failed(contract.CodeConflict, truncateUTF8(inFlight.Error(), 480)), true
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
