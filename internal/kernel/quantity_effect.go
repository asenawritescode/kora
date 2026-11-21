package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// prepareQuantityEffect makes a record's own quantity_effect rule happen
// whichever way the record is created.
//
// A record.create for a record type that declares an effect is rewritten, before
// authorization, into a record.mutate_bundle: the original create, followed by
// one "adjust" per affected target record. The
// bundle already runs as a single all-or-nothing transaction with idempotency,
// audit and outbox, so nothing new has to be made safe. The target quantity changes are
// authorized by the permission to create the source record, not by write
// permission on the target.
//
// A gated effect (status_field and apply_in_statuses) is rewritten only when the
// status the record is created with is one that applies it. Later status changes
// are handled where the record is written (see applyStatusEffect).
//
// Records without an effect, and every command other than record.create, pass
// through unchanged.
func prepareQuantityEffect(reg *doctype.Registry, op *Operation) *contract.Error {
	if reg == nil || op == nil || op.Command != CommandRecordCreate {
		return nil
	}
	var create RecordCreatePayload
	if err := json.Unmarshal(op.Payload, &create); err != nil || create.PublicRoute != "" {
		return nil
	}
	dt := reg.Get(create.Doctype)
	if dt == nil || !dt.QuantityEffect.Complete() {
		return nil
	}
	effect := dt.QuantityEffect
	data, ok := decodeCreateData(create.Data)
	if !ok {
		return nil // the normal create path reports malformed data
	}
	applies, cerr := createStatusApplies(dt, data)
	if cerr != nil {
		return cerr
	}
	if !applies {
		return nil
	}
	totals, normalised, cerr := createEffectTotals(dt, effect, data)
	if cerr != nil {
		return cerr
	}
	if normalised {
		signed, err := json.Marshal(data)
		if err != nil {
			return contract.NewError(contract.CodeInternal, "encoding quantity effect failed")
		}
		create.Data = signed
	}
	targetNames := nonzeroEffectTargets(totals)
	if len(targetNames) == 0 {
		return nil
	}
	if len(targetNames)+1 > MaxMutationBundleRecords {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s affects too many different records in one save (limit %d)", dt.Name, MaxMutationBundleRecords-1))
	}
	return rewriteToEffectBundle(op, dt, create.Data, totals, targetNames)
}

// decodeCreateData reads the data of a create. Empty data is an empty record;
// data that is not a JSON object is reported as not decodable.
func decodeCreateData(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, false
	}
	return data, true
}

// createEffectTotals sums the signed change of a new record per target record.
// normalised reports that a whole-record amount was signed by its reason, so the
// stored record must carry the signed number.
func createEffectTotals(dt *doctype.DocType, effect *doctype.QuantityEffect, data map[string]any) (totals map[string]float64, normalised bool, cerr *contract.Error) {
	totals = map[string]float64{}
	if effect.LinesField == "" {
		normalised, cerr = addEffectRow(dt, effect, data, true, totals)
		return totals, normalised, cerr
	}
	rows, _ := data[effect.LinesField].([]any)
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return nil, false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s lines must be objects", dt.Name))
		}
		if _, cerr = addEffectRow(dt, effect, row, false, totals); cerr != nil {
			return nil, false, cerr
		}
	}
	return totals, false, nil
}

// addEffectRow adds one row's signed change to totals. A whole record's amount
// may be signed by its reason; the signed amount is written back to the row so
// the stored record shows it. It reports whether that happened.
func addEffectRow(dt *doctype.DocType, effect *doctype.QuantityEffect, row map[string]any, whole bool, totals map[string]float64) (bool, *contract.Error) {
	targetName, _ := row[effect.SourceRecordField].(string)
	amount, ok := quantityNumber(row[effect.DeltaField])
	if targetName == "" || !ok || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s needs a linked record in %s and a number in %s", dt.Name, effect.SourceRecordField, effect.DeltaField))
	}
	if amount == 0 {
		if whole {
			return false, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s needs a nonzero number in %s", dt.Name, effect.DeltaField))
		}
		return false, nil
	}
	// The chosen reason can decide the direction. The person types a plain
	// amount; the signed number is stored on the record so its history is true.
	signed := false
	if whole {
		reason, _ := row[effect.ReasonField].(string)
		if sign := effect.ReasonSign(reason); sign != 0 {
			amount = sign * math.Abs(amount)
			row[effect.DeltaField] = amount
			signed = true
		}
	}
	totals[targetName] += amount * effect.Sign()
	return signed, nil
}

// nonzeroEffectTargets lists the target records whose total changes, sorted: one
// lock order for every writer, and a stable payload for idempotent replay.
func nonzeroEffectTargets(totals map[string]float64) []string {
	targetNames := make([]string, 0, len(totals))
	for targetName, total := range totals {
		if total != 0 {
			targetNames = append(targetNames, targetName)
		}
	}
	sort.Strings(targetNames)
	return targetNames
}

// rewriteToEffectBundle turns the operation into a record.mutate_bundle: the
// create, then one adjust per target record. It marks the adjust items so they
// are authorized by the create.
func rewriteToEffectBundle(op *Operation, dt *doctype.DocType, data json.RawMessage, totals map[string]float64, targetNames []string) *contract.Error {
	effect := dt.QuantityEffect
	records := []RecordMutationBundleItem{{Key: "record", Doctype: dt.Name, Data: data}}
	effectKeys := make(map[string]struct{}, len(targetNames))
	for i, targetName := range targetNames {
		change, _ := json.Marshal(map[string]float64{effect.TargetQuantityField: totals[targetName]})
		key := "effect_" + strconv.Itoa(i)
		effectKeys[key] = struct{}{}
		records = append(records, RecordMutationBundleItem{Key: key, Operation: "adjust", Doctype: effect.TargetDocType, Name: targetName, Data: change})
	}
	payload, err := json.Marshal(RecordMutationBundlePayload{Doctype: dt.Name, Records: records})
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding quantity effect failed")
	}
	op.Command = CommandRecordMutateBundle
	op.Payload = payload
	op.Context.effectItems = effectKeys
	return nil
}

// quantityNumber reads a JSON number, or text that holds one.
func quantityNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}
