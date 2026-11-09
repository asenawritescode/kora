package kernel

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
)

var errInvalidQuantity = errors.New("invalid or insufficient quantity")

// adjustQuantity applies a decrement or adjust delta to one numeric field using
// exact decimal arithmetic and returns the replacement data payload.
func adjustQuantity(recordDT *doctype.DocType, oldDoc *doctype.Document, operation string, data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var deltas map[string]any
	if err := decoder.Decode(&deltas); err != nil || len(deltas) != 1 {
		return nil, errors.New("quantity adjustment requires one numeric field")
	}
	for field, raw := range deltas {
		definition := recordDT.GetField(field)
		if definition == nil || (definition.Fieldtype != "Int" && definition.Fieldtype != "Float" && definition.Fieldtype != "Currency") {
			return nil, errInvalidQuantity
		}
		amount, ok := numeric.Parse(raw)
		if !ok || amount.IsZero() || definition.Fieldtype == "Int" && !amount.IsInteger() || operation == "decrement" && amount.IsNegative() {
			return nil, errInvalidQuantity
		}
		current, _ := numeric.Parse(oldDoc.Get(field))
		next := current.Add(amount)
		if operation == "decrement" {
			next = current.Sub(amount)
		}
		if next.IsNegative() {
			return nil, errInvalidQuantity
		}
		var value any = numeric.Number(next)
		if definition.Fieldtype == "Int" {
			value = next.IntPart()
		}
		return json.Marshal(map[string]any{field: value})
	}
	return nil, errInvalidQuantity
}
