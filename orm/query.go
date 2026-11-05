// Package orm provides the generic ORM layer for Kigae.
// All documents are represented as doctype.Document (map[string]any).
package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/numeric"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/workflow"
	"github.com/go-sql-driver/mysql"
)

func derivePrefix(name string) string {
	return DerivePrefix(name)
}

func (tx *TxManager) tableName(dt *doctype.DocType) string {
	return quoteTableName(tx.Dialect, dt.RawTableName())
}

// configuredSearchFieldNames returns configured fields in declaration order.
// Legacy DocTypes without search_fields retain their declared broad field set,
// but the query caller uses only explicitly indexed fields for quick search.
// Hidden and password fields are filtered from the result in both modes.
func configuredSearchFieldNames(d *doctype.DocType) []string {
	var fields []string
	seen := make(map[string]struct{})
	for _, name := range strings.Split(d.SearchFields, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		fields = append(fields, name)
	}
	if len(fields) == 0 {
		for _, field := range d.NonTableDataFields() {
			if searchableFieldType(field.Fieldtype) {
				fields = append(fields, field.Fieldname)
			}
		}
	}
	if title := strings.TrimSpace(d.TitleField); title != "" {
		if _, ok := seen[title]; !ok {
			fields = append(fields, title)
		}
	}
	return fields
}

func (tx *TxManager) childTableName(dt *doctype.DocType, field string) string {
	return quoteTableName(tx.Dialect, dt.RawChildTableName(field))
}

func quoteTableName(dialect db.QueryDialect, name string) string {
	if identifiers, ok := dialect.(interface{ QuoteIdent(string) string }); ok {
		return identifiers.QuoteIdent(name)
	}
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// DerivePrefix returns the stable prefix used by generated document names.
// Kernel operations use it when a transaction needs to preassign names for
// related records before resolving their cross-record references.
func DerivePrefix(name string) string {
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
	DB                          *sql.DB
	BusinessScope               contract.BusinessScope
	BusinessReferenceValidator  contract.BusinessReferenceValidator
	HistoricalSnapshotValidator contract.HistoricalSnapshotValidator
	// Transaction binds reads performed during a kernel operation to its owner
	// transaction. The kernel sets and clears this only for the operation scope.
	Transaction *sql.Tx
	Registry    *doctype.Registry
	Dialect     db.Dialect

	// Context carries the request-scoped context for script execution and
	// other cancellable operations. If nil, context.Background() is used.
	Context context.Context

	// Outbox writes events into the transactional outbox in the same transaction
	// as the business write (RFC §8.1). Mutation entry points are owned by the
	// kernel; a missing outbox is therefore an explicit disabled-delivery mode.
	Outbox outbox.Writer
	// EventOrigin is set by the kernel for one operation. It stamps the single
	// change event of each mutation with the authenticated origin, which
	// record-triggered workflows require.
	EventOrigin *EventOrigin
	// WorkflowStatusPolicies resolves published status policies for every
	// request-scoped kernel mutation path.
	WorkflowStatusPolicies workflow.StatusPolicyRepository

	// ScriptRunner executes JavaScript hooks (before_save, after_insert, etc.).
	// If nil, script execution is disabled.
	ScriptRunner script.Runner

	// ScriptStore persists script definitions and execution logs.
	// If nil, script loading and logging is disabled.
	ScriptStore *script.Store

	// ScriptProvider bridges scripts to the engine (ORM, secrets, HTTP).
	// Created per-request by siteTx() with the site's DB and registry.
	ScriptProvider script.KoraProvider
	// ScriptProviderFactory rebinds a request or tenant provider to the copied,
	// operation-scoped TxManager created by the kernel. Providers must return a
	// lightweight scoped copy; they may share immutable clients and transports.
	ScriptProviderFactory func(*TxManager) script.KoraProvider
	// ModuleDescriptors and ModuleExtensions are the current tenant activation
	// snapshot. Module-owned hook scripts must be present in the ordered extension
	// plan before they can run.
	ModuleDescriptors      []contract.ModuleDescriptor
	ModuleExtensions       []contract.ResolvedModuleExtension
	ModuleExtensionSchemas interface {
		Validate(contract.SchemaRef, json.RawMessage) error
	}

	// SiteName is the tenant identifier used in analytics events.
	SiteName string

	// AsyncHookSink receives after_* hooks for fire-and-forget execution.
	// If non-nil, after_* events are handed to the async worker contract.
	AsyncHookSink AsyncHookSink
	// AsyncHookInvocations is the durable hook store installed by the tenant
	// runtime. Kernel commands append frozen hooks in their source transaction.
	AsyncHookInvocations     script.AsyncHookInvocationStore
	AsyncHookTenantID        contract.TenantID
	AsyncHookSourceOperation contract.OperationID
	AsyncHookConfigRevision  uint64
	AsyncHookSequence        uint16
	SuppressAsyncHookEvents  bool

	// User and UserRole from the current request context, used for script execution.
	CurrentUser      string
	CurrentUserRole  string
	CurrentUserRoles []string
	// Execution is copied from the verified operation at the command boundary
	// and inherited by nested script-provider mutations.
	Execution       policy.Execution
	SkipHookScripts []string
	// ScriptMutationExecutor is scoped to the active kernel transaction and is
	// attached to the provider passed into lifecycle scripts. It is nil outside
	// an uncommitted operation, so post-commit scripts start a fresh command.
	ScriptMutationExecutor script.MutationExecutor
	// PostCommitHooks delays after_* callbacks from nested script writes until
	// the owning operation transaction has committed.
	PostCommitHooks []func()
	// HookScriptsByDoctype is a command-scoped snapshot of active lifecycle
	// scripts. Kernel.Execute resets it for each command so each DocType is read
	// once, rather than querying the script table once per lifecycle event.
	HookScriptsByDoctype map[string][]script.ScriptRecord
}

type contextQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (tx *TxManager) queryer() contextQueryer {
	if tx != nil && tx.Transaction != nil {
		return tx.Transaction
	}
	return tx.DB
}

func (tx *TxManager) queryContext() context.Context {
	if tx != nil && tx.Context != nil {
		return tx.Context
	}
	return context.Background()
}

// EventOrigin is the authenticated cause of a kernel mutation.
type EventOrigin struct {
	Actor          contract.ActorContext
	CorrelationID  string
	CausationID    string
	CausationDepth uint16
}

// recordChange identifies the committed record version a change event
// describes, so workflow triggers can consume the same event as webhooks.
type recordChange struct {
	Revision      uint64
	ChangedFields []string
}

// ChangedFields lists fields whose JSON value differs between two versions of
// a record, ignoring bookkeeping fields. The result is sorted.
func ChangedFields(before, after map[string]any) []string {
	changed := make([]string, 0)
	for field, value := range after {
		if field == "modified" || field == "modified_by" || field == "owner" {
			continue
		}
		previous, previousErr := json.Marshal(before[field])
		next, nextErr := json.Marshal(value)
		if previousErr != nil || nextErr != nil || string(previous) != string(next) {
			changed = append(changed, field)
		}
	}
	for field := range before {
		if _, exists := after[field]; !exists {
			changed = append(changed, field)
		}
	}
	sort.Strings(changed)
	return changed
}

// writeOutbox records the one change event of a mutation into the
// transactional outbox within the given transaction. It is a no-op when no
// outbox writer is configured.
func (tx *TxManager) writeOutbox(dbTx *sql.Tx, op eventstream.EventOp, dt *doctype.DocType, docName, modifiedBy string, data, oldData map[string]any, record recordChange) error {
	if tx.Outbox == nil {
		return nil
	}
	ctx := tx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	change := eventstream.ChangeEvent{
		Site:          tx.SiteName,
		BusinessScope: tx.BusinessScope,
		Doctype:       dt.Name,
		DocName:       docName,
		Operation:     op,
		Timestamp:     time.Now(),
		ModifiedBy:    modifiedBy,
		Data:          data,
		OldData:       oldData,
	}
	envelope := outbox.ChangeEventToEnvelope(change)
	payload := map[string]any{"data": data, "old_data": oldData, "table": dt.Name, "record_id": docName}
	if record.Revision > 0 {
		payload["revision"] = strconv.FormatUint(record.Revision, 10)
	}
	if len(record.ChangedFields) > 0 {
		payload["changed_fields"] = record.ChangedFields
	}
	envelope.Data = contract.MustEncodeData(payload)
	if origin := tx.EventOrigin; origin != nil {
		envelope.Actor = origin.Actor
		envelope.CorrelationID = origin.CorrelationID
		envelope.CausationID = origin.CausationID
		envelope.CausationDepth = origin.CausationDepth
	}
	return tx.Outbox.Append(ctx, dbTx, envelope)
}

// PrepareInsert runs the before_insert and before_save lifecycle hooks. It is
// used by kernel transactions so hooks run once.
func (tx *TxManager) PrepareInsert(dt *doctype.DocType, doc *doctype.Document) error {
	if err := tx.runHooks(dt, script.EventBeforeInsert, doc, nil); err != nil {
		return err
	}
	return tx.runHooks(dt, script.EventBeforeSave, doc, nil)
}

// CompleteInsert runs the best-effort after_insert and after_save hooks after
// the database transaction has committed.
func (tx *TxManager) CompleteInsert(dt *doctype.DocType, doc *doctype.Document) {
	_ = tx.runHooks(dt, script.EventAfterInsert, doc, nil)
	_ = tx.runHooks(dt, script.EventAfterSave, doc, nil)
}

// InsertInTx persists a new business record inside the operation kernel's
// transaction. It never begins or commits a transaction; the kernel owns the
// business write together with its receipt, audit row, and outbox events.
func (tx *TxManager) InsertInTx(dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document, owner, modifiedBy string) error {
	if err := dt.BindBusinessScope(doc, tx.BusinessScope, true, nil); err != nil {
		return fmt.Errorf("validating business scope: %w", err)
	}
	if err := tx.validateEntityReferences(dbTx, dt, doc); err != nil {
		return err
	}
	nextNum := 1
	if doc.Name == "" {
		name, allocated, err := AllocateDocumentName(dbTx, tx.Dialect, dt, doc.Fields, time.Now().UTC())
		if err != nil {
			return err
		}
		nextNum = int(allocated)
		doc.Name = name
	}

	// Match the precision persisted by the supported SQL dialects so the
	// optimistic-concurrency token returned in the same response can be echoed
	// back without being newer than the stored value.
	now := time.Now().UTC().Truncate(time.Microsecond)
	doc.DocStatus = 0
	doc.Set("modified", now)

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
		val, err := canonicalizeDecimal(doc, f, val)
		if err != nil {
			return err
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
		tx.tableName(dt),
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "),
	)

	if _, err := dbTx.Exec(db.Rebind(tx.Dialect, query), values...); err != nil {
		if valErr := tx.Dialect.ParseError(err, dt); valErr != nil {
			if valErr.Type == "UniqueConstraint" {
				return fmt.Errorf("%w: %w", ErrDuplicate, valErr)
			}
			return fmt.Errorf("%w: %w", ErrValidation, valErr)
		}
		return fmt.Errorf("inserting document: %w", err)
	}
	doc.Revision = 1

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
			if validationErr := tx.Dialect.ParseError(err, childDT); validationErr != nil {
				if validationErr.Type == "UniqueConstraint" {
					return fmt.Errorf("%w: %w", ErrDuplicate, validationErr)
				}
				return fmt.Errorf("%w: %w", ErrValidation, validationErr)
			}
			return fmt.Errorf("inserting child rows in %s: %w", f.Fieldname, err)
		}
	}

	computedHook := tx.computedScriptHook()

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
			if err := doctype.ComputeFieldsWithHook(childDT, child, computedHook); err != nil {
				slog.Warn("computed fields failed on child", "doctype", childDT.Name, "error", err)
			}
		}
		if err := persistComputedChildFields(dbTx, dt, f.Fieldname, childDT, children, tx.Dialect); err != nil {
			return fmt.Errorf("persisting computed child fields in %s: %w", f.Fieldname, err)
		}
	}

	// Resolve links before computed expressions consume their values.
	if err := tx.populateLinkedFields(dbTx, dt, doc); err != nil {
		return fmt.Errorf("populating linked fields: %w", err)
	}
	// Evaluate computed fields on the parent document (e.g., subtotal = SUM(items.line_total)).
	if err := doctype.ComputeFieldsWithHook(dt, doc, computedHook); err != nil {
		slog.Warn("computed fields failed", "doctype", dt.Name, "error", err)
	}

	// Persist linked and computed field values via UPDATE.
	if err := updateDerivedFieldsExec(dbTx, dt, doc, tx.Dialect); err != nil {
		return fmt.Errorf("persisting derived fields: %w", err)
	}

	if err := tx.writeOutbox(dbTx, eventstream.EventInsert, dt, doc.Name, modifiedBy, copyFieldsWithStatus(doc.Fields, doc.DocStatus), nil, recordChange{Revision: doc.Revision}); err != nil {
		return fmt.Errorf("recording insert to outbox: %w", err)
	}

	return nil
}

// updateComputedFields UPDATEs only computed field columns on the document.
func (tx *TxManager) updateComputedFields(dt *doctype.DocType, doc *doctype.Document) error {
	return tx.updateComputedFieldsExec(tx.DB, dt, doc)
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
		if targetDT.BusinessScopePolicy == contract.ScopeEntityOwned {
			if contract.ValidateResourceBusinessScope(tx.BusinessScope, targetDT.BusinessScopePolicy) != nil || targetDT.BusinessScopeField == "" {
				return fmt.Errorf("entity-owned linked resource %s requires verified entity scope", targetDT.Name)
			}
			where = "(" + where + ") AND " + tx.Dialect.QuoteIdent(targetDT.BusinessScopeField) + " = ?"
			args = append(args, tx.BusinessScope.LegalEntityID)
		}
		targetMeta := targetDT.GetField(targetField)
		if targetMeta == nil {
			continue
		}
		query := fmt.Sprintf("SELECT %s FROM %s WHERE %s LIMIT 1", targetField, tx.tableName(targetDT), where)
		var value any
		if err := ex.QueryRow(db.Rebind(tx.Dialect, query), args...).Scan(&value); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if bytes, ok := value.([]byte); ok {
			textValue := string(bytes)
			switch targetMeta.Fieldtype {
			case "Float", "Currency", "Percent":
				if number, err := numeric.Normalize(textValue); err == nil {
					value = number
				} else {
					value = textValue
				}
			case "Int":
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
func (tx *TxManager) updateComputedFieldsExec(ex db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	return updateDerivedFieldsExec(ex, dt, doc, tx.Dialect)
}

// updateDerivedFieldsExec persists both linked and computed values produced by
// the document lifecycle. It intentionally excludes ordinary user fields.
func updateDerivedFieldsExec(ex db.Queryer, dt *doctype.DocType, doc *doctype.Document, dialect db.QueryDialect) error {
	var setClauses []string
	var values []any

	for _, f := range dt.Fields {
		if (f.Computed == "" && f.LinkedField == "") || f.Fieldtype == "Table" {
			continue
		}
		val, err := canonicalizeDecimal(doc, f, doc.Get(f.Fieldname))
		if err != nil {
			return err
		}
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
		quoteTableName(dialect, dt.RawTableName()),
		strings.Join(setClauses, ", "),
	)

	_, err := ex.Exec(db.Rebind(dialect, query), values...)
	return err
}

// PrepareSave runs the before_save lifecycle hook. It is shared by ordinary
// kernel transactions so the document is prepared consistently.
func (tx *TxManager) PrepareSave(dt *doctype.DocType, doc, oldDoc *doctype.Document) error {
	return tx.runHooks(dt, script.EventBeforeSave, doc, oldDoc)
}

// CompleteSave runs the best-effort after_save hook after commit.
func (tx *TxManager) CompleteSave(dt *doctype.DocType, doc, oldDoc *doctype.Document) {
	_ = tx.runHooks(dt, script.EventAfterSave, doc, oldDoc)
}

// SaveInTx updates a business record inside the operation kernel's transaction.
// It never begins or commits a transaction; the kernel owns the mutation and
// its receipt, audit row, and outbox events. Returns orm.ErrNotFound wrapped
// error when the row vanished or is owner-hidden.
func (tx *TxManager) SaveInTx(dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document, modifiedBy string, owner string, oldDoc *doctype.Document) error {
	return tx.SaveInTxWithScope(dbTx, dt, doc, modifiedBy, policy.RowScope{UserID: owner, Owner: owner != ""}, oldDoc)
}

// SaveInTxWithScope applies optimistic revision matching and the actor's
// current owner-or-assignee predicate in the same write statement.
func (tx *TxManager) SaveInTxWithScope(dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document, modifiedBy string, scope policy.RowScope, oldDoc *doctype.Document) error {
	if err := dt.BindBusinessScope(doc, scope.Business, false, oldDoc); err != nil {
		return fmt.Errorf("validating business scope: %w", err)
	}
	if err := tx.validateEntityReferences(dbTx, dt, doc); err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	doc.Set("modified", now)
	dataFields := dt.NonTableDataFields()

	var setClauses []string
	var values []any

	for _, f := range dataFields {
		// Note: read_only is a UI hint, not an ORM constraint.
		// Kernel command policy decides whether the current operation may change
		// the field (for example, a trusted workflow transition).
		newVal, err := canonicalizeDecimal(doc, f, doc.Get(f.Fieldname))
		if err != nil {
			return err
		}
		if oldDoc != nil {
			if oldVal := oldDoc.Get(f.Fieldname); oldVal == doc.Get(f.Fieldname) {
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

	setClauses = append(setClauses, "modified = ?", "modified_by = ?", "doc_status = ?", "revision = revision + 1")
	values = append(values, now, modifiedBy, doc.DocStatus)

	where := "name = ?"
	values = append(values, doc.Name)
	if oldDoc != nil && oldDoc.Revision > 0 {
		where += " AND revision = ?"
		values = append(values, oldDoc.Revision)
	}
	scopedWhere, scopedArgs, scopeErr := tx.rowScopePredicate(dt, scope)
	if scopeErr != nil {
		return scopeErr
	}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		values = append(values, scope.UserID)
	} else if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		values = append(values, scopedArgs...)
	}

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s",
		tx.tableName(dt),
		strings.Join(setClauses, ", "),
		where,
	)

	result, err := dbTx.Exec(db.Rebind(tx.Dialect, query), values...)
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
		if oldDoc != nil && oldDoc.Revision > 0 {
			return fmt.Errorf("%w: document %q revision changed", ErrConflict, doc.Name)
		}
		return fmt.Errorf("%w: document %q not found or access denied", ErrNotFound, doc.Name)
	}
	if oldDoc != nil && oldDoc.Revision > 0 {
		doc.Revision = oldDoc.Revision + 1
	} else if doc.Revision == 0 {
		// Callers that do not provide a snapshot still receive the new
		// revision without dereferencing a nil old document.
		doc.Revision = 1
	}

	for _, f := range dt.TableFields() {
		childTableName := tx.childTableName(dt, f.Fieldname)
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
			if _, err := dbTx.Exec(db.Rebind(tx.Dialect,
				fmt.Sprintf("DELETE FROM %s WHERE parent = ?", childTableName)),
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

	computedHook := tx.computedScriptHook()

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
			if err := doctype.ComputeFieldsWithHook(childDT, child, computedHook); err != nil {
				slog.Warn("computed fields failed on child", "doctype", childDT.Name, "error", err)
			}
		}
		if err := persistComputedChildFields(dbTx, dt, f.Fieldname, childDT, children, tx.Dialect); err != nil {
			return fmt.Errorf("persisting computed child fields in %s: %w", f.Fieldname, err)
		}
	}

	if err := tx.populateLinkedFields(dbTx, dt, doc); err != nil {
		return fmt.Errorf("populating linked fields: %w", err)
	}
	if err := doctype.ComputeFieldsWithHook(dt, doc, computedHook); err != nil {
		slog.Warn("computed fields failed", "doctype", dt.Name, "error", err)
	}

	if err := updateDerivedFieldsExec(dbTx, dt, doc, tx.Dialect); err != nil {
		return fmt.Errorf("persisting derived fields: %w", err)
	}

	var oldOutboxData map[string]any
	update := recordChange{Revision: doc.Revision}
	if oldDoc != nil {
		oldOutboxData = copyFieldsWithStatus(oldDoc.Fields, oldDoc.DocStatus)
		update.ChangedFields = ChangedFields(oldDoc.Fields, doc.Fields)
	}
	if err := tx.writeOutbox(dbTx, eventstream.EventUpdate, dt, doc.Name, modifiedBy, copyFieldsWithStatus(doc.Fields, doc.DocStatus), oldOutboxData, update); err != nil {
		return fmt.Errorf("recording save to outbox: %w", err)
	}

	return nil
}

func (tx *TxManager) validateEntityReferences(queryer db.Queryer, dt *doctype.DocType, doc *doctype.Document) error {
	if tx == nil || tx.Registry == nil || dt == nil || doc == nil {
		return nil
	}
	for _, field := range dt.Fields {
		if field.Fieldtype != "Link" || field.Fieldname == dt.BusinessScopeField {
			continue
		}
		value := strings.TrimSpace(fmt.Sprint(doc.Get(field.Fieldname)))
		if value == "" || value == "<nil>" {
			continue
		}
		target := tx.Registry.Get(field.Options)
		if target == nil || target.BusinessScopePolicy != contract.ScopeEntityOwned {
			continue
		}
		if contract.ValidateResourceBusinessScope(tx.BusinessScope, contract.ScopeEntityOwned) != nil || target.BusinessScopeField == "" {
			return fmt.Errorf("reference to entity-owned resource %s requires verified entity scope", target.Name)
		}
		where := "name = ?"
		args := []any{value}
		if target.TitleField != "" && target.GetField(target.TitleField) != nil {
			where = "(" + where + " OR " + tx.Dialect.QuoteIdent(target.TitleField) + " = ?)"
			args = append(args, value)
		}
		where += " AND " + tx.Dialect.QuoteIdent(target.BusinessScopeField) + " = ?"
		args = append(args, tx.BusinessScope.LegalEntityID)
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", tx.tableName(target), where)
		if err := queryer.QueryRow(db.Rebind(tx.Dialect, query), args...).Scan(&count); err != nil {
			return fmt.Errorf("validating entity-owned reference %s.%s: %w", dt.Name, field.Fieldname, err)
		}
		if count == 0 {
			return fmt.Errorf("reference %s.%s does not resolve within the verified legal entity", dt.Name, field.Fieldname)
		}
	}
	return nil
}

// GetDoc loads a single document by name, including child table expansion.
// If owner is non-empty, only returns the document if owned by that user.
func (tx *TxManager) GetDoc(dt *doctype.DocType, name string, owner string) (*doctype.Document, error) {
	return tx.GetDocWithScope(dt, name, policy.RowScope{UserID: owner, Owner: owner != ""})
}

// GetDocWithScope reads a document only when the verified actor is its owner
// or matches one of the explicitly configured assignment fields.
func (tx *TxManager) GetDocWithScope(dt *doctype.DocType, name string, scope policy.RowScope) (*doctype.Document, error) {
	dataFields := dt.NonTableDataFields()

	var cols []string
	for _, f := range dataFields {
		cols = append(cols, f.Fieldname)
	}
	cols = append(cols, "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision")

	scanTargets := make([]any, len(cols))
	for i := range cols {
		var v any
		scanTargets[i] = &v
	}

	where := "name = ?"
	args := []any{name}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		args = append(args, scope.UserID)
		scope.Owner = false
	}
	scopedWhere, scopedArgs, err := tx.rowScopePredicate(dt, scope)
	if err != nil {
		return nil, err
	}
	if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		args = append(args, scopedArgs...)
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s",
		strings.Join(cols, ", "),
		tx.tableName(dt),
		where,
	)

	row := tx.queryer().QueryRowContext(tx.queryContext(), db.Rebind(tx.Dialect, query), args...)
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
		case "revision":
			doc.Revision = uint64(intVal(val))
		case "modified":
			// Resource reads expose this as the optimistic-concurrency token.
			// Keep the persisted database value so the token from GET can be
			// echoed back on the next update.
			doc.Set("modified", val)
		case "owner", "creation", "modified_by":
			// System columns.
		default:
			doc.Fields[col] = normalizeScannedFieldValue(dt.GetField(col), val)
		}
	}

	for _, f := range dt.TableFields() {
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		children, err := tx.getChildRows(tx.childTableName(dt, f.Fieldname), childDT, name)
		if err != nil {
			return nil, fmt.Errorf("loading child table %s: %w", f.Fieldname, err)
		}
		doc.Fields[f.Fieldname] = children
	}

	return doc, nil
}

func (tx *TxManager) rowScopePredicate(dt *doctype.DocType, scope policy.RowScope) (string, []any, error) {
	var entityClause string
	var entityArgs []any
	if scope.Business.TenantID != "" || scope.Business.Kind != "" || scope.Business.LegalEntityID != "" {
		if err := scope.Business.Validate(); err != nil {
			return "", nil, fmt.Errorf("invalid business scope: %w", err)
		}
	}
	if dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		if contract.ValidateResourceBusinessScope(scope.Business, dt.BusinessScopePolicy) != nil || dt.BusinessScopeField == "" || dt.GetField(dt.BusinessScopeField) == nil {
			return "", nil, fmt.Errorf("entity-owned resource %s requires explicit legal entity scope", dt.Name)
		}
		entityClause = tx.Dialect.QuoteIdent(dt.BusinessScopeField) + " = ?"
		entityArgs = append(entityArgs, scope.Business.LegalEntityID)
	}
	if scope.UserID == "" {
		if scope.Owner || len(scope.AssignedFields) > 0 {
			return "", nil, fmt.Errorf("row scope requires an actor identity")
		}
		return entityClause, entityArgs, nil
	}
	var actorClauses []string
	var actorArgs []any
	if scope.Owner {
		actorClauses = append(actorClauses, "owner = ?")
		actorArgs = append(actorArgs, scope.UserID)
	}
	seen := map[string]bool{}
	for _, fieldName := range scope.AssignedFields {
		fieldName = strings.TrimSpace(fieldName)
		if fieldName == "" || seen[fieldName] {
			continue
		}
		seen[fieldName] = true
		field := dt.GetField(fieldName)
		if field == nil || field.Fieldtype != "User" {
			return "", nil, fmt.Errorf("assignment scope field %q on %s must be a User field", fieldName, dt.Name)
		}
		actorClauses = append(actorClauses, tx.Dialect.QuoteIdent(fieldName)+" = ?")
		actorArgs = append(actorArgs, scope.UserID)
	}
	if len(actorClauses) == 0 {
		return entityClause, entityArgs, nil
	}
	actorClause := strings.Join(actorClauses, " OR ")
	if entityClause == "" {
		return actorClause, actorArgs, nil
	}
	return entityClause + " AND (" + actorClause + ")", append(entityArgs, actorArgs...), nil
}

// GetList returns a paginated list of documents with optional filtering.
// If owner is non-empty, only returns documents owned by that user.
func (tx *TxManager) GetList(dt *doctype.DocType, filters string, orderBy string, limit, offset int, owner string) ([]*doctype.Document, int, error) {
	return tx.GetListWithOptions(dt, filters, orderBy, limit, offset, owner, nil, "")
}

// GetListWithOptions is the projected/searchable form of GetList used by HTTP
// list endpoints. requestedFields is applied in SQL, not after scanning rows.
func (tx *TxManager) GetListWithOptions(dt *doctype.DocType, filters string, orderBy string, limit, offset int, owner string, requestedFields []string, search string) ([]*doctype.Document, int, error) {
	return tx.GetListWithScope(dt, filters, orderBy, limit, offset, policy.RowScope{UserID: owner, Owner: owner != ""}, requestedFields, search)
}

// GetListWithScope is the row-scoped list query used by authenticated
// resource consumers. Assignment field names come only from tenant policy.
func (tx *TxManager) GetListWithScope(dt *doctype.DocType, filters string, orderBy string, limit, offset int, scope policy.RowScope, requestedFields []string, search string) ([]*doctype.Document, int, error) {
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
	scopedWhere, scopedArgs, scopeErr := tx.rowScopePredicate(dt, scope)
	if scopeErr != nil {
		return nil, 0, scopeErr
	}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		whereArgs = append(whereArgs, scope.UserID)
	} else if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		whereArgs = append(whereArgs, scopedArgs...)
	}
	if search = strings.TrimSpace(search); search != "" {
		var searchClauses []string
		// Document names are opaque system identifiers. Exact comparison uses
		// their primary-key index and avoids weakening the configured business
		// field matching rules.
		nameSearchValue := search
		for _, fieldName := range configuredSearchFieldNames(dt) {
			f := dt.GetField(fieldName)
			if f == nil || f.Hidden || strings.EqualFold(f.Fieldtype, "Password") || !searchableFieldType(f.Fieldtype) || !f.SearchIndex {
				continue
			}
			// Quick search only targets indexed fields and uses prefix matching,
			// allowing the database to use the configured B-tree search index.
			searchValue := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(search) + "%"
			if strings.EqualFold(f.Fieldtype, "Link") && tx.Registry != nil {
				if target := tx.Registry.Get(strings.TrimSpace(f.Options)); target != nil {
					title := strings.TrimSpace(target.TitleField)
					if titleField := target.GetField(title); title != "" && titleField != nil && !titleField.Hidden && searchableFieldType(titleField.Fieldtype) && titleField.SearchIndex {
						searchClauses = append(searchClauses, fmt.Sprintf("%s LIKE ? ESCAPE '!' OR %s IN (SELECT %s FROM %s WHERE %s LIKE ? ESCAPE '!')",
							tx.Dialect.QuoteIdent(f.Fieldname),
							tx.Dialect.QuoteIdent(f.Fieldname),
							tx.Dialect.QuoteIdent("name"),
							tx.tableName(target),
							tx.Dialect.QuoteIdent(title)))
						whereArgs = append(whereArgs, searchValue, searchValue)
						continue
					}
				}
			}
			searchClauses = append(searchClauses, fmt.Sprintf("%s LIKE ? ESCAPE '!'", tx.Dialect.QuoteIdent(f.Fieldname)))
			whereArgs = append(whereArgs, searchValue)
		}
		searchClauses = append(searchClauses, fmt.Sprintf("%s = ?", tx.Dialect.QuoteIdent("name")))
		whereArgs = append(whereArgs, nameSearchValue)
		where += " AND (" + strings.Join(searchClauses, " OR ") + ")"
	}

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", tx.tableName(dt), where)
	err := tx.queryer().QueryRowContext(tx.queryContext(), db.Rebind(tx.Dialect, countQuery), whereArgs...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting documents: %w", err)
	}

	dataFields := dt.NonTableDataFields()
	selected := make(map[string]bool)
	var columnNames []string
	if len(requestedFields) > 0 {
		for _, name := range requestedFields {
			if name == "name" || name == "owner" || name == "creation" || name == "modified" || name == "modified_by" || name == "doc_status" {
				selected[name] = true
				continue
			}
			if f := dt.GetField(name); f != nil && f.Fieldtype != "Table" {
				selected[name] = true
			}
		}
	}
	for _, f := range dataFields {
		if len(requestedFields) == 0 || selected[f.Fieldname] {
			columnNames = append(columnNames, f.Fieldname)
		}
	}
	for _, name := range []string{"name", "owner", "creation", "modified", "modified_by", "doc_status"} {
		if len(requestedFields) == 0 || selected[name] || name == "name" {
			if !containsString(columnNames, name) {
				columnNames = append(columnNames, name)
			}
		}
	}
	cols := make([]string, 0, len(columnNames))
	for _, name := range columnNames {
		cols = append(cols, tx.Dialect.QuoteIdent(name))
	}

	if orderBy == "" {
		orderBy = dt.SortField + " " + dt.SortOrder
	}

	// Validate orderBy against known columns to prevent SQL injection.
	safeOrderBy, err := validateOrderBy(orderBy, dt, tx.Dialect.QuoteIdent)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid order_by: %w", err)
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT ? OFFSET ?",
		strings.Join(cols, ", "),
		tx.tableName(dt),
		where,
		safeOrderBy,
	)

	args := append(whereArgs, limit, offset)

	rows, err := tx.queryer().QueryContext(tx.queryContext(), db.Rebind(tx.Dialect, query), args...)
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

		for i, col := range columnNames {
			val := *(scanTargets[i].(*any))
			switch col {
			case "name":
				doc.Name = stringVal(val)
			case "doc_status":
				doc.DocStatus = intVal(val)
			default:
				doc.Fields[col] = normalizeScannedFieldValue(dt.GetField(col), val)
			}
		}

		docs = append(docs, doc)
	}

	return docs, total, rows.Err()
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func searchableFieldType(fieldType string) bool {
	switch fieldType {
	case "Data", "Text", "Small Text", "Long Text", "Link", "Dynamic Link", "Select", "Email", "Phone", "Code", "Autocomplete":
		return true
	default:
		return false
	}
}

// PrepareDelete runs the before_delete lifecycle hook when the prior document
func (tx *TxManager) PrepareDelete(dt *doctype.DocType, oldDoc *doctype.Document) error {
	if oldDoc == nil {
		return nil
	}
	return tx.runHooks(dt, script.EventBeforeDelete, oldDoc, nil)
}

// CompleteDelete runs the best-effort after_delete lifecycle hook after commit.
func (tx *TxManager) CompleteDelete(dt *doctype.DocType, oldDoc *doctype.Document) {
	if oldDoc != nil {
		_ = tx.runHooks(dt, script.EventAfterDelete, oldDoc, nil)
	}
}

// DeleteInTx removes a business record and its child rows in the operation
// kernel's transaction. The kernel owns commit and lifecycle-hook scheduling.
func (tx *TxManager) DeleteInTx(dbTx *sql.Tx, dt *doctype.DocType, name, owner string, oldDoc *doctype.Document) error {
	return tx.DeleteInTxWithScope(dbTx, dt, name, policy.RowScope{UserID: owner, Owner: owner != ""}, oldDoc)
}

// DeleteInTxWithScope deletes only when the current persisted row remains
// visible to the actor under the transaction's assignment policy.
func (tx *TxManager) DeleteInTxWithScope(dbTx *sql.Tx, dt *doctype.DocType, name string, scope policy.RowScope, oldDoc *doctype.Document) error {
	for _, f := range dt.TableFields() {
		childTable := tx.childTableName(dt, f.Fieldname)
		if _, err := dbTx.Exec(
			db.Rebind(tx.Dialect, fmt.Sprintf("DELETE FROM %s WHERE parent = ?", childTable)),
			name,
		); err != nil {
			return fmt.Errorf("deleting child rows for %s: %w", f.Fieldname, err)
		}
	}

	where := "name = ?"
	args := []any{name}
	scopedWhere, scopedArgs, scopeErr := tx.rowScopePredicate(dt, scope)
	if scopeErr != nil {
		return scopeErr
	}
	if scope.Owner && len(scope.AssignedFields) == 0 && scope.UserID != "" && dt.BusinessScopePolicy != contract.ScopeEntityOwned {
		where += " AND owner = ?"
		args = append(args, scope.UserID)
	} else if scopedWhere != "" {
		where += " AND (" + scopedWhere + ")"
		args = append(args, scopedArgs...)
	}
	result, err := dbTx.Exec(db.Rebind(tx.Dialect, fmt.Sprintf("DELETE FROM %s WHERE %s", tx.tableName(dt), where)), args...)
	if err != nil {
		return fmt.Errorf("deleting document: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("%w: document %q not found or access denied", ErrNotFound, name)
	}
	var oldFields map[string]any
	if oldDoc != nil {
		oldFields = oldDoc.Fields
	}
	if err := tx.writeOutbox(dbTx, eventstream.EventDelete, dt, name, "", oldFields, nil, recordChange{}); err != nil {
		return fmt.Errorf("recording delete to outbox: %w", err)
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
func validateOrderBy(orderBy string, dt *doctype.DocType, quoteIdent func(string) string) (string, error) {
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

		safeParts = append(safeParts, fmt.Sprintf("%s %s", quoteIdent(col), dir))
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

// normalizeScannedFieldValue gives temporal database values the same canonical
// string representation used by document mutations. MySQL's parseTime option
// returns DATE/TIME/DATETIME columns as time.Time; preserving that value makes
// Document.GetString return an empty string, which can silently skip date-based
// domain work such as recurring invoice generation.
func normalizeScannedFieldValue(field *doctype.Field, value any) any {
	if field == nil {
		return byteSliceToString(value)
	}
	if parsed, ok := value.(time.Time); ok {
		switch field.Fieldtype {
		case "Date":
			return parsed.Format("2006-01-02")
		case "Time":
			return parsed.Format("15:04:05.999999999")
		case "Datetime":
			return parsed.Format(time.RFC3339Nano)
		default:
			return value
		}
	}
	bytes, ok := value.([]byte)
	if !ok {
		return value
	}
	text := string(bytes)
	switch field.Fieldtype {
	case "Int":
		if number, err := strconv.ParseInt(text, 10, 64); err == nil {
			return number
		}
	case "Float", "Currency", "Percent":
		if number, err := numeric.Normalize(text); err == nil {
			return number
		}
	}
	return text
}

func intVal(v any) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case json.Number:
		f, _ := n.Float64()
		return int(f)
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
		if number, err := numeric.Normalize(def); err == nil {
			return number
		}
		return nil
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
	if f.Fieldtype == "Datetime" {
		switch value := v.(type) {
		case nil:
			return nil, nil
		case time.Time:
			return value, nil
		case string:
			if strings.TrimSpace(value) == "" {
				return nil, nil
			}
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"} {
				if parsed, err := time.Parse(layout, value); err == nil {
					return parsed, nil
				}
			}
			return nil, fmt.Errorf("invalid datetime")
		default:
			return nil, fmt.Errorf("invalid datetime value %T", v)
		}
	}
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
