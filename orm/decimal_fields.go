package orm

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/numeric"
)

// canonicalizeDecimal rewrites a Float, Currency or Percent value on doc to its
// exact json.Number form and returns the SQL bind value. Other field types are
// returned unchanged.
func canonicalizeDecimal(doc *doctype.Document, f doctype.Field, val any) (any, error) {
	if !numeric.IsDecimalFieldtype(f.Fieldtype) {
		return val, nil
	}
	normalized, err := numeric.Normalize(val)
	if err != nil {
		return nil, fmt.Errorf("field %s: %w", f.Fieldname, err)
	}
	if f.Precision != nil && normalized != nil {
		exact, _ := numeric.Parse(normalized)
		normalized = numeric.Number(exact.Round(int32(*f.Precision)))
	}
	if doc != nil {
		doc.Set(f.Fieldname, normalized)
	}
	return decimalBindValue(normalized), nil
}

// decimalBindValue passes decimals to the driver as text so DECIMAL and NUMERIC
// columns receive the exact value instead of a float64 approximation.
func decimalBindValue(v any) any {
	if n, ok := v.(json.Number); ok {
		return string(n)
	}
	return v
}
