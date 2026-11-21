package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
	"github.com/asenawritescode/kora/internal/orm"
)

// A gated quantity effect (status_field and apply_in_statuses) changes stock only
// while its record's status is in apply_in_statuses. The kernel moves stock in
// the transaction of the write that changes the status: entering the set applies
// the record's lines, leaving it reverses the lines as they were stored, and a
// save that keeps the status inside the set may not change the lines.

// createStatusApplies reports whether a new record applies its gated effect. The
// status is the one sent, or the field default when none is sent; a gated create
// with neither is refused rather than left silently out of stock.
func createStatusApplies(dt *doctype.DocType, data map[string]any) (bool, *contract.Error) {
	effect := dt.QuantityEffect
	if !effect.Gated() {
		return true, nil
	}
	status, _ := data[effect.StatusField].(string)
	if status == "" {
		if field := dt.GetField(effect.StatusField); field != nil {
			status = field.Default
		}
	}
	if status == "" {
		return false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s needs %s to decide whether it changes stock", dt.Name, effect.StatusField))
	}
	return effect.AppliesIn(status), nil
}

// effectWriter is the write path a gated effect runs in. The record-command path
// (mutation) and a pack command (commandWriter) each supply their transaction,
// operation and the way they save one stock record in it, so the status rules
// and the stock moves below are written once.
type effectWriter struct {
	k   *Kernel
	run txRun
	op  Operation
	// writeTarget saves a stock record the effect changed, with its audit rows
	// and its completion after commit. oldDoc is the stored record.
	writeTarget func(ctx context.Context, target *doctype.DocType, doc, oldDoc *doctype.Document) *contract.Error
}

// effects returns the effect writer of a record-command mutation.
func (m *mutation) effects() effectWriter {
	return effectWriter{k: m.k, run: m.run, op: m.op, writeTarget: m.writeEffectTarget}
}

// writeEffectTarget saves a stock record a mutation moved and tracks it so its
// after-commit completion runs with the bundle's.
func (m *mutation) writeEffectTarget(ctx context.Context, target *doctype.DocType, doc, oldDoc *doctype.Document) *contract.Error {
	beforeHash, cerr := m.writeItem(ctx, target, doc, oldDoc)
	if cerr != nil {
		return cerr
	}
	if _, auditErr := m.auditItem(target, doc, oldDoc, beforeHash); auditErr != nil {
		return auditErr
	}
	m.bundleSavedDocs = append(m.bundleSavedDocs, savedBundleDocument{dt: target, doc: doc, oldDoc: oldDoc})
	return nil
}

// applyStatusEffect moves stock for a gated record that a write has just saved.
// oldDoc is the stored record (nil for a new one), doc the one written. A new
// record created inside a bundle (a register checkout) moves stock here; a
// single create was already rewritten to carry its stock change (effectItems).
func (m *mutation) applyStatusEffect(ctx context.Context, dt *doctype.DocType, oldDoc, doc *doctype.Document) *contract.Error {
	return applyGatedEffect(ctx, m.effects(), dt, oldDoc, doc)
}

// applyGatedEffect applies the status rule of dt to a record the write w has
// just saved: entering the applying set moves stock by its lines, leaving it
// reverses them as stored, and a save that stays inside it may not change them.
func applyGatedEffect(ctx context.Context, w effectWriter, dt *doctype.DocType, oldDoc, doc *doctype.Document) *contract.Error {
	effect := dt.QuantityEffect
	if !effect.Gated() || (oldDoc == nil && len(w.op.Context.effectItems) > 0) {
		return nil
	}
	if oldDoc == nil {
		oldDoc = doctype.NewDocument(dt.Name)
	}
	wasIn := effect.AppliesIn(oldDoc.GetString(effect.StatusField))
	isIn := effect.AppliesIn(doc.GetString(effect.StatusField))
	switch {
	case wasIn && isIn:
		return refuseLineChange(dt, oldDoc, doc, effect)
	case isIn:
		return moveStock(ctx, w, dt, doc, 1)
	case wasIn:
		return moveStock(ctx, w, dt, oldDoc, -1)
	}
	return nil
}

// refuseLineChange rejects a save that keeps the status inside the applying set
// while its lines (the stock they move) change.
func refuseLineChange(dt *doctype.DocType, oldDoc, doc *doctype.Document, effect *doctype.QuantityEffect) *contract.Error {
	if sameEffectTotals(effectTotalsOf(effect, oldDoc), effectTotalsOf(effect, doc)) {
		return nil
	}
	return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s lines cannot change after it is %s", dt.Name, doc.GetString(effect.StatusField)))
}

// moveStock applies the stored change of source, scaled by dir (+1 applies it,
// -1 reverses it), to every target record it touches, in product order.
func moveStock(ctx context.Context, w effectWriter, dt *doctype.DocType, source *doctype.Document, dir float64) *contract.Error {
	totals := effectTotalsOf(dt.QuantityEffect, source)
	for _, product := range nonzeroEffectProducts(totals) {
		if cerr := moveTarget(ctx, w, dt.QuantityEffect, product, dir*totals[product]); cerr != nil {
			return cerr
		}
	}
	return nil
}

// effectTotalsOf sums the stored change of a record per target record, with the
// effect's direction applied. Blank and zero rows add nothing.
func effectTotalsOf(effect *doctype.QuantityEffect, doc *doctype.Document) map[string]float64 {
	totals := map[string]float64{}
	if effect.LinesField == "" {
		addStoredChange(totals, effect, doc.GetString(effect.SourceRecordField), doc.Get(effect.DeltaField))
		return totals
	}
	for _, line := range doc.GetTable(effect.LinesField) {
		addStoredChange(totals, effect, line.GetString(effect.SourceRecordField), line.Get(effect.DeltaField))
	}
	return totals
}

func addStoredChange(totals map[string]float64, effect *doctype.QuantityEffect, product string, value any) {
	amount, ok := numeric.Parse(value)
	if product == "" || !ok || amount.IsZero() {
		return
	}
	totals[product] += amount.InexactFloat64() * effect.Sign()
}

// sameEffectTotals reports whether two per-record totals hold the same changes.
func sameEffectTotals(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for product, amount := range a {
		if other, ok := b[product]; !ok || other != amount {
			return false
		}
	}
	return true
}

// moveTarget applies one signed change to a target record inside the transaction
// of the write that caused it. It is checked and written like a bundle adjust
// item, and it is authorized by the write of the source record, as a create is.
func moveTarget(ctx context.Context, w effectWriter, effect *doctype.QuantityEffect, product string, change float64) *contract.Error {
	reg, txMgr := w.run.Reg, w.run.Mgr
	target := reg.Get(effect.TargetDocType)
	if target == nil {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("quantity_effect target_doctype %q does not exist", effect.TargetDocType))
	}
	// Lock the stock row before reading it, so two sales moving the same stock
	// take turns instead of both writing from the same stale quantity.
	if lockErr := lockRecordRow(w.run.Tx, w.k.dialect(), target, product); lockErr != nil && !errors.Is(lockErr, sql.ErrNoRows) {
		return contract.NewError(contract.CodeDependencyUnavailable, "lock stock record failed")
	}
	oldDoc, loadErr := txMgr.GetDocWithScope(target, product, scopeForOperation(reg, w.op, target.Name, "write")) //nolint:contextcheck // the context travels in txMgr.Context
	if loadErr != nil {
		if errors.Is(loadErr, orm.ErrNotFound) {
			return contract.NewError(contract.CodeNotFound, "document not found")
		}
		return contract.NewError(contract.CodeDependencyUnavailable, "load stock record failed")
	}
	data, err := json.Marshal(map[string]float64{effect.TargetQuantityField: change})
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding quantity effect failed")
	}
	adjusted, adjustErr := adjustQuantity(target, oldDoc, "adjust", data)
	if adjustErr != nil {
		return contract.NewError(contract.CodeValidationFailed, adjustErr.Error())
	}
	doc := cloneDoc(oldDoc)
	if applyErr := applyData(target, doc, adjusted, reg, false); applyErr != nil {
		return contract.NewError(contract.CodeValidationFailed, applyErr.Error())
	}
	doc.IsNew = false
	if hookErr := txMgr.RunHooksForValidate(target, doc, oldDoc); hookErr != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return contract.NewError(contract.CodeValidationFailed, hookErr.Error())
	}
	resolver := func(linked *doctype.DocType, name string) (*doctype.Document, error) { //nolint:contextcheck // the context travels in txMgr.Context
		return txMgr.GetDoc(linked, name, "")
	}
	if verrs := doctype.ValidateDocumentWithResolvers(target, doc, reg, oldDoc, resolver, w.op.Context.BusinessReferenceValidator, w.op.Context.HistoricalSnapshotValidator); verrs.HasErrors() {
		return validationContractError(verrs)
	}
	if prepErr := txMgr.PrepareSave(target, doc, oldDoc); prepErr != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return contract.NewError(contract.CodeValidationFailed, prepErr.Error())
	}
	return w.writeTarget(ctx, target, doc, oldDoc)
}
