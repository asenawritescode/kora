package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestAdjustQuantityUsesExactDecimals(t *testing.T) {
	dt := &doctype.DocType{Name: "Stock", Fields: []doctype.Field{
		{Fieldname: "qty", Fieldtype: "Float"},
		{Fieldname: "count", Fieldtype: "Int"},
		{Fieldname: "balance", Fieldtype: "Currency"},
		{Fieldname: "label", Fieldtype: "Data"},
	}}
	old := doctype.NewDocument("Stock")
	old.Set("qty", json.Number("0.3"))
	old.Set("count", int64(5))
	old.Set("balance", json.Number("100.10"))

	tests := []struct {
		name      string
		operation string
		data      string
		want      string
		wantErr   bool
	}{
		{"float decrement has no binary noise", "decrement", `{"qty":0.1}`, `{"qty":0.2}`, false},
		{"currency adjust keeps cents", "adjust", `{"balance":"-0.05"}`, `{"balance":100.05}`, false},
		{"int decrement stays integer", "decrement", `{"count":2}`, `{"count":3}`, false},
		{"int rejects fractions", "decrement", `{"count":0.5}`, "", true},
		{"cannot go negative", "decrement", `{"qty":1}`, "", true},
		{"decrement rejects negative amount", "decrement", `{"qty":-1}`, "", true},
		{"zero rejected", "adjust", `{"qty":0}`, "", true},
		{"non numeric field rejected", "adjust", `{"label":1}`, "", true},
		{"exactly one field", "adjust", `{"qty":1,"count":1}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adjustQuantity(dt, old, tt.operation, []byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
