package orm

import (
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
	names, err := tx.MatchingNamesLocked(dt, filters, scope, 1)
	if err != nil || len(names) == 0 {
		return "", false, err
	}
	return names[0], true, nil
}

// MatchingNamesLocked returns up to limit record names matching filters within
// scope, ordered by name, with the same locking read as FirstMatchingNameLocked.
func (tx *TxManager) MatchingNamesLocked(dt *doctype.DocType, filters string, scope policy.RowScope, limit int) ([]string, error) {
	where := "1=1"
	var args []any
	if filters != "" && filters != "[]" {
		fs, err := ParseFilters(filters)
		if err != nil {
			return nil, fmt.Errorf("parsing filters: %w", err)
		}
		if err := fs.ValidateFields(dt); err != nil {
			return nil, fmt.Errorf("invalid filter: %w", err)
		}
		if where, args, err = fs.ToSQL(); err != nil {
			return nil, fmt.Errorf("building filter SQL: %w", err)
		}
	}
	scopedWhere, scopedArgs, err := tx.rowScopePredicate(dt, scope)
	if err != nil {
		return nil, err
	}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		args = append(args, scope.UserID)
	} else if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		args = append(args, scopedArgs...)
	}
	query := fmt.Sprintf("SELECT name FROM %s WHERE %s ORDER BY name LIMIT %d", tx.tableName(dt), where, limit)
	if named, ok := tx.Dialect.(interface{ DriverName() string }); !ok || named.DriverName() != "libsql" {
		query += " FOR SHARE"
	}
	rows, err := tx.queryer().QueryContext(tx.queryContext(), db.Rebind(tx.Dialect, query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
