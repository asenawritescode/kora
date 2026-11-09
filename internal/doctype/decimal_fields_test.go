package doctype

import (
	"strings"
	"testing"
)

func TestValidateDecimalFields(t *testing.T) {
	two, ten, minus := 2, 10, -1
	tests := []struct {
		name   string
		fields []Field
		err    string
	}{
		{"currency with precision and currency field", []Field{{Fieldname: "currency", Fieldtype: "Link", Options: "Currency"}, {Fieldname: "total", Fieldtype: "Currency", Options: "currency", Precision: &two}}, ""},
		{"currency without options uses tenant default", []Field{{Fieldname: "total", Fieldtype: "Currency"}}, ""},
		{"precision on Int rejected", []Field{{Fieldname: "qty", Fieldtype: "Int", Precision: &two}}, "applies only"},
		{"precision above storage scale", []Field{{Fieldname: "rate", Fieldtype: "Float", Precision: &ten}}, "between 0 and 9"},
		{"negative precision", []Field{{Fieldname: "rate", Fieldtype: "Percent", Precision: &minus}}, "between 0 and 9"},
		{"unknown currency field", []Field{{Fieldname: "total", Fieldtype: "Currency", Options: "ccy"}}, "does not exist"},
		{"currency field must hold a code", []Field{{Fieldname: "ccy", Fieldtype: "Int"}, {Fieldname: "total", Fieldtype: "Currency", Options: "ccy"}}, "Link, Data or Select"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := &DocType{Name: "Invoice", Fields: tt.fields}
			err := dt.validateDecimalFields()
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
		})
	}
}
