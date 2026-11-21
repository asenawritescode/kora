package doctype

import "testing"

// averageTypes is a receipt that moves Product stock through lines, with a
// cost_price on Product and a unit_cost on each line for the moving average.
func averageTypes(average *AverageCost, lineCost string) []*DocType {
	product := &DocType{Name: "Product", Fields: []Field{
		{Fieldname: "stock_qty", Fieldtype: "Float"},
		{Fieldname: "cost_price", Fieldtype: "Float"},
		{Fieldname: "label", Fieldtype: "Data"},
	}}
	line := &DocType{Name: "Receipt Line", IsChildTable: true, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "unit_cost", Fieldtype: lineCost},
	}}
	effect := &QuantityEffect{
		TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
		SourceRecordField: "product", DeltaField: "quantity", Direction: "increase", AverageCost: average,
	}
	receipt := &DocType{Name: "Receipt", QuantityEffect: effect, Fields: []Field{
		{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"},
	}}
	return []*DocType{product, line, receipt}
}

func TestQuantityAverageCostIsChecked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		average  *AverageCost
		lineCost string
		wantErr  bool
	}{
		{"valid", &AverageCost{TargetField: "cost_price", CostField: "unit_cost"}, "Float", false},
		{"target field missing", &AverageCost{TargetField: "", CostField: "unit_cost"}, "Float", true},
		{"target field does not exist", &AverageCost{TargetField: "avg", CostField: "unit_cost"}, "Float", true},
		{"target field is text", &AverageCost{TargetField: "label", CostField: "unit_cost"}, "Float", true},
		{"target field is the quantity", &AverageCost{TargetField: "stock_qty", CostField: "unit_cost"}, "Float", true},
		{"cost field missing", &AverageCost{TargetField: "cost_price", CostField: ""}, "Float", true},
		{"cost field does not exist", &AverageCost{TargetField: "cost_price", CostField: "price"}, "Float", true},
		{"cost field is text", &AverageCost{TargetField: "cost_price", CostField: "unit_cost"}, "Data", true},
		{"cost field is currency", &AverageCost{TargetField: "cost_price", CostField: "unit_cost"}, "Currency", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateAll(averageTypes(tc.average, tc.lineCost))
			if got := len(errs) > 0; got != tc.wantErr {
				t.Fatalf("errors = %v, want error %v", errs, tc.wantErr)
			}
		})
	}
}

func TestQuantityAverageCostOnTheRecordReadsTheRecordsOwnCost(t *testing.T) {
	product := &DocType{Name: "Product", Fields: []Field{{Fieldname: "stock_qty", Fieldtype: "Float"}, {Fieldname: "cost_price", Fieldtype: "Float"}}}
	sale := &DocType{Name: "Purchase", QuantityEffect: &QuantityEffect{
		TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "quantity",
		AverageCost: &AverageCost{TargetField: "cost_price", CostField: "unit_cost"},
	}, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}}
	if errs := ValidateAll([]*DocType{product, sale}); len(errs) == 0 {
		t.Fatal("cost_field unit_cost missing on the record was accepted")
	}
}
