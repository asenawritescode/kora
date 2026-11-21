package orm

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// DeriveFields fills the linked and computed fields of doc and of its child
// rows in memory, children first, then the parent. old is the stored document
// when doc is an update, nil for an insert. The save runs the same steps and
// stores the result; validation runs them first so constraints read the values
// the save will store, not what the client sent.
func (tx *TxManager) DeriveFields(dt *doctype.DocType, doc, old *doctype.Document) error {
	ex, err := tx.readQueryer()
	if err != nil {
		return err
	}
	hook := tx.computedScriptHook()
	for _, f := range dt.TableFields() {
		childDT := tx.Registry.Get(f.Options)
		if childDT == nil {
			continue
		}
		children := doc.GetTable(f.Fieldname)
		if err := tx.deriveChildRows(ex, childDT, children, priorChildren(old, f.Fieldname), hook); err != nil {
			return fmt.Errorf("populating linked fields in %s: %w", f.Fieldname, err)
		}
	}
	return tx.deriveParent(ex, dt, doc, old, hook)
}

// priorChildren returns the stored rows of a child table, nil when there is no
// stored document. A row with no stored match is a new row.
func priorChildren(old *doctype.Document, fieldname string) []*doctype.Document {
	if old == nil {
		return nil
	}
	return old.GetTable(fieldname)
}

// deriveChildRows resolves the links of each child row and computes its
// fields. Rows are matched to their stored rows by name. A failing computed
// expression is logged, not returned, as the save has always done; only a
// failed lookup is an error.
func (tx *TxManager) deriveChildRows(ex db.Queryer, childDT *doctype.DocType, children, storedChildren []*doctype.Document, hook doctype.ComputedScriptHook) error {
	stored := make(map[string]*doctype.Document, len(storedChildren))
	for _, row := range storedChildren {
		stored[row.Name] = row
	}
	for _, child := range children {
		if err := tx.populateLinkedFields(ex, childDT, child, stored[child.Name]); err != nil {
			return err
		}
		if err := doctype.ComputeFieldsWithHook(childDT, child, hook); err != nil {
			slog.Warn("computed fields failed on child", "doctype", childDT.Name, "error", err)
		}
	}
	return nil
}

// deriveParent resolves the parent's links and computes its fields after its
// child rows, so a SUM over child values reads the computed amounts.
func (tx *TxManager) deriveParent(ex db.Queryer, dt *doctype.DocType, doc, old *doctype.Document, hook doctype.ComputedScriptHook) error {
	if err := tx.populateLinkedFields(ex, dt, doc, old); err != nil {
		return err
	}
	if err := doctype.ComputeFieldsWithHook(dt, doc, hook); err != nil {
		slog.Warn("computed fields failed", "doctype", dt.Name, "error", err)
	}
	return nil
}

// readQueryer is the connection a kernel operation reads through: its owner
// transaction when one is bound, otherwise the database.
func (tx *TxManager) readQueryer() (db.Queryer, error) {
	switch {
	case tx.Transaction != nil:
		return tx.Transaction, nil
	case tx.DB != nil:
		return tx.DB, nil
	}
	return nil, errors.New("no database connection to derive fields")
}
