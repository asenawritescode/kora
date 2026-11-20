package doctype

import (
	"strings"
	"testing"
)

// statusTypes is stockTypes with a Select status on the adjustment record, so
// the status gate can be checked against a real field.
func statusTypes(header *QuantityEffect) []*DocType {
	types := stockTypes(header, goodLinesEffect())
	for _, dt := range types {
		if dt.Name == "Stock Adjustment" {
			dt.Fields = append(dt.Fields, Field{Fieldname: "status", Fieldtype: "Select", Options: "Open\nDone"})
		}
	}
	return types
}

func statusGateProblems(t *testing.T, header *QuantityEffect) string {
	t.Helper()
	var all []string
	for _, problem := range ValidateAll(statusTypes(header)) {
		all = append(all, problem.Code+": "+problem.Message)
	}
	return strings.Join(all, "\n")
}

func TestQuantityEffectStatusGateIsChecked(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(q *QuantityEffect)
		wantErr string // "" means the gate is valid
	}{
		{"valid gate on a record that is not append-only", func(q *QuantityEffect) {
			q.StatusField, q.ApplyInStatuses = "status", []string{"Done"}
		}, ""},
		{"statuses without a status field", func(q *QuantityEffect) {
			q.ApplyInStatuses = []string{"Done"}
		}, "status_field"},
		{"status field without statuses", func(q *QuantityEffect) {
			q.StatusField = "status"
		}, "apply_in_statuses"},
		{"status field missing", func(q *QuantityEffect) {
			q.StatusField, q.ApplyInStatuses = "absent", []string{"Done"}
		}, "status_field"},
		{"status field is not a Select", func(q *QuantityEffect) {
			q.StatusField, q.ApplyInStatuses = "note", []string{"Done"}
		}, "status_field"},
		{"status is not an option of the field", func(q *QuantityEffect) {
			q.StatusField, q.ApplyInStatuses = "status", []string{"Done", "Lost"}
		}, "is not one of the options"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			header := goodHeaderEffect()
			tc.mutate(header)
			got := statusGateProblems(t, header)
			if tc.wantErr == "" {
				if got != "" {
					t.Fatalf("a valid status gate was rejected:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, "InvalidQuantityEffect") || !strings.Contains(got, tc.wantErr) {
				t.Fatalf("expected an InvalidQuantityEffect problem on %q, got:\n%s", tc.wantErr, got)
			}
		})
	}
}

func TestQuantityEffectGatedNeedsBothFieldAndStatuses(t *testing.T) {
	cases := []struct {
		name   string
		effect *QuantityEffect
		want   bool
	}{
		{"no gate", &QuantityEffect{}, false},
		{"status field only", &QuantityEffect{StatusField: "status"}, false},
		{"statuses only", &QuantityEffect{ApplyInStatuses: []string{"Paid"}}, false},
		{"both set", &QuantityEffect{StatusField: "status", ApplyInStatuses: []string{"Paid"}}, true},
		{"nil effect", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.effect.Gated(); got != tc.want {
				t.Fatalf("Gated() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuantityEffectAppliesOnlyInTheListedStatuses(t *testing.T) {
	q := &QuantityEffect{StatusField: "status", ApplyInStatuses: []string{"Awaiting Payment", "Paid"}}
	for status, want := range map[string]bool{"Draft": false, "Awaiting Payment": true, "Paid": true, "Cancelled": false, "": false} {
		if got := q.AppliesIn(status); got != want {
			t.Fatalf("AppliesIn(%q) = %v, want %v", status, got, want)
		}
	}
	var none *QuantityEffect
	if none.AppliesIn("Paid") {
		t.Fatal("a nil effect applies in no status")
	}
}

func TestSaleStatusGateParsesFromYAML(t *testing.T) {
	dt, err := ParseYAML([]byte(`
name: Sale
module: Sales
fields:
  - fieldname: status
    fieldtype: Select
    options: "Draft\nAwaiting Payment\nPartially Paid\nPaid\nRefunded\nCancelled"
  - fieldname: items
    fieldtype: Table
    options: Sale Item
quantity_effect:
  target_doctype: Product
  target_quantity_field: stock_qty
  lines_field: items
  source_record_field: product
  delta_field: quantity
  direction: decrease
  status_field: status
  apply_in_statuses: [Awaiting Payment, Partially Paid, Paid]
`), "inline")
	if err != nil {
		t.Fatal(err)
	}
	q := dt.QuantityEffect
	if !q.Gated() || q.StatusField != "status" || len(q.ApplyInStatuses) != 3 || q.Direction != "decrease" {
		t.Fatalf("status gate did not parse as configured: %#v", q)
	}
	if !q.AppliesIn("Paid") || q.AppliesIn("Draft") || q.AppliesIn("Cancelled") {
		t.Fatalf("the parsed gate applies in the wrong statuses: %#v", q.ApplyInStatuses)
	}
}
