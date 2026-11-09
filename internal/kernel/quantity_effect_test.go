package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func effectRegistry() *doctype.Registry {
	product := &doctype.DocType{Name: "Product", Fields: []doctype.Field{
		{Fieldname: "product_name", Fieldtype: "Data"},
		{Fieldname: "stock_qty", Fieldtype: "Float"},
	}}
	adjustment := &doctype.DocType{Name: "Stock Adjustment", AppendOnly: true,
		QuantityEffect: &doctype.QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta"},
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "delta", Fieldtype: "Float"},
		}}
	line := &doctype.DocType{Name: "Receipt Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}}
	receipt := &doctype.DocType{Name: "Receipt",
		QuantityEffect: &doctype.QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items", SourceRecordField: "product", DeltaField: "quantity"},
		Fields:         []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"}}}
	returns := &doctype.DocType{Name: "Issue",
		QuantityEffect: &doctype.QuantityEffect{TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items", SourceRecordField: "product", DeltaField: "quantity", Direction: "decrease"},
		Fields:         []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"}}}
	plain := &doctype.DocType{Name: "Note", Fields: []doctype.Field{{Fieldname: "text", Fieldtype: "Data"}}}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{product, adjustment, line, receipt, returns, plain}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	return reg
}

func createOp(t *testing.T, doctypeName string, data map[string]any) Operation {
	t.Helper()
	body, _ := json.Marshal(data)
	payload, _ := json.Marshal(RecordCreatePayload{Doctype: doctypeName, Data: body})
	return Operation{Command: CommandRecordCreate, Payload: payload}
}

func bundleOf(t *testing.T, op Operation) RecordMutationBundlePayload {
	t.Helper()
	if op.Command != CommandRecordMutateBundle {
		t.Fatalf("command = %q, want a bundle", op.Command)
	}
	var bundle RecordMutationBundlePayload
	if err := json.Unmarshal(op.Payload, &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func amountOf(t *testing.T, item RecordMutationBundleItem, field string) float64 {
	t.Helper()
	var data map[string]float64
	if err := json.Unmarshal(item.Data, &data); err != nil || len(data) != 1 {
		t.Fatalf("effect data = %s", item.Data)
	}
	return data[field]
}

func TestCreateWithAnEffectBecomesCreatePlusStockChange(t *testing.T) {
	op := createOp(t, "Stock Adjustment", map[string]any{"product": "PROD-1", "delta": 5})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	bundle := bundleOf(t, op)
	if bundle.Doctype != "Stock Adjustment" || len(bundle.Records) != 2 {
		t.Fatalf("bundle = %#v", bundle)
	}
	first, second := bundle.Records[0], bundle.Records[1]
	if first.Operation != "" && first.Operation != "create" || first.Doctype != "Stock Adjustment" {
		t.Fatalf("first item must be the original create: %#v", first)
	}
	if second.Operation != "adjust" || second.Doctype != "Product" || second.Name != "PROD-1" || amountOf(t, second, "stock_qty") != 5 {
		t.Fatalf("second item must add 5 to PROD-1: %#v", second)
	}
	if !op.Context.isEffectItem(second.Key) || op.Context.isEffectItem(first.Key) {
		t.Fatalf("only the stock change may be marked as an engine-applied effect")
	}
}

func TestNegativeDeltaTakesStockOut(t *testing.T) {
	op := createOp(t, "Stock Adjustment", map[string]any{"product": "PROD-1", "delta": -3})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if got := amountOf(t, bundleOf(t, op).Records[1], "stock_qty"); got != -3 {
		t.Fatalf("change = %v, want -3", got)
	}
}

func TestLinesAreSummedPerProductAndOrderedByProduct(t *testing.T) {
	op := createOp(t, "Receipt", map[string]any{"items": []map[string]any{
		{"product": "SODA", "quantity": 24}, {"product": "WATER", "quantity": 12}, {"product": "SODA", "quantity": 6},
	}})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	records := bundleOf(t, op).Records
	if len(records) != 3 {
		t.Fatalf("want the receipt plus two stock changes, got %d items", len(records))
	}
	if records[1].Name != "SODA" || amountOf(t, records[1], "stock_qty") != 30 || records[2].Name != "WATER" || amountOf(t, records[2], "stock_qty") != 12 {
		t.Fatalf("changes = %#v", records[1:])
	}
}

func TestDecreaseDirectionTakesLineQuantitiesOut(t *testing.T) {
	op := createOp(t, "Issue", map[string]any{"items": []map[string]any{{"product": "SODA", "quantity": 4}}})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if got := amountOf(t, bundleOf(t, op).Records[1], "stock_qty"); got != -4 {
		t.Fatalf("change = %v, want -4", got)
	}
}

func TestRecordsWithoutAnEffectAreLeftAlone(t *testing.T) {
	op := createOp(t, "Note", map[string]any{"text": "hi"})
	before := string(op.Payload)
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if op.Command != CommandRecordCreate || string(op.Payload) != before {
		t.Fatalf("a record with no effect was rewritten: %s %s", op.Command, op.Payload)
	}
}

func TestOnlyCreatesAreRewritten(t *testing.T) {
	op := Operation{Command: CommandRecordUpdate, Payload: json.RawMessage(`{"doctype":"Stock Adjustment","name":"X","data":{"delta":1}}`)}
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil || op.Command != CommandRecordUpdate {
		t.Fatalf("an update was rewritten: %v %s", err, op.Command)
	}
}

func TestLinesThatAllNetToNothingAreJustACreate(t *testing.T) {
	op := createOp(t, "Receipt", map[string]any{"items": []map[string]any{{"product": "SODA", "quantity": 0}}})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil || op.Command != CommandRecordCreate {
		t.Fatalf("err=%v command=%s, want a plain create", err, op.Command)
	}
}

func TestBadAmountsAreRejectedBeforeAnythingIsSaved(t *testing.T) {
	cases := map[string]map[string]any{
		"missing product":     {"delta": 5},
		"missing amount":      {"product": "PROD-1"},
		"zero amount":         {"product": "PROD-1", "delta": 0},
		"text amount":         {"product": "PROD-1", "delta": "lots"},
		"amount not a number": {"product": "PROD-1", "delta": map[string]any{"x": 1}},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			op := createOp(t, "Stock Adjustment", data)
			err := prepareQuantityEffect(effectRegistry(), &op)
			if err == nil || err.Type != contract.CodeValidationFailed {
				t.Fatalf("err = %v, want a validation failure", err)
			}
		})
	}
}

func TestAnAmountSentAsTextThatIsANumberIsAccepted(t *testing.T) {
	op := createOp(t, "Stock Adjustment", map[string]any{"product": "PROD-1", "delta": "2.5"})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if got := amountOf(t, bundleOf(t, op).Records[1], "stock_qty"); got != 2.5 {
		t.Fatalf("change = %v, want 2.5", got)
	}
}

func TestTooManyDifferentProductsIsRefused(t *testing.T) {
	var lines []map[string]any
	for i := 0; i < MaxMutationBundleRecords; i++ {
		lines = append(lines, map[string]any{"product": "P" + string(rune('A'+i%26)) + string(rune('a'+i/26)), "quantity": 1})
	}
	op := createOp(t, "Receipt", map[string]any{"items": lines})
	if err := prepareQuantityEffect(effectRegistry(), &op); err == nil {
		t.Fatalf("a receipt that would exceed the bundle size limit was accepted")
	}
}

// A client-sent bundle may use any key it likes, but only the kernel's own
// rewrite can mark items as engine-applied effects. A forged "effect_0" key must
// not skip the write-permission check.
func TestAClientBundleCannotClaimTheEffectExemption(t *testing.T) {
	payload, _ := json.Marshal(RecordMutationBundlePayload{Doctype: "Stock Adjustment", Records: []RecordMutationBundleItem{
		{Key: "record", Doctype: "Stock Adjustment", Data: json.RawMessage(`{"product":"P","delta":1}`)},
		{Key: "effect_0", Operation: "adjust", Doctype: "Product", Name: "P", Data: json.RawMessage(`{"stock_qty":1000000}`)},
	}})
	op := Operation{Command: CommandRecordMutateBundle, Payload: payload}
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if op.Context.isEffectItem("effect_0") || op.Context.isEffectItem("record") {
		t.Fatalf("a client-supplied bundle was granted the engine-applied exemption")
	}
}

// The reason chosen on the record decides whether an adjustment adds or removes
// stock. The person types a plain amount; the record keeps the signed number.
func reasonRegistry() *doctype.Registry {
	product := &doctype.DocType{Name: "Product", Fields: []doctype.Field{{Fieldname: "stock_qty", Fieldtype: "Float"}}}
	adjustment := &doctype.DocType{Name: "Stock Adjustment", AppendOnly: true,
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta", ReasonField: "reason",
			ReasonDirections: map[string]string{"Damaged": "decrease", "Lost or stolen": "decrease", "Found": "increase", "Opening balance": "increase"},
		},
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "delta", Fieldtype: "Float"},
			{Fieldname: "reason", Fieldtype: "Select", Options: "Opening balance\nCount correction\nDamaged\nLost or stolen\nFound\nOther"},
		}}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{product, adjustment}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	return reg
}

func TestReasonDecidesWhetherAnAdjustmentAddsOrRemoves(t *testing.T) {
	cases := []struct {
		reason string
		typed  float64
		stock  float64 // change applied to the product
		stored float64 // number kept on the adjustment record
	}{
		{"Damaged", 5, -5, -5},
		{"Lost or stolen", 2, -2, -2},
		{"Found", 3, 3, 3},
		{"Opening balance", 50, 50, 50},
		{"Damaged", -5, -5, -5},          // a minus sign does not turn a loss into a gain
		{"Found", -3, 3, 3},              // nor does it turn a find into a loss
		{"Count correction", -4, -4, -4}, // no fixed direction: as typed
		{"Count correction", 4, 4, 4},
		{"Other", -1, -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			op := createOp(t, "Stock Adjustment", map[string]any{"product": "P1", "delta": tc.typed, "reason": tc.reason})
			if err := prepareQuantityEffect(reasonRegistry(), &op); err != nil {
				t.Fatal(err)
			}
			bundle := bundleOf(t, op)
			if got := amountOf(t, bundle.Records[1], "stock_qty"); got != tc.stock {
				t.Fatalf("stock change = %v, want %v", got, tc.stock)
			}
			var stored map[string]any
			if err := json.Unmarshal(bundle.Records[0].Data, &stored); err != nil {
				t.Fatal(err)
			}
			if stored["delta"] != tc.stored {
				t.Fatalf("number kept on the record = %v, want %v", stored["delta"], tc.stored)
			}
		})
	}
}

func TestReasonWithoutDirectionsKeepsTheTypedSign(t *testing.T) {
	op := createOp(t, "Stock Adjustment", map[string]any{"product": "P1", "delta": -5.0, "reason": "Damaged"})
	if err := prepareQuantityEffect(effectRegistry(), &op); err != nil {
		t.Fatal(err)
	}
	if got := amountOf(t, bundleOf(t, op).Records[1], "stock_qty"); got != -5 {
		t.Fatalf("stock change = %v, want -5", got)
	}
}
