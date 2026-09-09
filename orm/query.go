// Package orm provides the generic ORM layer for Kora.
// All documents are represented as doctype.Document (map[string]any).
package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/script"
	"github.com/go-sql-driver/mysql"
)

// generateName creates a unique document name based on the DocType.
// Format: {PREFIX}-{NNNN} where PREFIX is derived from the DocType name.
func generateName(dt *doctype.DocType, existingCount int) string {
	prefix := derivePrefix(dt.Name)
	return fmt.Sprintf("%s-%04d", prefix, existingCount+1)
}

func derivePrefix(name string) string {
	// For multi-word names, take the first letter of each word.
	// For single-word names, take the first 4 letters.
	// Examples: "Customer" → "CUST", "Work Order" → "WO", "Work Order Item" → "WOI"
	parts := strings.Fields(name)
	if len(parts) == 1 {
		s := strings.ToUpper(name)
		if len(s) > 4 {
			s = s[:4]
		}
		return s
	}
	var result strings.Builder
	for _, p := range parts {
		if len(p) > 0 {
			c := p[0]
			if c >= 'a' && c <= 'z' {
				c = c - 32
			}
			result.WriteByte(c)
		}
	}
	return result.String()
}

// TxManager provides transactional operations on documents.
type TxManager struct {
	DB       *sql.DB
	Registry *doctype.Registry
	Dialect  db.Dialect

	// Context carries the request-scoped context for script execution and
	// other cancellable operations. If nil, context.Background() is used.
	Context context.Context

	// EventBus receives change events after successful writes.
	// If nil, analytics event emission is disabled (no-op).
	EventBus analytics.EventBus

	// Outbox writes events into the transactional outbox in the same transaction
	// as the business write (RFC §8.1). If nil, outbox recording is disabled and
	// the existing post-commit EventBus.Publish path remains the only emission.
	Outbox outbox.Writer

	// ScriptRunner executes JavaScript hooks (before_save, after_insert, etc.).
	// If nil, script execution is disabled.
	ScriptRunner script.Runner

	// ScriptStore persists script definitions and execution logs.
	// If nil, script loading and logging is disabled.
	ScriptStore *script.Store

	// ScriptProvider bridges scripts to the engine (ORM, secrets, HTTP).
	// Created per-request by siteTx() with the site's DB and registry.
	ScriptProvider script.KoraProvider

	// SiteName is the tenant identifier used in analytics events.
	SiteName string

	// AsyncHookSink receives after_* hooks for fire-and-forget execution.
	// If non-nil, after_* events are handed to the async worker contract.
	AsyncHookSink AsyncHookSink

	// User and UserRole from the current request context, used for script execution.
	CurrentUser     string
	CurrentUserRole string
}

// writeOutbox records a change event into the transactional outbox within the
// given transaction. It is a no-op when tx.Outbox is nil, preserving the existing
// post-commit EventBus.Publish behavior.
func (tx *TxManager) writeOutbox(dbTx *sql.Tx, op analytics.EventOp, dt *doctype.DocType, docName, modifiedBy string, data, oldData map[string]any) error {
	if tx.Outbox == nil {
		return nil
	}
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	change := analytics.ChangeEvent{
		Site:       tx.SiteName,
		Doctype:    dt.Name,
		DocName:    docName,
		Operation:  op,
		Timestamp:  time.Now(),
		ModifiedBy: modifiedBy,
		Data:       data,
		OldData:    oldData,
	}
	return tx.Outbox.Append(ctx, dbTx, outbox.ChangeEventToEnvelope(change))
}

// Insert creates a new document in the database.
// modifiedBy is stored in the modified_by column — use the actor responsible (e.g., user or "ai-assistant").
func (tx *TxManager) Insert(dt *doctype.DocType, doc *doctype.Document, owner, modifiedBy string) error {
	if !doc.IsNew {
		return fmt.Errorf("cannot insert an existing document")
	}

	// Run before_insert + before_save hooks — scripts can modify doc or reject.
	if err := tx.runHooks(dt, script.EventBeforeInsert, doc, nil); err != nil {
		return err
	}
	if err := tx.runHooks(dt, script.EventBeforeSave, doc, nil); err != nil {
		return err
	}

	dbTx, err := tx.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer dbTx.Rollback()

	if err := tx.InsertInTx(dbTx, dt, doc, owner, modifiedBy); err != nil {
		return err
	}

	if err := dbTx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	doc.IsNew = false

	if tx.EventBus != nil {
		tx.EventBus.Publish(analytics.ChangeEvent{
			Site:       tx.SiteName,
			Doctype:    dt.Name,
			DocName:    doc.Name,
			Operation:  analytics.EventInsert,
			Timestamp:  time.Now(),
			ModifiedBy: modifiedBy,
			Data:       copyFieldsWithStatus(doc.Fields, doc.DocStatus),
		})
	}

	// Run after_insert + after_save hooks — best-effort, errors logged not returned.
	_ = tx.runHooks(dt, script.EventAfterInsert, doc, nil)
	_ = tx.runHooks(dt, script.EventAfterSave, doc, nil)

	return nil
}

// InsertInTx performs the transactional portion of Insert using an existing
// SQL transaction without committing it. Callers that must commit additional
// rows atomically with the business write (operation kernel idempotency
// receipts, audit rows, extra outbox events) use this variant and own the
// surrounding transaction's Commit/Rollback.
func (tx *TxManager) InsertInTx(dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document, owner, modifiedBy string) error {
	nextNum := 1
	if doc.Name == "" {
		prefix := derivePrefix(dt.Name)
		allocated, err := tx.Dialect.AllocateNameNumber(dbTx, dt.Name, prefix, dt.RawTableName())
		if err != nil {
			return err
		}
		nextNum = int(allocated)
		doc.Name = fmt.Sprintf("%s-%04d", prefix, nextNum)
	}

	now := time.Now()
	doc.DocStatus = 0

	dataFields := dt.NonTableDataFields()
	var columns []string
	var placeholders []string
	var values []any

	columns = append(columns, "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx")
	placeholders = append(placeholders, "?", "?", "?", "?", "?", "?", "?")
	values = append(values, doc.Name, owner, now, now, modifiedBy, doc.DocStatus, nextNum)

	for _, f := range dataFields {
		columns = append(columns, f.Fieldname)
		placeholders = append(placeholders, "?")

		val := doc.Get(f.Fieldname)
		if val == nil && f.Default != "" {
			val = convertDefault(f.Default, f.Fieldtype)
			doc.Set(f.Fieldname, val)
		}
		// Normalize JSON fields: arrays/objects marshalled to string for SQL binding.
		if normalized, err := normalizeSQLValue(f, val); err != nil {
			return fmt.Errorf("field %s: %w", f.Fieldname, err)
		} else {
			values = append(values, normalized)
		}
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		dt.TableName(),
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "),
	)

	if _, err := dbTx.Exec(query, values...); err != nil {
		if valErr := tx.Dialect.ParseError(err, dt); valErr != nil {
			if valErr.Type == "UniqueConstraint" {
				return fmt.Errorf("%w: %w", ErrDuplicate, valErr)
			}
			return fmt.Errorf("%w: %w", ErrValidation, valErr)
		}
		return fmt.Errorf("inserting document: %w", err)
	}

	for _, f := range dt.TableFields() {
		children := doc.GetTable(f.Fieldname)
		if children == nil {
			continue
		}
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			return fmt.Errorf("child doctype %q not found", f.Options)
		}
		if err := insertChildrenBatch(dbTx, dt, f.Fieldname, childDT, children, doc.Name, tx.Dialect); err != nil {
			return fmt.Errorf("inserting child rows in %s: %w", f.Fieldname, err)
		}
	}

	// Set up script-based computed fields.
	tx.setupComputedHook()
	defer doctype.SetComputedScriptHook(nil)

	// Evaluate computed fields on child items (e.g., line_total = quantity * unit_price).
	for _, f := range dt.TableFields() {
		children := doc.GetTable(f.Fieldname)
		if children == nil {
			continue
		}
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		for _, child := range children {
			if err := tx.populateLinkedFields(dbTx, childDT, child); err != nil {
				return fmt.Errorf("populating linked fields in %s: %w", f.Fieldname, err)
			}
			if err := doctype.ComputeFields(childDT, child); err != nil {
				slog.Warn("computed fields failed on child", "doctype", childDT.Name, "error", err)
			}
		}
		if err := persistComputedChildFields(dbTx, dt, f.Fieldname, childDT, children); err != nil {
			return fmt.Errorf("persisting computed child fields in %s: %w", f.Fieldname, err)
		}
	}

	// Resolve links before computed expressions consume their values.
	if err := tx.populateLinkedFields(dbTx, dt, doc); err != nil {
		return fmt.Errorf("populating linked fields: %w", err)
	}
	// Evaluate computed fields on the parent document (e.g., subtotal = SUM(items.line_total)).
	if err := doctype.ComputeFields(dt, doc); err != nil {
		slog.Warn("computed fields failed", "doctype", dt.Name, "error", err)
	}

	// Persist linked and computed field values via UPDATE.
	if err := updateDerivedFieldsExec(dbTx, dt, doc); err != nil {
		return fmt.Errorf("persisting derived fields: %w", err)
	}

	if err := tx.writeOutbox(dbTx, analytics.EventInsert, dt, doc.Name, modifiedBy, copyFieldsWithStatus(doc.Fields, doc.DocStatus), nil); err != nil {
		return fmt.Errorf("recording insert to outbox: %w", err)
	}

	return nil
}

// updateComputedFields UPDATEs only computed field columns on the document.
func (tx *TxManager) updateComputedFields(dt *doctype.DocType, doc *doctype.Document) error {
	return updateComputedFieldsExec(tx.DB, dt, doc)
}

// populateLinkedFields resolves Link values to their configured linked_field
// values before computed fields run. Link inputs may use either the document
// name or its title field, which keeps API payloads human-friendly.
func (tx *TxManager) populateLinkedFields(ex db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	if dt == nil || doc == nil {
		return nil
	}
	for _, field := range dt.Fields {
		if field.LinkedField == "" || field.Fieldtype == "Table" {
			continue
		}
		sourceField, targetField, ok := strings.Cut(field.LinkedField, ".")
		if !ok || sourceField == "" || targetField == "" {
			continue
		}
		linkValue := strings.TrimSpace(fmt.Sprint(doc.Get(sourceField)))
		if linkValue == "" || linkValue == "<nil>" {
			continue
		}
		source := dt.GetField(sourceField)
		if source == nil || source.Fieldtype != "Link" {
			continue
		}
		targetDT := tx.Registry.Get(source.Options)
		if targetDT == nil {
			continue
		}
		where := "name = ?"
		args := []any{linkValue}
		if targetDT.TitleField != "" && targetDT.GetField(targetDT.TitleField) != nil {
			where += " OR " + targetDT.TitleField + " = ?"
			args = append(args, linkValue)
		}
		targetMeta := targetDT.GetField(targetField)
		if targetMeta == nil {
			continue
		}
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s LIMIT 1", targetField, targetDT.TableName(), where)
		var value any
		if err := ex.QueryRow(query, args...).Scan(&value); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if bytes, ok := value.([]byte); ok {
			textValue := string(bytes)
			switch targetMeta.Fieldtype {
			case "Int", "Float", "Currency", "Percent":
				if number, err := strconv.ParseFloat(textValue, 64); err == nil {
					value = number
				} else {
					value = textValue
				}
			default:
				value = textValue
			}
		}
		if value != nil {
			doc.Set(field.Fieldname, value)
		}
	}
	return nil
}

// updateComputedFieldsExec UPDATEs computed fields using the given executor (DB or Tx).
func updateComputedFieldsExec(ex db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	return updateDerivedFieldsExec(ex, dt, doc)
}

// updateDerivedFieldsExec persists both linked and computed values produced by
// the document lifecycle. It intentionally excludes ordinary user fields.
func updateDerivedFieldsExec(ex db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	var setClauses []string
	var values []any

	for _, f := range dt.Fields {
		if (f.Computed == "" && f.LinkedField == "") || f.Fieldtype == "Table" {
			continue
		}
		val := doc.Get(f.Fieldname)
		if val != nil {
			setClauses = append(setClauses, fmt.Sprintf("%s = ?", f.Fieldname))
			values = append(values, val)
		}
	}

	if len(setClauses) == 0 {
		return nil
	}

	values = append(values, doc.Name)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE name = ?",
		dt.TableName(),
		strings.Join(setClauses, ", "),
	)

	_, err := ex.Exec(query, values...)
	return err
}

// Save updates an existing document.
// If owner is non-empty, only updates if the document is owned by that user.
// All operations run in a database transaction to ensure atomicity.
// oldDoc is the document before modifications (from GetDoc); when provided, child table
// reconciliation uses a diff-based approach instead of DELETE-all + re-INSERT-all.
func (tx *TxManager) Save(dt *doctype.DocType, doc *doctype.Document, modifiedBy string, owner string, oldDoc *doctype.Document) error {
	if doc.IsNew {
		return fmt.Errorf("cannot save a new document; use Insert instead")
	}

	// Run before_save hooks — scripts can modify doc or reject.
	if err := tx.runHooks(dt, script.EventBeforeSave, doc, oldDoc); err != nil {
		return err
	}

	// Start a transaction so DELETE + INSERT for child tables is atomic.
	dbTx, err := tx.DB.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer dbTx.Rollback() // no-op after Commit

	if err := tx.SaveInTx(dbTx, dt, doc, modifiedBy, owner, oldDoc); err != nil {
		return err
	}

	if err := dbTx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	if tx.EventBus != nil {
		var oldData map[string]any
		if oldDoc != nil {

			oldData = copyFieldsWithStatus(oldDoc.Fields, oldDoc.DocStatus)
		}
		tx.EventBus.Publish(analytics.ChangeEvent{
			Site:       tx.SiteName,
			Doctype:    dt.Name,
			DocName:    doc.Name,
			Operation:  analytics.EventUpdate,
			Timestamp:  time.Now(),
			ModifiedBy: modifiedBy,
			Data:       copyFieldsWithStatus(doc.Fields, doc.DocStatus),
			OldData:    oldData,
		})
	}

	// Run after_save hooks — best-effort.
	_ = tx.runHooks(dt, script.EventAfterSave, doc, oldDoc)

	return nil
}

// SaveInTx performs the transactional portion of Save using an existing SQL
// transaction without committing it. Operation-kernel callers use this variant
// to commit idempotency receipts and audit rows atomically with the mutation.
// Returns orm.ErrNotFound wrapped error when the row vanished or is owner-hidden.
func (tx *TxManager) SaveInTx(dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document, modifiedBy string, owner string, oldDoc *doctype.Document) error {
	now := time.Now()
	dataFields := dt.NonTableDataFields()

	var setClauses []string
	var values []any

	for _, f := range dataFields {
		// Note: read_only is a UI hint, not an ORM constraint.
		// The workflow handler needs to persist state changes to read_only fields.
		// Direct edits are blocked at the API level (HandleUpdate).
		newVal := doc.Get(f.Fieldname)
		if oldDoc != nil {
			if oldVal := oldDoc.Get(f.Fieldname); oldVal == newVal {
				continue
			}
		}
		// Normalize JSON fields: arrays/objects marshalled to string for SQL binding.
		if normalized, err := normalizeSQLValue(f, newVal); err != nil {
			return fmt.Errorf("field %s: %w", f.Fieldname, err)
		} else {
			setClauses = append(setClauses, fmt.Sprintf("%s = ?", f.Fieldname))
			values = append(values, normalized)
		}
	}

	setClauses = append(setClauses, "modified = ?", "modified_by = ?", "doc_status = ?")
	values = append(values, now, modifiedBy, doc.DocStatus)

	where := "name = ?"
	values = append(values, doc.Name)
	if owner != "" {
		where += " AND owner = ?"
		values = append(values, owner)
	}

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s",
		dt.TableName(),
		strings.Join(setClauses, ", "),
		where,
	)

	result, err := dbTx.Exec(query, values...)
	if err != nil {
		if valErr := tx.Dialect.ParseError(err, dt); valErr != nil {
			if valErr.Type == "UniqueConstraint" {
				return fmt.Errorf("%w: %w", ErrDuplicate, valErr)
			}
			return fmt.Errorf("%w: %w", ErrValidation, valErr)
		}
		return fmt.Errorf("updating document: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("%w: document %q not found or access denied", ErrNotFound, doc.Name)
	}

	for _, f := range dt.TableFields() {
		childTableName := dt.ChildTableName(f.Fieldname)
		newChildren := doc.GetTable(f.Fieldname)
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			if newChildren != nil {
				return fmt.Errorf("child doctype %q not found", f.Options)
			}
			continue
		}

		var oldChildren []*doctype.Document
		if oldDoc != nil {
			oldChildren = oldDoc.GetTable(f.Fieldname)
		}

		if oldDoc == nil {
			// Fallback: no old document available — DELETE-all + re-INSERT-all.
			if _, err := dbTx.Exec(
				fmt.Sprintf("DELETE FROM %s WHERE parent = ?", childTableName),
				doc.Name,
			); err != nil {
				return fmt.Errorf("deleting old child rows for %s: %w", f.Fieldname, err)
			}
			if newChildren == nil {
				continue
			}
			if err := insertChildrenBatch(dbTx, dt, f.Fieldname, childDT, newChildren, doc.Name, tx.Dialect); err != nil {
				return fmt.Errorf("inserting child rows in %s: %w", f.Fieldname, err)
			}
		} else {
			// Diff-based reconciliation: only DELETE removed, INSERT new, UPDATE changed.
			if err := reconcileChildren(dbTx, dt, f.Fieldname, childDT, oldChildren, newChildren, doc.Name, tx.Dialect); err != nil {
				return fmt.Errorf("reconciling child rows in %s: %w", f.Fieldname, err)
			}
		}
	}

	// Set up script-based computed fields.
	tx.setupComputedHook()
	defer doctype.SetComputedScriptHook(nil)

	// Evaluate computed fields on child items first, then parent.
	for _, f := range dt.TableFields() {
		children := doc.GetTable(f.Fieldname)
		if children == nil {
			continue
		}
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		for _, child := range children {
			if err := tx.populateLinkedFields(dbTx, childDT, child); err != nil {
				return fmt.Errorf("populating linked fields in %s: %w", f.Fieldname, err)
			}
			if err := doctype.ComputeFields(childDT, child); err != nil {
				slog.Warn("computed fields failed on child", "doctype", childDT.Name, "error", err)
			}
		}
		if err := persistComputedChildFields(dbTx, dt, f.Fieldname, childDT, children); err != nil {
			return fmt.Errorf("persisting computed child fields in %s: %w", f.Fieldname, err)
		}
	}

	if err := tx.populateLinkedFields(dbTx, dt, doc); err != nil {
		return fmt.Errorf("populating linked fields: %w", err)
	}
	if err := doctype.ComputeFields(dt, doc); err != nil {
		slog.Warn("computed fields failed", "doctype", dt.Name, "error", err)
	}

	if err := updateDerivedFieldsExec(dbTx, dt, doc); err != nil {
		return fmt.Errorf("persisting derived fields: %w", err)
	}

	var oldOutboxData map[string]any
	if oldDoc != nil {
		oldOutboxData = copyFieldsWithStatus(oldDoc.Fields, oldDoc.DocStatus)
	}
	if err := tx.writeOutbox(dbTx, analytics.EventUpdate, dt, doc.Name, modifiedBy, copyFieldsWithStatus(doc.Fields, doc.DocStatus), oldOutboxData); err != nil {
		return fmt.Errorf("recording save to outbox: %w", err)
	}

	return nil
}

// GetDoc loads a single document by name, including child table expansion.
// If owner is non-empty, only returns the document if owned by that user.
func (tx *TxManager) GetDoc(dt *doctype.DocType, name string, owner string) (*doctype.Document, error) {
	dataFields := dt.NonTableDataFields()

	var cols []string
	for _, f := range dataFields {
		cols = append(cols, f.Fieldname)
	}
	cols = append(cols, "name", "owner", "creation", "modified", "modified_by", "doc_status")

	scanTargets := make([]any, len(cols))
	for i := range cols {
		var v any
		scanTargets[i] = &v
	}

	where := "name = ?"
	args := []any{name}
	if owner != "" {
		where += " AND owner = ?"
		args = append(args, owner)
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s",
		strings.Join(cols, ", "),
		dt.TableName(),
		where,
	)

	row := tx.DB.QueryRow(query, args...)
	if err := row.Scan(scanTargets...); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: document %q not found in %s", ErrNotFound, name, dt.Name)
		}
		return nil, fmt.Errorf("scanning document: %w", err)
	}

	doc := doctype.NewDocument(dt.Name)
	doc.Name = name
	doc.IsNew = false

	for i, col := range cols {
		val := *(scanTargets[i].(*any))
		switch col {
		case "name":
			doc.Name = stringVal(val)
		case "doc_status":
			doc.DocStatus = intVal(val)
		case "owner", "creation", "modified", "modified_by":
			// System columns.
		default:
			doc.Fields[col] = byteSliceToString(val)
		}
	}

	for _, f := range dt.TableFields() {
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		children, err := tx.getChildRows(dt.ChildTableName(f.Fieldname), childDT, name)
		if err != nil {
			return nil, fmt.Errorf("loading child table %s: %w", f.Fieldname, err)
		}
		doc.Fields[f.Fieldname] = children
	}

	return doc, nil
}

// GetList returns a paginated list of documents with optional filtering.
// If owner is non-empty, only returns documents owned by that user.
func (tx *TxManager) GetList(dt *doctype.DocType, filters string, orderBy string, limit, offset int, owner string) ([]*doctype.Document, int, error) {
	where := "1=1"
	var whereArgs []any
	if filters != "" && filters != "[]" {
		fs, err := ParseFilters(filters)
		if err != nil {
			return nil, 0, fmt.Errorf("parsing filters: %w", err)
		}
		// Validate filter field names against the DocType's actual fields.
		if err := fs.ValidateFields(dt); err != nil {
			return nil, 0, fmt.Errorf("invalid filter: %w", err)
		}
		where, whereArgs, err = fs.ToSQL()
		if err != nil {
			return nil, 0, fmt.Errorf("building filter SQL: %w", err)
		}
	}
	if owner != "" {
		where += " AND owner = ?"
		whereArgs = append(whereArgs, owner)
	}

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", dt.TableName(), where)
	err := tx.DB.QueryRow(countQuery, whereArgs...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting documents: %w", err)
	}

	dataFields := dt.NonTableDataFields()
	var cols []string
	for _, f := range dataFields {
		cols = append(cols, f.Fieldname)
	}
	cols = append(cols, "name", "owner", "creation", "modified", "modified_by", "doc_status")

	if orderBy == "" {
		orderBy = dt.SortField + " " + dt.SortOrder
	}

	// Validate orderBy against known columns to prevent SQL injection.
	safeOrderBy, err := validateOrderBy(orderBy, dt)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid order_by: %w", err)
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT ? OFFSET ?",
		strings.Join(cols, ", "),
		dt.TableName(),
		where,
		safeOrderBy,
	)

	args := append(whereArgs, limit, offset)

	rows, err := tx.DB.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying documents: %w", err)
	}
	defer rows.Close()

	docs := make([]*doctype.Document, 0, limit)
	for rows.Next() {
		scanTargets := make([]any, len(cols))
		for i := range cols {
			var v any
			scanTargets[i] = &v
		}

		if err := rows.Scan(scanTargets...); err != nil {
			return nil, 0, fmt.Errorf("scanning row: %w", err)
		}

		doc := doctype.NewDocument(dt.Name)
		doc.IsNew = false

		for i, col := range cols {
			val := *(scanTargets[i].(*any))
			switch col {
			case "name":
				doc.Name = stringVal(val)
			case "doc_status":
				doc.DocStatus = intVal(val)
			default:
				doc.Fields[col] = byteSliceToString(val)
			}
		}

		docs = append(docs, doc)
	}

	return docs, total, rows.Err()
}

// Delete removes a document by name.
// If owner is non-empty, only deletes if the document is owned by that user.
func (tx *TxManager) Delete(dt *doctype.DocType, name string, owner string) error {
	// Read the document before deleting — needed for analytics event Data and hooks.
	var oldDoc *doctype.Document
	var oldFields map[string]any
	if tx.EventBus != nil || tx.ScriptRunner != nil || tx.Outbox != nil {
		var err error
		oldDoc, err = tx.GetDoc(dt, name, owner)
		if err == nil {
			oldFields = oldDoc.Fields
		}
	}

	// Run before_delete hooks — scripts can reject deletion.
	if oldDoc != nil {
		if err := tx.runHooks(dt, script.EventBeforeDelete, oldDoc, nil); err != nil {
			return err
		}
	}

	dbTx, err := tx.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer dbTx.Rollback()

	for _, f := range dt.TableFields() {
		childTable := dt.ChildTableName(f.Fieldname)
		if _, err := dbTx.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE parent = ?", childTable),
			name,
		); err != nil {
			return fmt.Errorf("deleting child rows for %s: %w", f.Fieldname, err)
		}
	}

	where := "name = ?"
	args := []any{name}
	if owner != "" {
		where += " AND owner = ?"
		args = append(args, owner)
	}

	result, err := dbTx.Exec(
		fmt.Sprintf("DELETE FROM %s WHERE %s", dt.TableName(), where),
		args...,
	)
	if err != nil {
		return fmt.Errorf("deleting document: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("%w: document %q not found or access denied", ErrNotFound, name)
	}

	if err := tx.writeOutbox(dbTx, analytics.EventDelete, dt, name, "", oldFields, nil); err != nil {
		return fmt.Errorf("recording delete to outbox: %w", err)
	}

	if err := dbTx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	if tx.EventBus != nil && oldFields != nil {
		tx.EventBus.Publish(analytics.ChangeEvent{
			Site:       tx.SiteName,
			Doctype:    dt.Name,
			DocName:    name,
			Operation:  analytics.EventDelete,
			Timestamp:  time.Now(),
			ModifiedBy: "",
			Data:       oldFields,
		})
	}

	// Run after_delete hooks — best-effort.
	if oldDoc != nil {
		_ = tx.runHooks(dt, script.EventAfterDelete, oldDoc, nil)
	}

	return nil
}

// validSortColumns returns the set of column names that can be used in ORDER BY clauses.
// These are the DocType's data fields plus system columns.
func validSortColumns(dt *doctype.DocType) map[string]bool {
	return dt.ValidSortColumns()
}

// validateOrderBy parses and validates an ORDER BY clause against known columns.
// It returns a safe, backtick-quoted ORDER BY string suitable for SQL interpolation.
// This prevents SQL injection via the order_by query parameter.
func validateOrderBy(orderBy string, dt *doctype.DocType) (string, error) {
	if orderBy == "" {
		return "", fmt.Errorf("order_by must not be empty")
	}

	validCols := validSortColumns(dt)
	parts := strings.Split(orderBy, ",")
	var safeParts []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Split into field name and optional direction.
		segments := strings.Fields(part)
		if len(segments) == 0 || len(segments) > 2 {
			return "", fmt.Errorf("invalid ORDER BY clause: %q", part)
		}

		col := segments[0]
		dir := "ASC"
		if len(segments) == 2 {
			dir = strings.ToUpper(segments[1])
			if dir != "ASC" && dir != "DESC" {
				return "", fmt.Errorf("invalid sort direction %q in ORDER BY; must be ASC or DESC", segments[1])
			}
		}

		if !validCols[col] {
			return "", fmt.Errorf("unknown column %q in ORDER BY", col)
		}

		safeParts = append(safeParts, fmt.Sprintf("`%s` %s", col, dir))
	}

	if len(safeParts) == 0 {
		return "", fmt.Errorf("no valid columns in ORDER BY")
	}

	return strings.Join(safeParts, ", "), nil
}

// parseDuplicateError detects MySQL error 1062 (Duplicate entry) and converts it
// to a doctype.ValidationError. Unique constraints are enforced at the database level
// via UNIQUE KEY indexes (uq_{fieldname}); this function maps the error back to the field.
func parseDuplicateError(err error, dt *doctype.DocType) *doctype.ValidationError {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		// Message format: "Duplicate entry 'value' for key 'uq_fieldname'"
		fieldName := parseKeyFromDuplicateError(mysqlErr.Message)
		if fieldName != "" {
			// Find the field label for a user-friendly message.
			label := fieldName
			if f := dt.GetField(fieldName); f != nil {
				label = f.Label
			}
			return &doctype.ValidationError{
				Type:    "UniqueConstraint",
				Message: fmt.Sprintf("%s must be unique.", label),
				Field:   fieldName,
				DocType: dt.Name,
			}
		}
	}
	return nil
}

// parseNotNullError detects MySQL NOT NULL / missing default errors (1364, 1048) and
// converts them to a doctype.ValidationError so callers (including AI tool execution)
// get a clear, actionable message instead of a raw MySQL error.
func parseNotNullError(err error, dt *doctype.DocType) *doctype.ValidationError {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1364 || mysqlErr.Number == 1048) {
		// Messages: "Field 'full_name' doesn't have a default value" or "Column 'full_name' cannot be null"
		fieldName := parseFieldFromNotNullError(mysqlErr.Message)
		if fieldName != "" {
			label := fieldName
			if f := dt.GetField(fieldName); f != nil {
				label = f.Label
			}
			return &doctype.ValidationError{
				Type:    "NotNullConstraint",
				Message: fmt.Sprintf("%s is required.", label),
				Field:   fieldName,
				DocType: dt.Name,
			}
		}
	}
	return nil
}

// parseFieldFromNotNullError extracts the field name from a MySQL NOT NULL error.
// Formats: "Field 'fieldname' doesn't have a default value" or "Column 'fieldname' cannot be null"
func parseFieldFromNotNullError(msg string) string {
	for _, prefix := range []string{"Field '", "Column '"} {
		idx := strings.Index(msg, prefix)
		if idx >= 0 {
			start := idx + len(prefix)
			end := strings.IndexByte(msg[start:], '\'')
			if end >= 0 {
				return msg[start : start+end]
			}
		}
	}
	return ""
}

// parseKeyFromDuplicateError extracts the field name from a MySQL duplicate entry error message.
// MySQL format: "Duplicate entry 'value' for key 'uq_fieldname'"
// Returns "" if the key name cannot be parsed.
func parseKeyFromDuplicateError(msg string) string {
	// Look for "key 'uq_" prefix and extract the field name.
	const prefix = "key 'uq_"
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		// Also try without the uq_ prefix (some index types use different naming).
		const altPrefix = "key '"
		idx = strings.Index(msg, altPrefix)
		if idx < 0 {
			return ""
		}
		idx += len(altPrefix)
	} else {
		idx += len(prefix)
	}
	// Extract until the closing quote.
	end := strings.IndexByte(msg[idx:], '\'')
	if end < 0 {
		return ""
	}
	return msg[idx : idx+end]
}

// --- Helpers ---

func stringVal(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return fmt.Sprintf("%v", v)
}

// byteSliceToString converts []byte to string for JSON-safe storage.
// The MySQL driver returns []byte for VARCHAR/TEXT columns. JSON marshaling
// encodes []byte as base64, so we must convert to string first.
func byteSliceToString(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

func intVal(v any) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func convertDefault(def string, fieldtype string) any {
	switch fieldtype {
	case "Int":
		var n int64
		fmt.Sscanf(def, "%d", &n)
		return n
	case "Float", "Currency", "Percent":
		var f float64
		fmt.Sscanf(def, "%f", &f)
		return f
	case "Check":
		return def == "1" || def == "true"
	case "Date":
		if strings.EqualFold(def, "today") {
			return time.Now().Format("2006-01-02")
		}
	case "Datetime":
		if strings.EqualFold(def, "now") || strings.EqualFold(def, "today") {
			return time.Now()
		}
	case "Time":
		if strings.EqualFold(def, "now") {
			return time.Now().Format("15:04:05.000000")
		}
	default:
		return def
	}
	return def
}

// copyFieldsWithStatus creates a copy of fields with doc_status injected for analytics.
func copyFieldsWithStatus(fields map[string]any, docStatus int) map[string]any {
	out := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	out["doc_status"] = docStatus
	return out
}

// normalizeSQLValue converts a field value to a SQL-bindable type.
// JSON fields: arrays/objects/slices are marshalled to JSON strings.
// Strings are validated as JSON. Everything else passes through.
func normalizeSQLValue(f doctype.Field, v any) (any, error) {
	if f.Fieldtype != "JSON" {
		return v, nil
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if x == "" {
			return nil, nil
		}
		var js json.RawMessage
		if err := json.Unmarshal([]byte(x), &js); err != nil {
			return nil, fmt.Errorf("invalid JSON")
		}
		return x, nil
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return string(b), nil
	}
}
