package script

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/internal/db"
)

const maxLifecycleInventoryRows = 10000

// LifecycleInventoryError exposes only a stable code and fixed table name.
// The underlying database error may contain deployment details and is never
// serialized by the inventory command.
type LifecycleInventoryError struct {
	Code  string
	Table string
	Cause error
}

func (e *LifecycleInventoryError) Error() string {
	if e == nil {
		return "lifecycle script inventory error"
	}
	return fmt.Sprintf("lifecycle script inventory %s for %s", e.Code, e.Table)
}

func (e *LifecycleInventoryError) Unwrap() error { return e.Cause }

// LifecycleInventory contains aggregate counts only. It never returns script
// names, source, doctypes, or tenant-defined event values.
type LifecycleInventory struct {
	Complete                     bool             `json:"complete"`
	RowsInspected                int              `json:"rows_inspected"`
	DocumentEventScripts         int64            `json:"document_event_scripts"`
	ActiveDocumentEventScripts   int64            `json:"active_document_event_scripts"`
	InactiveDocumentEventScripts int64            `json:"inactive_document_event_scripts"`
	ActiveAfterEventScripts      int64            `json:"active_after_event_scripts"`
	ActiveAfterEventByEvent      map[string]int64 `json:"active_after_event_by_event,omitempty"`
	UnknownDocumentEventScripts  int64            `json:"unknown_document_event_scripts"`
	OtherScriptTypes             int64            `json:"other_script_types"`
}

// InspectLifecycleInventory performs a bounded, read-only scan of script
// metadata for one tenant. It deliberately does not select names or source.
func InspectLifecycleInventory(ctx context.Context, database *sql.DB, dialect db.Dialect, siteName string) (LifecycleInventory, error) {
	out := LifecycleInventory{Complete: true, ActiveAfterEventByEvent: map[string]int64{}}
	if ctx == nil || database == nil || dialect == nil || siteName == "" {
		return LifecycleInventory{Complete: false}, errors.New("lifecycle inventory requires context, database, dialect, and tenant")
	}
	const table = "_kora_script"
	query := db.Rebind(dialect, "SELECT script_type, event, is_active FROM _kora_script WHERE site = ? LIMIT ?")
	rows, err := database.QueryContext(ctx, query, siteName, maxLifecycleInventoryRows+1)
	if err != nil {
		return failedLifecycleInventory(out, "query_failed", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		if out.RowsInspected == maxLifecycleInventoryRows {
			return failedLifecycleInventory(out, "row_limit_exceeded", table, nil)
		}
		var scriptType, event string
		var active bool
		if err := rows.Scan(&scriptType, &event, &active); err != nil {
			return failedLifecycleInventory(out, "read_failed", table, err)
		}
		out.RowsInspected++
		if scriptType != string(TypeDocEvent) {
			out.OtherScriptTypes++
			continue
		}
		out.DocumentEventScripts++
		knownEvent := knownLifecycleEvent(Event(event))
		if !knownEvent {
			out.UnknownDocumentEventScripts++
		}
		if !active {
			out.InactiveDocumentEventScripts++
			continue
		}
		out.ActiveDocumentEventScripts++
		if !knownEvent {
			continue
		}
		if IsAfterEvent(Event(event)) {
			out.ActiveAfterEventScripts++
			out.ActiveAfterEventByEvent[event]++
		}
	}
	if err := rows.Err(); err != nil {
		return failedLifecycleInventory(out, "read_failed", table, err)
	}
	if err := rows.Close(); err != nil {
		return failedLifecycleInventory(out, "read_failed", table, err)
	}
	if len(out.ActiveAfterEventByEvent) == 0 {
		out.ActiveAfterEventByEvent = nil
	}
	return out, nil
}

func failedLifecycleInventory(out LifecycleInventory, code, table string, cause error) (LifecycleInventory, error) {
	out.Complete = false
	out.ActiveAfterEventByEvent = nil
	return out, &LifecycleInventoryError{Code: code, Table: table, Cause: cause}
}

func knownLifecycleEvent(event Event) bool {
	switch event {
	case EventBeforeInsert, EventAfterInsert, EventBeforeSave, EventAfterSave,
		EventBeforeDelete, EventAfterDelete, EventBeforeSubmit, EventAfterSubmit,
		EventBeforeCancel, EventAfterCancel, EventValidate, EventComputed, EventPayment:
		return true
	default:
		return false
	}
}
