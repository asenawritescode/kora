package doctype

import (
	"strings"
	"testing"
)

// stockMovementType is an append-only ledger record type: one row per move of a
// target's quantity.
func stockMovementType(appendOnly bool) *DocType {
	return &DocType{Name: "Stock Movement", AppendOnly: appendOnly, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "change", Fieldtype: "Float"},
		{Fieldname: "balance_after", Fieldtype: "Float"},
		{Fieldname: "source_doctype", Fieldtype: "Data"},
		{Fieldname: "source_name", Fieldtype: "Data"},
		{Fieldname: "reason", Fieldtype: "Data"},
	}}
}

func goodLedger() *QuantityLedger {
	return &QuantityLedger{
		Doctype: "Stock Movement", TargetField: "product", ChangeField: "change", BalanceField: "balance_after",
		SourceDoctypeField: "source_doctype", SourceNameField: "source_name", ReasonField: "reason",
	}
}

// ledgerTypes is the stock fixture with the header effect writing to a ledger.
func ledgerTypes(ledger *QuantityLedger, movement *DocType) []*DocType {
	header := goodHeaderEffect()
	header.Ledger = ledger
	return append(stockTypes(header, goodLinesEffect()), movement)
}

func ledgerProblemsOf(types []*DocType) string {
	var all []string
	for _, problem := range ValidateAll(types) {
		all = append(all, problem.Code+": "+problem.Message)
	}
	return strings.Join(all, "\n")
}

func TestQuantityLedgerIsCheckedAgainstTheLedgerType(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *QuantityLedger, m *DocType)
		want   string
	}{
		{"ledger record type missing", func(l *QuantityLedger, m *DocType) { l.Doctype = "Nope" }, "ledger"},
		{"ledger type is not append_only", func(l *QuantityLedger, m *DocType) { m.AppendOnly = false }, "ledger"},
		{"target field is not a link to the target", func(l *QuantityLedger, m *DocType) { m.Fields[0].Fieldtype = "Data" }, "target_field"},
		{"target field missing", func(l *QuantityLedger, m *DocType) { l.TargetField = "absent" }, "target_field"},
		{"change field missing", func(l *QuantityLedger, m *DocType) { l.ChangeField = "" }, "change_field"},
		{"change field is not a number", func(l *QuantityLedger, m *DocType) { m.Fields[1].Fieldtype = "Data" }, "change_field"},
		{"balance field missing", func(l *QuantityLedger, m *DocType) { l.BalanceField = "absent" }, "balance_field"},
		{"balance field is not a number", func(l *QuantityLedger, m *DocType) { m.Fields[2].Fieldtype = "Data" }, "balance_field"},
		{"source type field missing", func(l *QuantityLedger, m *DocType) { l.SourceDoctypeField = "absent" }, "source_doctype_field"},
		{"source name field is a number", func(l *QuantityLedger, m *DocType) { m.Fields[4].Fieldtype = "Float" }, "source_name_field"},
		{"reason field is a number", func(l *QuantityLedger, m *DocType) { m.Fields[5].Fieldtype = "Float" }, "reason_field"},
		{"reason field missing", func(l *QuantityLedger, m *DocType) { l.ReasonField = "absent" }, "reason_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, m := goodLedger(), stockMovementType(true)
			tc.mutate(l, m)
			got := ledgerProblemsOf(ledgerTypes(l, m))
			if !strings.Contains(got, "InvalidQuantityEffect") || !strings.Contains(got, tc.want) {
				t.Fatalf("expected an InvalidQuantityEffect problem mentioning %q, got:\n%s", tc.want, got)
			}
		})
	}
}

func TestQuantityLedgerGoodConfigIsAccepted(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *QuantityLedger)
	}{
		{"all fields named", func(l *QuantityLedger) {}},
		{"reason is optional", func(l *QuantityLedger) { l.ReasonField = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := goodLedger()
			tc.mutate(l)
			if got := ledgerProblemsOf(ledgerTypes(l, stockMovementType(true))); got != "" {
				t.Fatalf("a good ledger was rejected:\n%s", got)
			}
		})
	}
}

func TestQuantityLedgerParsesFromYAML(t *testing.T) {
	dt, err := ParseYAML([]byte(`
name: Receipt
module: Stock
fields:
  - fieldname: items
    fieldtype: Table
    options: Receipt Line
quantity_effect:
  target_doctype: Product
  target_quantity_field: stock_qty
  lines_field: items
  source_record_field: product
  delta_field: quantity
  direction: increase
  ledger:
    doctype: Stock Movement
    target_field: product
    change_field: change
    balance_field: balance_after
    source_doctype_field: source_doctype
    source_name_field: source_name
    reason_field: reason
`), "inline")
	if err != nil {
		t.Fatal(err)
	}
	if got := dt.QuantityEffect.Ledger; got == nil || got.Doctype != "Stock Movement" || got.BalanceField != "balance_after" || got.ReasonField != "reason" {
		t.Fatalf("ledger did not parse as expected: %#v", got)
	}
}
