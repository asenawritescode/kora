package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
	"github.com/shopspring/decimal"
)

// A quantity effect with average_cost keeps a moving average cost on its target.
// A move that adds stock folds the cost of its rows into the target's average,
// in the same write that moves the quantity: new_avg = (max(old_qty, 0) * old_avg
// + value) / (max(old_qty, 0) + qty). Decreases, reversals and rows without a
// cost leave the average unchanged.

// costMove is what one move adds to a target's average: the quantity it adds and
// the value (quantity times unit cost) of the rows that carry a cost.
type costMove struct {
	qty, value decimal.Decimal
}

// averageMove is a move that also updates the target's average: the effect that
// made it, which names the average field, and the cost it adds.
type averageMove struct {
	effect *doctype.QuantityEffect
	cost   costMove
}

// targetMove is one change a move makes to a target record. cost is set when the
// move adds stock with a cost, so the target's average moves in the same write.
type targetMove struct {
	name   string
	change float64
	cost   *costMove
}

// averageOf is the average move a bundle item carries, if it is a target the
// kernel adjusts for a quantity effect with average_cost.
func (c OperationContext) averageOf(item RecordMutationBundleItem) (averageMove, bool) {
	if !c.isEffectItem(item.Key) {
		return averageMove{}, false
	}
	average, ok := c.effectAverages[item.Name]
	return average, ok
}

// movingAverage is the average after a move. A negative quantity on hand counts as zero.
// The caller guarantees move.qty is positive.
func movingAverage(oldQty, oldAvg decimal.Decimal, move costMove) decimal.Decimal {
	base := decimal.Max(oldQty, decimal.Zero)
	return base.Mul(oldAvg).Add(move.value).Div(base.Add(move.qty))
}

// costMoveOf sums, per target record, the quantity a move adds and its value.
// Only increases count, and only rows with a positive quantity and a cost.
// It is empty when the effect has no average_cost or the move decreases the target.
func costMoveOf(effect *doctype.QuantityEffect, doc *doctype.Document) map[string]costMove {
	moves := map[string]costMove{}
	if effect == nil || effect.AverageCost == nil {
		return moves
	}
	effect = effect.ForStatus(doc.GetString(effect.StatusField))
	if effect.Sign() < 0 {
		return moves
	}
	if effect.LinesField == "" {
		addCostRow(moves, effect, doc)
		return moves
	}
	for _, line := range doc.GetTable(effect.LinesField) {
		addCostRow(moves, effect, line)
	}
	return moves
}

// addCostRow adds one row to the move of its target. A row with a blank cost,
// or no positive quantity, adds nothing.
func addCostRow(moves map[string]costMove, effect *doctype.QuantityEffect, row *doctype.Document) {
	target := row.GetString(effect.SourceRecordField)
	amount, amountOK := numeric.Parse(row.Get(effect.DeltaField))
	cost, costOK := numeric.Parse(row.Get(effect.AverageCost.CostField))
	if target == "" || !amountOK || !costOK || !amount.IsPositive() {
		return
	}
	move := moves[target]
	move.qty = move.qty.Add(amount)
	move.value = move.value.Add(amount.Mul(cost))
	moves[target] = move
}

// createEffectAverages gives each target a new record moves the average of, for
// the bundle that carries the record's stock change. It reads the stored form of
// the record, so a reason-signed amount is already signed.
func createEffectAverages(reg *doctype.Registry, dt *doctype.DocType, data json.RawMessage) map[string]averageMove {
	effect := dt.QuantityEffect
	if effect == nil || effect.AverageCost == nil {
		return nil
	}
	doc := doctype.NewDocument(dt.Name)
	if err := applyData(dt, doc, data, reg, false); err != nil {
		return nil // the create itself reports the malformed data
	}
	averages := map[string]averageMove{}
	for targetName, cost := range costMoveOf(effect, doc) {
		averages[targetName] = averageMove{effect: effect, cost: cost}
	}
	return averages
}

// targetAdjust is the adjust a move writes to its target: the quantity change,
// with the new average when the move carries a cost.
func targetAdjust(target *doctype.DocType, oldDoc *doctype.Document, effect *doctype.QuantityEffect, move targetMove) ([]byte, error) {
	data, err := json.Marshal(map[string]float64{effect.TargetQuantityField: move.change})
	if err != nil {
		return nil, errors.New("encoding quantity effect failed")
	}
	adjusted, err := adjustQuantity(target, oldDoc, "adjust", data)
	if err != nil || move.cost == nil {
		return adjusted, err
	}
	return averageAdjust(target, oldDoc, effect, adjusted, *move.cost)
}

// averageAdjust adds the target's new average to an adjust's values. The average
// is rounded to the average field's precision when it has one.
func averageAdjust(target *doctype.DocType, oldDoc *doctype.Document, effect *doctype.QuantityEffect, adjusted []byte, cost costMove) ([]byte, error) {
	field := target.GetField(effect.AverageCost.TargetField)
	if field == nil {
		return nil, fmt.Errorf("quantity_effect average_cost target_field %q does not exist on %s", effect.AverageCost.TargetField, target.Name)
	}
	decoder := json.NewDecoder(bytes.NewReader(adjusted))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("read quantity adjustment: %w", err)
	}
	oldQty, _ := numeric.Parse(oldDoc.Get(effect.TargetQuantityField))
	oldAvg, _ := numeric.Parse(oldDoc.Get(field.Fieldname))
	next := movingAverage(oldQty, oldAvg, cost)
	if field.Precision != nil {
		next = next.Round(int32(*field.Precision))
	}
	values[field.Fieldname] = numeric.Number(next)
	return json.Marshal(values)
}
