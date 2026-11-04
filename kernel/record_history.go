package kernel

import (
	"database/sql"
	"encoding/json"
	"reflect"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// RecordFieldChange is the before/after projection stored for a record write.
// Password and hidden fields are excluded before persistence, not only at read.
type RecordFieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

func recordFieldChanges(dt *doctype.DocType, before, after *doctype.Document) []RecordFieldChange {
	changes := make([]RecordFieldChange, 0)
	for _, field := range dt.DataFields() {
		if field.Hidden || field.Fieldtype == "Password" {
			continue
		}
		var oldValue, newValue any
		if before != nil {
			oldValue = before.Get(field.Fieldname)
		}
		if after != nil {
			newValue = after.Get(field.Fieldname)
		}
		if reflect.DeepEqual(oldValue, newValue) {
			continue
		}
		changes = append(changes, RecordFieldChange{Field: field.Fieldname, Before: oldValue, After: newValue})
	}
	return changes
}

// writeRecordHistory stores field-level changes in the same transaction as
// the business mutation and operation audit. It is separate from the immutable
// operation ledger, whose payload-free design remains unchanged.
func (k *Kernel) writeRecordHistory(tx *sql.Tx, row AuditRow, dt *doctype.DocType, before, after *doctype.Document) error {
	changes, err := json.Marshal(recordFieldChanges(dt, before, after))
	if err != nil {
		return err
	}
	_, err = tx.Exec(db.Rebind(k.dialect(), `INSERT INTO _kora_record_history
		(id, site, operation_id, doctype, doc_name, changes_json)
		VALUES (?, ?, ?, ?, ?, ?)`),
		contract.NewEventID(), row.Site, row.OperationID, row.Doctype, row.DocName, string(changes))
	return err
}
