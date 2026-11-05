package orm

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

// FirstMatchingNameLocked returns the first record name matching filters within
// scope. It is a locking read (FOR SHARE), so inside a repeatable-read
// transaction it still sees rows committed after the transaction's snapshot.
// Kernel preconditions rely on that to stay correct under concurrency.
func (tx *TxManager) FirstMatchingNameLocked(dt *doctype.DocType, filters string, scope policy.RowScope) (string, bool, error) {
	where := "1=1"
	var args []any
	if filters != "" && filters != "[]" {
		fs, err := ParseFilters(filters)
		if err != nil {
			return "", false, fmt.Errorf("parsing filters: %w", err)
		}
		if err := fs.ValidateFields(dt); err != nil {
			return "", false, fmt.Errorf("invalid filter: %w", err)
		}
		if where, args, err = fs.ToSQL(); err != nil {
			return "", false, fmt.Errorf("building filter SQL: %w", err)
		}
	}
	scopedWhere, scopedArgs, err := tx.rowScopePredicate(dt, scope)
	if err != nil {
		return "", false, err
	}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		args = append(args, scope.UserID)
	} else if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		args = append(args, scopedArgs...)
	}
	query := fmt.Sprintf("SELECT name FROM %s WHERE %s ORDER BY name LIMIT 1", tx.tableName(dt), where)
	if named, ok := tx.Dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR SHARE"
	}
	var name string
	err = tx.queryer().QueryRowContext(tx.queryContext(), db.Rebind(tx.Dialect, query), args...).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
