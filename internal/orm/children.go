package orm

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/oklog/ulid/v2"
)

// insertChildRows resolves the child doctype of table field f, applies the
// tenant's "today" defaults to its rows, and inserts them.
func (tx *TxManager) insertChildRows(dbTx *sql.Tx, parent *doctype.DocType, f doctype.Field, children []*doctype.Document, parentName string) error {
	childDT := tx.Registry.Get(f.Options)
	if childDT == nil {
		return fmt.Errorf("child doctype %q not found", f.Options)
	}
	tx.applyChildTodayDefaults(childDT, children)
	return insertChildrenBatch(dbTx, parent, f.Fieldname, childDT, children, parentName, tx.Dialect)
}

// applyChildDefaults sets each field of a new child row that lacks a value to its
// default. The insert applies every default, clock ones included. Derivation runs
// before the insert, so it passes clock=false: a "today" Date stays unset there
// and applyChildTodayDefaults fills it with the tenant's business day at insert.
func applyChildDefaults(childDT *doctype.DocType, child *doctype.Document, clock bool) {
	for _, f := range childDT.DataFields() {
		if f.Fieldtype == "Table" || f.Default == "" || child.Get(f.Fieldname) != nil {
			continue
		}
		if !clock && clockDefault(f.Default) {
			continue
		}
		child.Set(f.Fieldname, convertDefault(f.Default, f.Fieldtype, ""))
	}
}

// clockDefault reports a default that reads the clock.
func clockDefault(def string) bool {
	switch strings.ToLower(strings.TrimSpace(def)) {
	case "today", "now":
		return true
	}
	return false
}

// applyChildTodayDefaults sets a Date field defaulting to "today" on each child
// row that lacks a value, to the tenant's business day. It runs before
// insertChildrenBatch, which has no TxManager to resolve the clock.
func (tx *TxManager) applyChildTodayDefaults(childDT *doctype.DocType, children []*doctype.Document) {
	for _, f := range childDT.DataFields() {
		today := tx.todayDefault(f.Fieldtype, f.Default)
		if today == "" {
			continue
		}
		for _, child := range children {
			if child.Get(f.Fieldname) == nil {
				child.Set(f.Fieldname, today)
			}
		}
	}
}

// insertChildrenBatch inserts multiple child rows in a single multi-row INSERT statement.
// Chunked at 100 rows to stay safely within MySQL's max_allowed_packet.
func insertChildrenBatch(ex db.Queryer, parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, children []*doctype.Document, parentName string, dialect db.QueryDialect) error {
	if len(children) == 0 {
		return nil
	}

	childTableName := quoteTableName(dialect, parentDT.RawChildTableName(parentField))
	prefix := derivePrefix(childDT.Name)

	// Build column list once (same for all rows).
	var columns []string
	columns = append(columns, "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx")
	columns = append(columns, "parent", "parentfield", "parenttype")
	for _, f := range childDT.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		columns = append(columns, f.Fieldname)
	}

	// Build list of update columns (non-key, non-parent) for upsert clause.
	var updateCols []string
	for _, col := range columns {
		if col != "name" && col != "parent" && col != "parentfield" && col != "parenttype" {
			updateCols = append(updateCols, col)
		}
	}

	// Insert in chunks of 100.
	const chunkSize = 100
	for start := 0; start < len(children); start += chunkSize {
		end := min(start+chunkSize, len(children))
		chunk := children[start:end]

		var placeholders []string
		var values []any
		now := time.Now()

		for i, child := range chunk {
			idx := start + i
			if child.Name == "" {
				child.Name = fmt.Sprintf("%s-%s", prefix, ulid.Make().String())
			}

			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?"+strings.Repeat(", ?", len(columns)-10)+")")

			values = append(values,
				child.Name, "", now, now, "", 0, idx, // name, owner, creation, modified, modified_by, doc_status, idx
				parentName, parentField, parentDT.Name, // parent, parentfield, parenttype
			)

			applyChildDefaults(childDT, child, true)
			for _, f := range childDT.DataFields() {
				if f.Fieldtype == "Table" {
					continue
				}
				val, err := canonicalizeDecimal(child, f, child.Get(f.Fieldname))
				if err != nil {
					return err
				}
				values = append(values, blankAsNull(f, val))
			}
		}

		query := fmt.Sprintf(
			"INSERT INTO %s (%s) VALUES %s %s",
			childTableName,
			strings.Join(columns, ", "),
			strings.Join(placeholders, ", "),
			dialect.UpsertClause([]string{"name"}, updateCols),
		)

		if _, err := ex.Exec(db.Rebind(dialect, query), values...); err != nil {
			return fmt.Errorf("batch inserting child rows [%d-%d]: %w", start, end, err)
		}
	}

	return nil
}

// reconcileChildren performs three-way reconciliation for child table rows.
// It compares old and new child rows and only issues necessary DB operations:
//   - DELETE rows present in old but missing in new
//   - INSERT rows present in new but missing in old
//   - UPDATE rows present in both with changed data
func reconcileChildren(ex db.Queryer, parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, oldChildren, newChildren []*doctype.Document, parentName string, dialect db.QueryDialect) error {
	childTableName := quoteTableName(dialect, parentDT.RawChildTableName(parentField))

	oldByName := make(map[string]*doctype.Document)
	for _, c := range oldChildren {
		oldByName[c.Name] = c
	}
	newByName := make(map[string]*doctype.Document)
	var unnamedNewChildren []*doctype.Document
	for _, c := range newChildren {
		if c.Name == "" {
			unnamedNewChildren = append(unnamedNewChildren, c)
			continue
		}
		newByName[c.Name] = c
	}

	// DELETE rows that were removed (in old but not in new).
	var toDelete []string
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			toDelete = append(toDelete, name)
		}
	}
	if len(toDelete) > 0 {
		placeholders := make([]string, len(toDelete))
		args := make([]any, len(toDelete))
		for i, name := range toDelete {
			placeholders[i] = "?"
			args[i] = name
		}
		query := fmt.Sprintf("DELETE FROM %s WHERE name IN (%s)",
			childTableName, strings.Join(placeholders, ", "))
		if _, err := ex.Exec(db.Rebind(dialect, query), args...); err != nil {
			return fmt.Errorf("deleting removed child rows: %w", err)
		}
	}

	// INSERT new rows (in new but not in old).
	toInsert := append([]*doctype.Document(nil), unnamedNewChildren...)
	for name, child := range newByName {
		if _, ok := oldByName[name]; !ok {
			toInsert = append(toInsert, child)
		}
	}
	if len(toInsert) > 0 {
		if err := insertChildrenBatch(ex, parentDT, parentField, childDT, toInsert, parentName, dialect); err != nil {
			return fmt.Errorf("inserting new child rows: %w", err)
		}
	}

	// UPDATE rows that exist in both but have changed.
	for name, newChild := range newByName {
		oldChild, ok := oldByName[name]
		if !ok {
			continue
		}
		if documentsEqual(oldChild, newChild, childDT) {
			continue
		}
		if err := updateChildRow(ex, childTableName, childDT, newChild, dialect); err != nil {
			return fmt.Errorf("updating child row %s: %w", name, err)
		}
	}

	return nil
}

// insertFreshChildRows inserts the rows of a new parent. It has no rows yet, so any
// name a row carries cannot be honoured: each row gets a new name.
func (tx *TxManager) insertFreshChildRows(dbTx *sql.Tx, parent *doctype.DocType, f doctype.Field, children []*doctype.Document, parentName string) error {
	for _, child := range children {
		child.Name = ""
	}
	return tx.insertChildRows(dbTx, parent, f, children, parentName)
}

// saveChildTable saves the rows of one table field of doc. Without an old document the
// rows are replaced wholesale; otherwise they are reconciled against the old rows.
func (tx *TxManager) saveChildTable(dbTx *sql.Tx, dt *doctype.DocType, f doctype.Field, doc, oldDoc *doctype.Document) error {
	childTableName := tx.childTableName(dt, f.Fieldname)
	newChildren := doc.GetTable(f.Fieldname)
	childDT := tx.Registry.Get(f.Options)
	if childDT == nil {
		if newChildren != nil {
			return fmt.Errorf("child doctype %q not found", f.Options)
		}
		return nil
	}
	if err := tx.checkChildRowNames(dbTx, dt, f.Fieldname, doc.Name, newChildren); err != nil {
		return err
	}
	var oldChildren []*doctype.Document
	if oldDoc != nil {
		oldChildren = oldDoc.GetTable(f.Fieldname)
	}
	if oldDoc == nil {
		// Fallback: no old document available — DELETE-all + re-INSERT-all.
		if _, err := dbTx.Exec(db.Rebind(tx.Dialect,
			fmt.Sprintf("DELETE FROM %s WHERE parent = ?", childTableName)),
			doc.Name,
		); err != nil {
			return fmt.Errorf("deleting old child rows for %s: %w", f.Fieldname, err)
		}
		if newChildren == nil {
			return nil
		}
		if err := tx.insertChildRows(dbTx, dt, f, newChildren, doc.Name); err != nil {
			return fmt.Errorf("inserting child rows in %s: %w", f.Fieldname, err)
		}
		return nil
	}
	// Diff-based reconciliation: only DELETE removed, INSERT new, UPDATE changed.
	if err := reconcileChildren(dbTx, dt, f.Fieldname, childDT, oldChildren, newChildren, doc.Name, tx.Dialect); err != nil {
		return fmt.Errorf("reconciling child rows in %s: %w", f.Fieldname, err)
	}
	return nil
}

// checkChildRowNames refuses a named row that is not already a row of this
// parent's table field. Honouring such a name would upsert another parent's
// row through insertChildrenBatch, which does not touch the parent columns.
func (tx *TxManager) checkChildRowNames(dbTx *sql.Tx, parentDT *doctype.DocType, parentField, parentName string, rows []*doctype.Document) error {
	var named []string
	for _, row := range rows {
		if row.Name != "" {
			named = append(named, row.Name)
		}
	}
	if len(named) == 0 {
		return nil
	}

	existing, err := dbTx.QueryContext(tx.queryContext(), db.Rebind(tx.Dialect,
		fmt.Sprintf("SELECT name FROM %s WHERE parent = ? AND parentfield = ?", tx.childTableName(parentDT, parentField))),
		parentName, parentField,
	)
	if err != nil {
		return fmt.Errorf("reading child row names: %w", err)
	}
	defer existing.Close()

	current := make(map[string]bool)
	for existing.Next() {
		var name string
		if err := existing.Scan(&name); err != nil {
			return fmt.Errorf("reading child row names: %w", err)
		}
		current[name] = true
	}
	if err := existing.Err(); err != nil {
		return fmt.Errorf("reading child row names: %w", err)
	}
	for _, name := range named {
		if !current[name] {
			msg := fmt.Sprintf("Row %s is not part of %s %s.", name, parentDT.Name, parentName)
			return fmt.Errorf("%w: %w", ErrValidation, &doctype.ValidationError{Type: "ChildRow", Message: msg, DocType: parentDT.Name, Field: parentField})
		}
	}
	return nil
}

// updateChildRow issues an UPDATE for a single child row, setting all data columns.
func updateChildRow(ex db.Queryer, tableName string, childDT *doctype.DocType, doc *doctype.Document, dialect db.QueryDialect) error {
	var setClauses []string
	var values []any

	for _, f := range childDT.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		val, err := canonicalizeDecimal(doc, f, doc.Get(f.Fieldname))
		if err != nil {
			return err
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = ?", f.Fieldname))
		values = append(values, blankAsNull(f, val))
	}

	if len(setClauses) == 0 {
		return nil
	}

	setClauses = append(setClauses, "modified = ?")
	values = append(values, time.Now())

	values = append(values, doc.Name)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE name = ?",
		tableName, strings.Join(setClauses, ", "))

	_, err := ex.Exec(db.Rebind(dialect, query), values...)
	return err
}

// persistComputedChildFields writes computed child values back to the parent-owned
// child table after ComputeFields has populated them in memory.
func persistComputedChildFields(ex db.Queryer, parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, children []*doctype.Document, dialect db.QueryDialect) error {
	var computedFields []doctype.Field
	for _, f := range childDT.DataFields() {
		if f.Fieldtype != "Table" && f.Computed != "" {
			computedFields = append(computedFields, f)
		}
	}
	if len(computedFields) == 0 {
		return nil
	}

	tableName := quoteTableName(dialect, parentDT.RawChildTableName(parentField))
	for _, child := range children {
		if child == nil || child.Name == "" {
			continue
		}

		setClauses := make([]string, 0, len(computedFields)+1)
		values := make([]any, 0, len(computedFields)+2)
		for _, f := range computedFields {
			val, err := canonicalizeDecimal(child, f, child.Get(f.Fieldname))
			if err != nil {
				return err
			}
			setClauses = append(setClauses, fmt.Sprintf("%s = ?", f.Fieldname))
			values = append(values, val)
		}
		setClauses = append(setClauses, "modified = ?")
		values = append(values, time.Now(), child.Name)

		query := fmt.Sprintf("UPDATE %s SET %s WHERE name = ?", tableName, strings.Join(setClauses, ", "))
		if _, err := ex.Exec(db.Rebind(dialect, query), values...); err != nil {
			return fmt.Errorf("updating computed child row %s: %w", child.Name, err)
		}
	}

	return nil
}

// documentsEqual compares two documents' data fields for equality (ignoring system columns).
func documentsEqual(a, b *doctype.Document, dt *doctype.DocType) bool {
	if a == nil || b == nil {
		return a == b
	}
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		if a.Get(f.Fieldname) != b.Get(f.Fieldname) {
			return false
		}
	}
	return true
}

// getChildRows fetches child rows for a parent from the given child table.
func (tx *TxManager) getChildRows(tableName string, childDT *doctype.DocType, parentName string) ([]*doctype.Document, error) {
	dataFields := childDT.DataFields()

	var cols []string
	for _, f := range dataFields {
		if f.Fieldtype == "Table" {
			continue
		}
		cols = append(cols, f.Fieldname)
	}
	cols = append(cols, "name", "idx", "parent", "parentfield", "parenttype")

	rows, err := tx.queryer().QueryContext(tx.queryContext(), db.Rebind(tx.Dialect,
		fmt.Sprintf("SELECT %s FROM %s WHERE parent = ? ORDER BY idx", strings.Join(cols, ", "), tableName)),
		parentName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var children []*doctype.Document
	for rows.Next() {
		scanTargets := make([]any, len(cols))
		for i := range cols {
			var v any
			scanTargets[i] = &v
		}

		if err := rows.Scan(scanTargets...); err != nil {
			return nil, err
		}

		child := doctype.NewDocument(childDT.Name)
		child.IsNew = false

		for i, col := range cols {
			val := *(scanTargets[i].(*any))
			switch col {
			case "name":
				child.Name = stringVal(val)
			default:
				child.Fields[col] = normalizeScannedFieldValue(childDT.GetField(col), val)
			}
		}

		children = append(children, child)
	}

	return children, rows.Err()
}
