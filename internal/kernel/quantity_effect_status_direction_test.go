package kernel

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// returnSale is statusSale with a Return status that applies in the opposite
// direction: a return puts the quantity back.
func returnSale() *doctype.DocType {
	sale := statusSale("Draft")
	sale.QuantityEffect.ApplyInStatuses = append(sale.QuantityEffect.ApplyInStatuses, "Return")
	sale.QuantityEffect.StatusDirections = map[string]string{"Return": "increase"}
	sale.Fields[0].Options += "\nReturn"
	return sale
}

func TestStatusDirectionSignsTheStoredTotals(t *testing.T) {
	effect := returnSale().QuantityEffect
	for _, tc := range []struct {
		status string
		want   float64
	}{{"Paid", -3}, {"Return", 3}} {
		doc := doctype.NewDocument("Sale")
		doc.Set("status", tc.status)
		line := doctype.NewDocument("Sale Item")
		line.Set("product", "SODA")
		line.Set("quantity", 3)
		doc.SetTable("items", []*doctype.Document{line})
		if got := effectTotalsOf(effect, doc)["SODA"]; got != tc.want {
			t.Fatalf("%s totals = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestStatusDirectionSignsACreate(t *testing.T) {
	reg := statusRegistry(t, returnSale())
	for _, tc := range []struct {
		status string
		want   float64
	}{{"Paid", -3}, {"Return", 3}} {
		op := createOp(t, "Sale", map[string]any{"status": tc.status, "items": []map[string]any{{"product": "SODA", "quantity": 3}}})
		if cerr := prepareQuantityEffect(reg, &op); cerr != nil {
			t.Fatalf("prepareQuantityEffect: %v", cerr)
		}
		bundle := bundleOf(t, op)
		if got := amountOf(t, bundle.Records[1], "stock_qty"); got != tc.want {
			t.Fatalf("%s create moves %v, want %v", tc.status, got, tc.want)
		}
	}
}
