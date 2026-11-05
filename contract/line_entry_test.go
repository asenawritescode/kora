package contract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLineEntryLayoutValidate(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"lines":{"type":"array","items":{"type":"object","properties":{"item":{"type":"string"},"unit_cost":{"type":"number"},"qty":{"type":"integer"}}}},"note":{"type":"string"}}}`)
	out := json.RawMessage(`{"type":"object","properties":{"total":{"type":"string"}}}`)
	valid := LineEntryLayout{LinesPath: "lines", LookupQuery: "p~p.catalog~item_search", LookupInput: "search", ItemMap: map[string]string{"item": "name", "unit_cost": "standard_cost"}, QuantityField: "qty", AmountExpr: "qty * unit_cost", TotalOutput: "total"}
	fields := []string{"name", "standard_cost"}
	tests := []struct {
		name   string
		mutate func(*LineEntryLayout)
		input  json.RawMessage
		want   string
	}{
		{"valid", func(*LineEntryLayout) {}, in, ""},
		{"map value not allowed", func(l *LineEntryLayout) { l.ItemMap = map[string]string{"item": "secret"} }, in, "item_map"},
		{"lines_path not array", func(l *LineEntryLayout) { l.LinesPath = "note" }, in, "lines_path"},
		{"total_output missing", func(l *LineEntryLayout) { l.TotalOutput = "nope" }, in, "total_output"},
		{"quantity not numeric", func(l *LineEntryLayout) { l.QuantityField = "item" }, in, "quantity_field"},
		{"expr non-line field", func(l *LineEntryLayout) { l.AmountExpr = "qty * note" }, in, "amount_expr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := valid
			tc.mutate(&l)
			err := l.Validate(tc.input, out, fields)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), ErrLayoutInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if validateSurfacePresentation(SurfacePresentation{Layout: "line_entry"}) == nil || validateSurfacePresentation(SurfacePresentation{LineEntry: &valid}) == nil {
		t.Fatal("shape must be rejected")
	}
}

func TestLineEntryTotalOutputResolvesInNestedCommandOutput(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"lines":{"type":"array","items":{"type":"object","properties":{"item":{"type":"string"},"qty":{"type":"integer"}}}}}}`)
	out := json.RawMessage(`{"type":"object","properties":{"output":{"type":"object","properties":{"total":{}}}}}`)
	l := LineEntryLayout{LinesPath: "lines", LookupQuery: "p~p.c~q", LookupInput: "s", ItemMap: map[string]string{"item": "name"}, QuantityField: "qty", TotalOutput: "total"}
	if err := l.Validate(in, out, []string{"name"}); err != nil {
		t.Fatal(err)
	}
	l.TotalOutput = "nope"
	if err := l.Validate(in, out, []string{"name"}); err == nil {
		t.Fatal("want error")
	}
}
