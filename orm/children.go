package orm

import (
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func (tx *TxManager) insertChild(parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, doc *doctype.Document, parentName string, idx int) error {
	return insertChildExec(tx.DB, parentDT, parentField, childDT, doc, parentName, idx, tx.Dialect)
}

// insertChildExec inserts a child row using the given executor (DB or Tx).
func insertChildExec(ex db.Queryer, parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, doc *doctype.Document, parentName string, idx int, dialect db.QueryDialect) error {
	if doc.Name == "" {
		// Generate unique child row name using ULID — no database query needed.
		prefix := derivePrefix(childDT.Name)
		doc.Name = fmt.Sprintf("%s-%s", prefix, ulid.Make().String())
	}

	now := time.Now()

	var columns []string
	var placeholders []string
	var values []any

	columns = append(columns, "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx")
	placeholders = append(placeholders, "?", "?", "?", "?", "?", "?", "?")
	values = append(values, doc.Name, "", now, now, "", 0, idx)

	columns = append(columns, "parent", "parentfield", "parenttype")
	placeholders = append(placeholders, "?", "?", "?")
	values = append(values, parentName, parentField, parentDT.Name)

	for _, f := range childDT.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		columns = append(columns, f.Fieldname)
		placeholders = append(placeholders, "?")
		val := doc.Get(f.Fieldname)
		if val == nil && f.Default != "" {
			val = convertDefault(f.Default, f.Fieldtype)
		}
		values = append(values, val)
	}

	// Build upsert clause (ON DUPLICATE KEY UPDATE for MySQL, ON CONFLICT for SQLite).
	var updateCols []string
	for _, col := range columns {
		if col != "name" {
			updateCols = append(updateCols, col)
		}
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) %s",
		parentDT.ChildTableName(parentField),
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "),
		dialect.UpsertClause([]string{"name"}, updateCols),
	)

	_, err := ex.Exec(query, values...)
	return err
}

// insertChildrenBatch inserts multiple child rows in a single multi-row INSERT statement.
// Chunked at 100 rows to stay safely within MySQL's max_allowed_packet.
func insertChildrenBatch(ex db.Queryer, parentDT *doctype.DocType, parentField string, childDT *doctype.DocType, children []*doctype.Document, parentName string, dialect db.QueryDialect) error {
	if len(children) == 0 {
		return nil
	}

	childTableName := parentDT.ChildTableName(parentField)
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

			for _, f := range childDT.DataFields() {
				if f.Fieldtype == "Table" {
					continue
				}
				val := child.Get(f.Fieldname)
				if val == nil && f.Default != "" {
					val = convertDefault(f.Default, f.Fieldtype)
				}
				values = append(values, val)
			}
		}

		query := fmt.Sprintf(
			"INSERT INTO %s (%s) VALUES %s %s",
			childTableName,
			strings.Join(columns, ", "),
			strings.Join(placeholders, ", "),
			dialect.UpsertClause([]string{"name"}, updateCols),
		)

		if _, err := ex.Exec(query, values...); err != nil {
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
	childTableName := parentDT.ChildTableName(parentField)

	oldByName := make(map[string]*doctype.Document)
	for _, c := range oldChildren {
		oldByName[c.Name] = c
	}
	newByName := make(map[string]*doctype.Document)
	for _, c := range newChildren {
		if c.Name != "" {
			newByName[c.Name] = c
		}
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
		if _, err := ex.Exec(query, args...); err != nil {
			return fmt.Errorf("deleting removed child rows: %w", err)
		}
	}

	// INSERT new rows (in new but not in old).
	var toInsert []*doctype.Document
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
		if err := updateChildRow(ex, childTableName, childDT, newChild); err != nil {
			return fmt.Errorf("updating child row %s: %w", name, err)
		}
	}

	return nil
}

// updateChildRow issues an UPDATE for a single child row, setting all data columns.
func updateChildRow(ex db.Queryer, tableName string, childDT *doctype.DocType, doc *doctype.Document) error {
	var setClauses []string
	var values []any

	for _, f := range childDT.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = ?", f.Fieldname))
		values = append(values, doc.Get(f.Fieldname))
	}

	if len(setClauses) == 0 {
		return nil
	}

	setClauses = append(setClauses, "modified = ?")
	values = append(values, time.Now())

	values = append(values, doc.Name)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE name = ?",
		tableName, strings.Join(setClauses, ", "))

	_, err := ex.Exec(query, values...)
	return err
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

	rows, err := tx.DB.Query(
		fmt.Sprintf("SELECT %s FROM %s WHERE parent = ? ORDER BY idx", strings.Join(cols, ", "), tableName),
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
				child.Fields[col] = byteSliceToString(val)
			}
		}

		children = append(children, child)
	}

	return children, rows.Err()
}
