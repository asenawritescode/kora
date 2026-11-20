package kernel

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func statusEffect() *doctype.QuantityEffect {
	return &doctype.QuantityEffect{
		TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
		SourceRecordField: "product", DeltaField: "quantity", Direction: "decrease",
		StatusField: "status", ApplyInStatuses: []string{"Awaiting Payment", "Paid"},
	}
}

func statusSale(defaultStatus string) *doctype.DocType {
	return &doctype.DocType{Name: "Sale", QuantityEffect: statusEffect(), Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nAwaiting Payment\nPaid\nCancelled", Default: defaultStatus},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}}
}

func statusRegistry(t *testing.T, sale *doctype.DocType) *doctype.Registry {
	t.Helper()
	product := &doctype.DocType{Name: "Product", Fields: []doctype.Field{{Fieldname: "stock_qty", Fieldtype: "Float"}}}
	line := &doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{product, line, sale}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	return reg
}

func TestCreateAppliesTheStatusGateOnlyInTheListedStatuses(t *testing.T) {
	sale := statusSale("Draft")
	cases := []struct {
		name    string
		data    map[string]any
		want    bool
		wantErr bool
	}{
		{"draft stays out", map[string]any{"status": "Draft"}, false, false},
		{"paid applies", map[string]any{"status": "Paid"}, true, false},
		{"awaiting payment applies", map[string]any{"status": "Awaiting Payment"}, true, false},
		{"cancelled stays out", map[string]any{"status": "Cancelled"}, false, false},
		{"omitted status takes the field default", map[string]any{}, false, false},
		{"blank status takes the field default", map[string]any{"status": ""}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cerr := createStatusApplies(sale, tc.data)
			if (cerr != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("createStatusApplies = %v, %v; want %v, error %v", got, cerr, tc.want, tc.wantErr)
			}
		})
	}
}

func TestCreateWithoutAStatusDefaultIsRefused(t *testing.T) {
	sale := statusSale("")
	if _, cerr := createStatusApplies(sale, map[string]any{}); cerr == nil {
		t.Fatal("a gated create with no status and no field default must be refused, not silently left out of stock")
	}
}

func TestUngatedEffectAlwaysAppliesOnCreate(t *testing.T) {
	sale := statusSale("Draft")
	sale.QuantityEffect.StatusField, sale.QuantityEffect.ApplyInStatuses = "", nil
	if got, cerr := createStatusApplies(sale, map[string]any{"status": "Draft"}); cerr != nil || !got {
		t.Fatalf("an effect with no status gate must apply on create: %v, %v", got, cerr)
	}
}

func TestStoredEffectTotalsReadTheLinesWithTheDirection(t *testing.T) {
	sale := statusSale("Draft")
	line := func(product string, quantity float64) *doctype.Document {
		doc := doctype.NewDocument("Sale Item")
		doc.Set("product", product)
		doc.Set("quantity", quantity)
		return doc
	}
	doc := doctype.NewDocument("Sale")
	doc.SetTable("items", []*doctype.Document{line("SODA", 3), line("SODA", 2), line("WATER", 0), line("", 4)})
	got := effectTotalsOf(sale.QuantityEffect, doc)
	want := map[string]float64{"SODA": -5}
	if !sameEffectTotals(got, want) {
		t.Fatalf("totals = %v, want %v (decrease, zero and blank lines add nothing)", got, want)
	}
}

func TestStoredEffectTotalsOfAHeaderEffectUseTheSignedAmount(t *testing.T) {
	effect := &doctype.QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta", StatusField: "status", ApplyInStatuses: []string{"Paid"}}
	doc := doctype.NewDocument("Adjustment")
	doc.Set("product", "SODA")
	doc.Set("delta", float64(-4))
	if got := effectTotalsOf(effect, doc); !sameEffectTotals(got, map[string]float64{"SODA": -4}) {
		t.Fatalf("totals = %v, want SODA -4", got)
	}
}

func TestSameEffectTotalsComparesEveryProduct(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]float64
		want bool
	}{
		{"equal", map[string]float64{"A": 1, "B": 2}, map[string]float64{"B": 2, "A": 1}, true},
		{"different amount", map[string]float64{"A": 1}, map[string]float64{"A": 2}, false},
		{"extra product", map[string]float64{"A": 1}, map[string]float64{"A": 1, "B": 1}, false},
		{"both empty", nil, map[string]float64{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameEffectTotals(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameEffectTotals = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGatedCreateIsRewrittenOnlyWhenItsStatusApplies(t *testing.T) {
	reg := statusRegistry(t, statusSale("Draft"))
	cases := []struct {
		name       string
		status     string
		wantBundle bool
	}{
		{"draft create changes no stock", "Draft", false},
		{"paid create changes stock once in a bundle", "Paid", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := createOp(t, "Sale", map[string]any{"status": tc.status, "items": []map[string]any{{"product": "SODA", "quantity": 3}}})
			if cerr := prepareQuantityEffect(reg, &op); cerr != nil {
				t.Fatalf("prepareQuantityEffect: %v", cerr)
			}
			if got := op.Command == CommandRecordMutateBundle; got != tc.wantBundle {
				t.Fatalf("bundle = %v, want %v (command %s)", got, tc.wantBundle, op.Command)
			}
			if tc.wantBundle {
				bundle := bundleOf(t, op)
				if len(bundle.Records) != 2 || amountOf(t, bundle.Records[1], "stock_qty") != -3 {
					t.Fatalf("bundle = %+v, want one adjust of -3 on SODA's stock", bundle)
				}
			}
		})
	}
}
