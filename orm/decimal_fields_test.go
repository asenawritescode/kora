package orm

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestCanonicalizeDecimal(t *testing.T) {
	currency := doctype.Field{Fieldname: "amount", Fieldtype: "Currency"}
	tests := []struct {
		name     string
		field    doctype.Field
		in       any
		wantDoc  any
		wantBind any
		wantErr  bool
	}{
		{"float input becomes exact text", currency, 0.1, json.Number("0.1"), "0.1", false},
		{"numeric string", currency, "1250.50", json.Number("1250.5"), "1250.5", false},
		{"three decimals survive", doctype.Field{Fieldname: "qty", Fieldtype: "Float"}, 0.125, json.Number("0.125"), "0.125", false},
		{"empty clears", currency, "", nil, nil, false},
		{"garbage rejected", currency, "ten", nil, nil, true},
		{"non decimal untouched", doctype.Field{Fieldname: "n", Fieldtype: "Int"}, int64(3), int64(3), int64(3), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := doctype.NewDocument("Thing")
			doc.Set(tt.field.Fieldname, tt.in)
			bind, err := canonicalizeDecimal(doc, tt.field, tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr {
				return
			}
			if bind != tt.wantBind {
				t.Fatalf("bind = %#v, want %#v", bind, tt.wantBind)
			}
			if got := doc.Get(tt.field.Fieldname); got != tt.wantDoc {
				t.Fatalf("doc value = %#v, want %#v", got, tt.wantDoc)
			}
		})
	}
}

func TestDecimalColumnsReadExactly(t *testing.T) {
	f := &doctype.Field{Fieldname: "amount", Fieldtype: "Currency"}
	if got := normalizeScannedFieldValue(f, []byte("1000000000000.123456789")); got != json.Number("1000000000000.123456789") {
		t.Fatalf("scanned = %#v", got)
	}
	if got := normalizeScannedFieldValue(f, []byte("19.990000000")); got != json.Number("19.99") {
		t.Fatalf("scanned = %#v", got)
	}
}

func TestDecimalDefaultsAreExact(t *testing.T) {
	if got := convertDefault("0.1", "Currency"); got != json.Number("0.1") {
		t.Fatalf("default = %#v", got)
	}
	if got := convertDefault("not-a-number", "Float"); got != nil {
		t.Fatalf("invalid default = %#v, want nil", got)
	}
}
