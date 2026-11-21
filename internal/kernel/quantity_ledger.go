package kernel

import (
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
)

// A quantity effect with a ledger appends one row to the ledger record type for
// every target record it moves, in the transaction of the write that moved it.
// The row is authorized by that write, so it needs no create permission on the
// ledger, and it is written by the same kernel path, so a replay that returns
// the stored result writes nothing again.

// moveCause is the record whose save moved stock: its type, its name, and the
// reason the ledger shows for the move.
type moveCause struct {
	dt     *doctype.DocType
	name   string
	reason string
}

// ledgerReason is the reason a ledger row shows: the effect's reason field when
// it names one, otherwise the status that applies the effect.
func ledgerReason(effect *doctype.QuantityEffect, doc *doctype.Document) string {
	if effect.ReasonField != "" {
		return doc.GetString(effect.ReasonField)
	}
	if effect.Gated() {
		return doc.GetString(effect.StatusField)
	}
	return ""
}

// appendLedger writes one ledger row for a move of change to product, which
// leaves the target at balance. It does nothing for an effect without a ledger.
func (w effectWriter) appendLedger(cause moveCause, product string, change, balance float64) *contract.Error {
	ledgerConfig := cause.dt.QuantityEffect.Ledger
	if ledgerConfig == nil {
		return nil
	}
	ledger := w.run.Reg.Get(ledgerConfig.Doctype)
	if ledger == nil {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("quantity_effect ledger doctype %q does not exist", ledgerConfig.Doctype))
	}
	row := doctype.NewDocument(ledger.Name)
	row.Set(ledgerConfig.TargetField, product)
	row.Set(ledgerConfig.ChangeField, change)
	row.Set(ledgerConfig.BalanceField, balance)
	row.Set(ledgerConfig.SourceDoctypeField, cause.dt.Name)
	row.Set(ledgerConfig.SourceNameField, cause.name)
	if ledgerConfig.ReasonField != "" {
		row.Set(ledgerConfig.ReasonField, cause.reason)
	}
	if err := w.run.Mgr.InsertInTx(w.run.Tx, ledger, row, w.owner, w.user); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapORMError(err, w.k.dialect())
	}
	return nil
}

// quantityAfter reads a target's quantity field from a saved document.
func quantityAfter(doc *doctype.Document, field string) float64 {
	amount, _ := numeric.Parse(doc.Get(field))
	return amount.InexactFloat64()
}

// effectLedger writes the ledger rows of a record whose quantity effect the
// kernel turned into bundle adjust items (see prepareQuantityEffect). The adjust
// items are written before this runs, so each balance is read from its item.
func (m *mutation) effectLedger(b *bundle) *contract.Error {
	if len(m.op.Context.effectItems) == 0 || len(b.docs) == 0 {
		return nil
	}
	source, sourceDT := b.docs[0], b.types[0]
	effect := sourceDT.QuantityEffect
	if effect == nil || effect.Ledger == nil {
		return nil
	}
	balances := map[string]float64{}
	for i := 1; i < len(b.docs); i++ {
		if m.op.Context.isEffectItem(b.payload.Records[i].Key) && b.types[i].Name == effect.TargetDocType {
			balances[b.docs[i].Name] = quantityAfter(b.docs[i], effect.TargetQuantityField)
		}
	}
	w := m.effects()
	cause := moveCause{dt: sourceDT, name: source.Name, reason: ledgerReason(effect, source)}
	totals := effectTotalsOf(effect, source)
	for _, product := range nonzeroEffectProducts(totals) {
		balance, ok := balances[product]
		if !ok {
			return contract.NewError(contract.CodeInternal, "quantity effect target was not written")
		}
		if cerr := w.appendLedger(cause, product, totals[product], balance); cerr != nil {
			return cerr
		}
	}
	return nil
}
