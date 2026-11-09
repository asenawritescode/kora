package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// prepareQuantityEffect makes a record's own quantity_effect rule happen
// whichever way the record is created.
//
// A record.create for a record type that declares an effect is rewritten, before
// authorization, into a record.mutate_bundle: the original create, followed by
// one "adjust" per affected target record (for example one per product). The
// bundle already runs as a single all-or-nothing transaction with idempotency,
// audit and outbox, so nothing new has to be made safe. The stock changes are
// authorized by the permission to create the source record, not by write
// permission on the target.
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
	var data map[string]any
	if len(create.Data) > 0 {
		if err := json.Unmarshal(create.Data, &data); err != nil {
			return nil // the normal create path reports malformed data
		}
	}

	totals := map[string]float64{}
	normalised := false
	add := func(row map[string]any, whole bool) *contract.Error {
		product, _ := row[effect.SourceRecordField].(string)
		amount, ok := quantityNumber(row[effect.DeltaField])
		if product == "" || !ok || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s needs a linked record in %s and a number in %s", dt.Name, effect.SourceRecordField, effect.DeltaField))
		}
		if amount == 0 {
			if whole {
				return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s needs a nonzero number in %s", dt.Name, effect.DeltaField))
			}
			return nil
		}
		// The chosen reason can decide the direction. The person types a plain
		// amount; the signed number is stored on the record so its history is true.
		if reason, _ := row[effect.ReasonField].(string); !whole {
			// lines never use reason_directions
		} else if sign := effect.ReasonSign(reason); sign != 0 {
			amount = sign * math.Abs(amount)
			row[effect.DeltaField] = amount
			normalised = true
		}
		totals[product] += amount * effect.Sign()
		return nil
	}
	if effect.LinesField == "" {
		if err := add(data, true); err != nil {
			return err
		}
	} else {
		rows, _ := data[effect.LinesField].([]any)
		for _, raw := range rows {
			row, ok := raw.(map[string]any)
			if !ok {
				return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s lines must be objects", dt.Name))
			}
			if err := add(row, false); err != nil {
				return err
			}
		}
	}

	if normalised {
		signed, err := json.Marshal(data)
		if err != nil {
			return contract.NewError(contract.CodeInternal, "encoding quantity effect failed")
		}
		create.Data = signed
	}
	products := make([]string, 0, len(totals))
	for product, total := range totals {
		if total != 0 {
			products = append(products, product)
		}
	}
	if len(products) == 0 {
		return nil
	}
	sort.Strings(products) // one lock order for every writer, and a stable payload for idempotent replay
	if len(products)+1 > MaxMutationBundleRecords {
		return contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("%s affects too many different records in one save (limit %d)", dt.Name, MaxMutationBundleRecords-1))
	}

	records := []RecordMutationBundleItem{{Key: "record", Doctype: dt.Name, Data: create.Data}}
	effectKeys := make(map[string]struct{}, len(products))
	for i, product := range products {
		change, _ := json.Marshal(map[string]float64{effect.TargetQuantityField: totals[product]})
		key := "effect_" + strconv.Itoa(i)
		effectKeys[key] = struct{}{}
		records = append(records, RecordMutationBundleItem{Key: key, Operation: "adjust", Doctype: effect.TargetDocType, Name: product, Data: change})
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
